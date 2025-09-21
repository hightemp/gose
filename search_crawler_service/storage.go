package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB: sites

func ensureSite(ctx context.Context, db *pgxpool.Pool, domain string, cfg Config) (int64, error) {
	var id int64
	const q = `
INSERT INTO sites (domain, enabled, rps_limit, rps_burst, depth_limit)
VALUES ($1, TRUE, $2, $3, $4)
ON CONFLICT (domain) DO UPDATE SET updated_at = now()
RETURNING id;`
	err := db.QueryRow(ctx, q, domain, cfg.Crawler.RPSPerHost, cfg.Crawler.RPSBurst, cfg.Crawler.DepthLimit).Scan(&id)
	if err != nil {
		Error("ensureSite failed", "domain", domain, "err", err)
		return 0, err
	}
	Debug("ensureSite ok", "domain", domain, "id", id)
	return id, nil
}

// DB: crawl_queue

func enqueueIfNotExists(ctx context.Context, db *pgxpool.Pool, siteID int64, url string, urlHash string, priority int) (bool, error) {
	const ins = `
INSERT INTO crawl_queue (site_id, url, url_hash, priority, status, attempts, created_at, updated_at)
SELECT $1, $2, $3, $4, 'queued'::crawl_status, 0, now(), now()
WHERE NOT EXISTS (
  SELECT 1 FROM crawl_queue
  WHERE site_id = $1 AND url_hash = $3 AND status IN ('queued','processing')
);`
	ct, err := db.Exec(ctx, ins, siteID, url, urlHash, priority)
	if err != nil {
		Error("enqueueIfNotExists failed", "site_id", siteID, "url", url, "err", err)
		return false, err
	}
	ok := ct.RowsAffected() > 0
	if ok {
		Debug("enqueueIfNotExists inserted", "site_id", siteID, "url", url)
	} else {
		Debug("enqueueIfNotExists duplicate", "site_id", siteID, "url", url)
	}
	return ok, nil
}

func markQueueError(ctx context.Context, db *pgxpool.Pool, id int64, msg string, retryAfter time.Duration) {
	const q = `
UPDATE crawl_queue
SET status = 'error',
    last_error = $2,
    next_try_at = now() + $3::interval,
    updated_at = now()
WHERE id = $1;`
	_, _ = db.Exec(ctx, q, id, msg, fmt.Sprintf("%f seconds", retryAfter.Seconds()))
	Warn("queue item marked error", "id", id, "retry_after", retryAfter.String(), "error", msg)
}

func markQueueDone(ctx context.Context, db *pgxpool.Pool, id int64) {
	const q = `
UPDATE crawl_queue
SET status = 'done', updated_at = now()
WHERE id = $1;`
	_, _ = db.Exec(ctx, q, id)
	Debug("queue item done", "id", id)
}

// DB: pages

func upsertPage(ctx context.Context, db *pgxpool.Pool, siteID int64, rawURL string, title, description, lang string, httpStatus int, contentType, html, text string) (int64, error) {
	urlHash := sha256Hex(rawURL)
	htmlHash := sha256Hex(html)
	var id int64

	const q = `
INSERT INTO pages (site_id, url, url_hash, title, description, lang, http_status, content_type, html_hash, html, fetched_at, text, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5, NULLIF($6,''), $7,$8,$9,$10,now(),$11,now(),now())
ON CONFLICT (site_id, url_hash) DO UPDATE
SET title = COALESCE(NULLIF(EXCLUDED.title, ''), pages.title),
	   description = COALESCE(NULLIF(EXCLUDED.description, ''), pages.description),
	   lang = COALESCE(NULLIF(EXCLUDED.lang, ''), pages.lang),
	   http_status = EXCLUDED.http_status,
	   content_type = EXCLUDED.content_type,
	   html_hash = EXCLUDED.html_hash,
	   html = EXCLUDED.html,
	   fetched_at = EXCLUDED.fetched_at,
	   text = EXCLUDED.text,
	   updated_at = now()
RETURNING id;`
	if err := db.QueryRow(ctx, q, siteID, rawURL, urlHash, title, description, lang, httpStatus, contentType, htmlHash, html, text).Scan(&id); err != nil {
		Error("upsertPage failed", "site_id", siteID, "url", rawURL, "err", err)
		return 0, err
	}
	Debug("upsertPage ok", "site_id", siteID, "url", rawURL, "id", id, "status", httpStatus, "ctype", contentType, "html_bytes", len(html), "text_bytes", len(text))
	return id, nil
}

// DB: site domain & page links

func getSiteDomain(ctx context.Context, db *pgxpool.Pool, siteID int64) (string, error) {
	var d string
	err := db.QueryRow(ctx, "SELECT domain FROM sites WHERE id=$1", siteID).Scan(&d)
	if err != nil {
		Error("getSiteDomain failed", "site_id", siteID, "err", err)
		return "", err
	}
	return d, nil
}

func insertPageLink(ctx context.Context, db *pgxpool.Pool, fromPageID int64, toURL string, toURLHash string) error {
	const q = `
INSERT INTO page_links (from_page_id, to_url, to_url_hash)
VALUES ($1,$2,$3)
ON CONFLICT DO NOTHING;`
	_, err := db.Exec(ctx, q, fromPageID, toURL, toURLHash)
	if err != nil {
		Debug("insertPageLink failed", "from_page_id", fromPageID, "to_url", toURL, "err", err)
		return err
	}
	return nil
}
