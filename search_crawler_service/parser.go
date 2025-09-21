package main

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// --- Text extraction (MVP) ---

var (
	rmScript = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	rmStyle  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	rmTags   = regexp.MustCompile(`(?is)<[^>]+>`)
	spaceSeq = regexp.MustCompile(`\s+`)
)

func extractVisibleText(html string) string {
	// Very basic cleaner for MVP: strip scripts/styles/tags, collapse spaces
	s := rmScript.ReplaceAllString(html, " ")
	s = rmStyle.ReplaceAllString(s, " ")
	s = rmTags.ReplaceAllString(s, " ")
	s = spaceSeq.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// --- Title/Description extraction (MVP) ---
var (
	reTitle    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reMetaDesc = regexp.MustCompile(`(?is)<meta\s+[^>]*name\s*=\s*["']description["'][^>]*content\s*=\s*["']([^"']+)["'][^>]*>`)
	reOGDesc   = regexp.MustCompile(`(?is)<meta\s+[^>]*property\s*=\s*["']og:description["'][^>]*content\s*=\s*["']([^"']+)["'][^>]*>`)
)

// extractTitle returns trimmed & unescaped <title> or empty string.
func extractTitle(htmlStr string) string {
	m := reTitle.FindStringSubmatch(htmlStr)
	if len(m) >= 2 {
		t := strings.TrimSpace(m[1])
		t = rmTags.ReplaceAllString(t, " ")
		t = spaceSeq.ReplaceAllString(t, " ")
		t = strings.TrimSpace(t)
		t = html.UnescapeString(t)
		if len(t) > 512 {
			t = t[:512]
		}
		return t
	}
	return ""
}

// extractMetaDescription tries common meta description tags.
func extractMetaDescription(htmlStr string) string {
	if m := reMetaDesc.FindStringSubmatch(htmlStr); len(m) >= 2 {
		s := strings.TrimSpace(m[1])
		s = html.UnescapeString(s)
		s = spaceSeq.ReplaceAllString(s, " ")
		if len(s) > 1024 {
			s = s[:1024]
		}
		return s
	}
	if m := reOGDesc.FindStringSubmatch(htmlStr); len(m) >= 2 {
		s := strings.TrimSpace(m[1])
		s = html.UnescapeString(s)
		s = spaceSeq.ReplaceAllString(s, " ")
		if len(s) > 1024 {
			s = s[:1024]
		}
		return s
	}
	return ""
}

// --- Link extraction and enqueue (MVP) ---

var reHref = regexp.MustCompile(`(?is)<a\s[^>]*href\s*=\s*["']([^"']+)["']`)

func isInDomain(host string, siteDomain string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	d := strings.ToLower(strings.TrimSpace(siteDomain))
	if h == d || strings.HasSuffix(h, "."+d) {
		return true
	}
	return false
}

func extractAndEnqueueLinks(ctx context.Context, db *pgxpool.Pool, cfg Config, siteID int64, siteDomain string, fromPageID int64, baseURL string, html string) (int, int, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return 0, 0, err
	}
	matches := reHref.FindAllStringSubmatch(html, -1)
	seen := make(map[string]struct{})
	enqueued := 0

	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		href := strings.TrimSpace(m[1])
		if href == "" {
			continue
		}
		low := strings.ToLower(href)
		if strings.HasPrefix(low, "javascript:") || strings.HasPrefix(low, "mailto:") || strings.HasPrefix(low, "tel:") || strings.HasPrefix(low, "#") {
			continue
		}

		u, err := url.Parse(href)
		if err != nil {
			continue
		}
		var abs *url.URL
		if u.IsAbs() {
			abs = u
		} else {
			abs = base.ResolveReference(u)
		}
		abs.Fragment = ""
		if abs.Scheme != "http" && abs.Scheme != "https" {
			continue
		}
		abs.Host = normalizeHost(abs.Host)
		if !isInDomain(abs.Host, siteDomain) {
			continue
		}

		final := abs.String()
		if _, ok := seen[final]; ok {
			continue
		}
		seen[final] = struct{}{}

		toHash := sha256Hex(final)
		_ = insertPageLink(ctx, db, fromPageID, final, toHash)

		if ok, err := enqueueIfNotExists(ctx, db, siteID, final, toHash, 0); err == nil && ok {
			enqueued++
		}
	}

	return enqueued, len(seen), nil
}
