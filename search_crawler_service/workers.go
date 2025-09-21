package main

import (
	"context"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// runWorkers starts background loop that takes tasks from DB and processes them.
func runWorkers(ctx context.Context, db *pgxpool.Pool, cfg Config, ppool *ProxyPool) {
	// Determine worker count: config value or default = min(NumCPU*4, 64)
	wc := cfg.Crawler.Workers
	if wc <= 0 {
		ncpu := runtime.NumCPU()
		wc = ncpu * 4
		if wc > 64 {
			wc = 64
		}
		if wc < 1 {
			wc = 1
		}
	}
	Info("starting workers", "count", wc)

	for i := 0; i < wc; i++ {
		go func(id int) {
			Info("worker started", "worker", id)
			idleSleep := 500 * time.Millisecond
			for {
				if ctx.Err() != nil {
					return
				}
				ok, err := pickAndProcessOne(ctx, db, cfg, ppool)
				if err != nil {
					Error("worker error", "worker", id, "err", err)
					time.Sleep(1 * time.Second)
					continue
				}
				if !ok {
					time.Sleep(idleSleep)
				}
			}
		}(i + 1)
	}
}

type queueItem struct {
	ID     int64
	SiteID int64
	URL    string
}

func pickAndProcessOne(ctx context.Context, db *pgxpool.Pool, cfg Config, ppool *ProxyPool) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const sel = `
SELECT id, site_id, url
FROM crawl_queue
WHERE status = 'queued'
  AND (next_try_at IS NULL OR next_try_at <= now())
ORDER BY priority DESC, id
FOR UPDATE SKIP LOCKED
LIMIT 1;`
	var it queueItem
	if err := tx.QueryRow(ctx, sel).Scan(&it.ID, &it.SiteID, &it.URL); err != nil {
		// no rows
		if strings.Contains(err.Error(), "no rows") {
			_ = tx.Rollback(ctx)
			return false, nil
		}
		return false, err
	}

	const updToProcessing = `
UPDATE crawl_queue
SET status = 'processing', attempts = attempts + 1, updated_at = now()
WHERE id = $1;`
	if _, err := tx.Exec(ctx, updToProcessing, it.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	Debug("picked queue item", "id", it.ID, "site_id", it.SiteID, "url", it.URL)

	// per-host rate limit
	host := ""
	if u, err := url.Parse(it.URL); err == nil {
		host = normalizeHost(u.Host)
	}
	lim := getHostLimiter(host, cfg.Crawler.RPSPerHost, cfg.Crawler.RPSBurst)
	if lim != nil {
		_ = lim.Wait(ctx)
	}

	// Build HTTP client with proxy (http/https only for MVP)
	proxyURL := ppool.Next()
	client := buildHTTPClient(proxyURL, cfg.Crawler.HTMLFetchTimeout.Duration)

	// Fetch
	status, ctype, html, err := fetchHTML(ctx, client, it.URL, int(cfg.Crawler.HTMLMaxSize.Bytes), cfg.Crawler.UserAgent)
	if err != nil {
		// mark error with next_try_at
		markQueueError(ctx, db, it.ID, fmt.Sprintf("fetch: %v", err), 5*time.Minute)
		return true, nil
	}
	Debug("fetched html", "status", status, "ctype", ctype, "bytes", len(html))
	// Only allow text/html
	if !isAllowedContentType(ctype, cfg.Crawler.ContentTypes) {
		markQueueError(ctx, db, it.ID, fmt.Sprintf("content-type not allowed: %s", ctype), 30*time.Minute)
		return true, nil
	}
	// Extract text (very basic for MVP)
	text := extractVisibleText(html)

	// Extract title/description (MVP)
	title := extractTitle(html)
	description := extractMetaDescription(html)
	lang := ""

	// Upsert page
	pageID, err := upsertPage(ctx, db, it.SiteID, it.URL, title, description, lang, status, ctype, html, text)
	if err != nil {
		markQueueError(ctx, db, it.ID, fmt.Sprintf("store: %v", err), 10*time.Minute)
		return true, nil
	}

	// Extract links and enqueue in-domain
	if siteDomain, err := getSiteDomain(ctx, db, it.SiteID); err == nil {
		eCount, total, _ := extractAndEnqueueLinks(ctx, db, cfg, it.SiteID, siteDomain, pageID, it.URL, html)
		Debug("links processed", "found", total, "enqueued", eCount)
	}

	markQueueDone(ctx, db, it.ID)
	return true, nil
}
