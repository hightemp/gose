package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// buildHTTPClient builds HTTP client with optional proxy and sane defaults.
func buildHTTPClient(p *url.URL, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// Limits/timeouts (simple defaults)
		MaxIdleConns:          64,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    false,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	// If explicit proxy provided and supported scheme
	if p != nil {
		switch p.Scheme {
		case "http", "https":
			tr.Proxy = func(_ *http.Request) (*url.URL, error) { return p, nil }
		default:
			// socks5 not supported in MVP client; fall back to no proxy
			Warn("proxy scheme not supported, using direct", "scheme", p.Scheme)
			tr.Proxy = nil
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
}

// fetchHTML performs a GET and returns status, content-type, and body (limited by maxBytes).
func fetchHTML(ctx context.Context, client *http.Client, target string, maxBytes int, userAgent string) (status int, contentType string, html string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", "", err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()

	status = resp.StatusCode
	if status < 200 || status >= 400 {
		return status, resp.Header.Get("Content-Type"), "", fmt.Errorf("http status %d", status)
	}
	contentType = resp.Header.Get("Content-Type")
	// limit body
	lim := io.LimitedReader{R: resp.Body, N: int64(maxBytes)}
	buf, err := io.ReadAll(&lim)
	if err != nil {
		return status, contentType, "", err
	}
	// if truncated (N==0 and more data), we treat as ok since size limit reached
	return status, contentType, string(buf), nil
}

// isAllowedContentType checks whether ctype belongs to allowed list (prefix match).
func isAllowedContentType(ctype string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	// normalize
	ct := strings.ToLower(strings.TrimSpace(ctype))
	for _, a := range allow {
		if strings.HasPrefix(ct, strings.ToLower(strings.TrimSpace(a))) {
			return true
		}
	}
	return false
}
