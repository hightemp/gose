package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	// Load service config (YAML)
	cfgPath := getenv("CRAWLER_CONFIG_PATH", defaultConfigPath)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		Error("failed to load config", "path", cfgPath, "err", err)
		os.Exit(1)
	}
	// Allow DB DSN override from environment (.env) as requested
	if dsn := os.Getenv("PG_DSN"); dsn != "" {
		cfg.Postgres.DSN = dsn
	}

	// DB
	ctx := context.Background()
	db, err := pgxpool.New(ctx, cfg.Postgres.DSN)
	if err != nil {
		Error("failed to init pgx pool", "dsn", cfg.Postgres.DSN, "err", err)
		os.Exit(1)
	}
	defer db.Close()

	// Load proxies config
	proxiesPath := cfg.Proxies.ConfigPath
	if proxiesPath == "" {
		proxiesPath = "./deploy/proxies.yaml"
	}
	// Resolve relative to config directory if needed
	if !filepath.IsAbs(proxiesPath) {
		if _, err := os.Stat(proxiesPath); errors.Is(err, os.ErrNotExist) {
			base := filepath.Dir(cfgPath)
			pp := filepath.Join(base, proxiesPath)
			if _, err2 := os.Stat(pp); err2 == nil {
				proxiesPath = pp
			}
		}
	}
	pcfg, err := loadProxies(proxiesPath)
	if err != nil {
		Error("failed to load proxies", "path", proxiesPath, "err", err)
		os.Exit(1)
	}
	pool, err := NewProxyPool(pcfg)
	if err != nil {
		Error("failed to init proxy pool", "err", err)
		os.Exit(1)
	}

	// HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Check DB connectivity quickly
		dbOK := "ok"
		if err := db.Ping(r.Context()); err != nil {
			dbOK = "error: " + err.Error()
		}
		type resp struct {
			Status      string    `json:"status"`
			Uptime      string    `json:"uptime"`
			Now         time.Time `json:"now"`
			Proxies     int       `json:"proxies"`
			HTTPAddr    string    `json:"http_addr"`
			PostgresDSN string    `json:"postgres_dsn"`
			DB          string    `json:"db"`
			Whitelist   []string  `json:"whitelist_domains"`
			DepthLimit  int       `json:"depth_limit"`
			RPSPerHost  int       `json:"rps_per_host"`
			RPSBurst    int       `json:"rps_burst"`
		}
		out := resp{
			Status:      "ok",
			Uptime:      time.Since(startedAt).String(),
			Now:         time.Now().UTC(),
			Proxies:     pool.Len(),
			HTTPAddr:    cfg.HTTP.Addr,
			PostgresDSN: cfg.Postgres.DSN,
			DB:          dbOK,
			Whitelist:   cfg.Crawler.WhitelistDomains,
			DepthLimit:  cfg.Crawler.DepthLimit,
			RPSPerHost:  cfg.Crawler.RPSPerHost,
			RPSBurst:    cfg.Crawler.RPSBurst,
		}
		writeJSON(w, http.StatusOK, out)
	})

	// API: enqueue URL into crawl_queue
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req EnqueueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		u := strings.TrimSpace(req.URL)
		if u == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		parsed, err := url.Parse(u)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			http.Error(w, "invalid url", http.StatusBadRequest)
			return
		}
		// Sanitize: drop fragment
		parsed.Fragment = ""
		// Normalize host: lower-case, strip default ports, strip trailing dot
		host := normalizeHost(parsed.Host)
		if host == "" {
			http.Error(w, "invalid host", http.StatusBadRequest)
			return
		}
		parsed.Host = host
		// Optional: enforce whitelist if provided
		if len(cfg.Crawler.WhitelistDomains) > 0 && !isHostAllowed(host, cfg.Crawler.WhitelistDomains) {
			http.Error(w, "host not in whitelist", http.StatusForbidden)
			return
		}
		// Ensure site exists
		siteID, err := ensureSite(r.Context(), db, host, cfg)
		if err != nil {
			http.Error(w, "ensure site error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Enqueue if not already queued/processing
		finalURL := parsed.String()
		urlHash := sha256Hex(finalURL)
		priority := 0
		if req.Priority != nil {
			priority = *req.Priority
		}
		enq, err := enqueueIfNotExists(r.Context(), db, siteID, finalURL, urlHash, priority)
		if err != nil {
			http.Error(w, "enqueue error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		resp := EnqueueResponse{
			Enqueued: enq,
			SiteID:   siteID,
			URL:      finalURL,
			URLHash:  urlHash,
		}
		if !enq {
			resp.Message = "duplicate (already queued or processing)"
		}
		writeJSON(w, http.StatusOK, resp)
	})

	addr := cfg.HTTP.Addr
	if addr == "" {
		addr = ":8082"
	}
	Info("crawler service starting",
		"addr", addr,
		"config_path", cfgPath,
		"proxies_path", proxiesPath,
		"proxy_pool_size", pool.Len())

	// Start background workers for crawling
	go runWorkers(ctx, db, cfg, pool)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		Error("http server error", "err", err)
		os.Exit(1)
	}
}
