package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath = "./deploy/manager_ui.config.yaml"
)

type Config struct {
	Version int      `yaml:"version"`
	HTTP    HTTPConf `yaml:"http"`
	UI      UIConf   `yaml:"ui"`
}

type HTTPConf struct {
	Addr string `yaml:"addr"`
}

type UIConf struct {
	Title        string `yaml:"title"`
	TemplatesDir string `yaml:"templates_dir"`
}

type Server struct {
	cfg   Config
	db    *pgxpool.Pool
	tmpl  *template.Template
	title string
}

type Stats struct {
	PagesTotal      int64 `json:"pages_total"`
	QueueTotal      int64 `json:"queue_total"`
	QueueQueued     int64 `json:"queue_queued"`
	QueueProcessing int64 `json:"queue_processing"`
	QueueDone       int64 `json:"queue_done"`
	QueueError      int64 `json:"queue_error"`

	IndexedPercent float64 `json:"indexed_percent"`

	DBSizeBytes  int64  `json:"db_size_bytes"`
	DBSizePretty string `json:"db_size_pretty"`

	// Indexing time metrics
	IndexingStartedAt          time.Time `json:"indexing_started_at"`
	IndexingElapsedSeconds     int64     `json:"indexing_elapsed_seconds"`
	IndexingElapsedPretty      string    `json:"indexing_elapsed_pretty"`
	ETASeconds                 int64     `json:"eta_seconds"`
	ETAPretty                  string    `json:"eta_pretty"`
	EstimatedFinalDBSizeBytes  int64     `json:"estimated_final_db_size_bytes"`
	EstimatedFinalDBSizePretty string    `json:"estimated_final_db_size_pretty"`

	GeneratedAt time.Time `json:"generated_at"`
}

func main() {
	cfgPath := getenv("MANAGER_UI_CONFIG_PATH", defaultConfigPath)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("failed to load config %q: %v", cfgPath, err)
	}

	// DB DSN from env (.env / Compose)
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		log.Fatalf("PG_DSN is required in environment (.env)")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer pool.Close()

	templatesDir := cfg.UI.TemplatesDir
	if templatesDir == "" {
		templatesDir = "./templates"
	}
	funcs := template.FuncMap{
		"raw": func(s string) template.HTML { return template.HTML(s) },
	}
	tmpl, err := template.New("base").Funcs(funcs).ParseGlob(filepath.Join(templatesDir, "*.html"))
	if err != nil {
		log.Fatalf("failed to parse templates: %v", err)
	}

	srv := &Server{
		cfg:   cfg,
		db:    pool,
		tmpl:  tmpl,
		title: cfg.UI.Title,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/metrics", srv.handleMetrics)

	addr := cfg.HTTP.Addr
	if addr == "" {
		addr = ":8081"
	}
	log.Printf("manager ui listening on %s (config: %s, templates: %s)", addr, cfgPath, templatesDir)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server error: %v", err)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	stats, err := s.collectStats(r.Context())
	if err != nil {
		http.Error(w, "stats error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Title": s.title,
		"Stats": stats,
	}
	s.render(w, "index.html", data)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	stats, err := s.collectStats(r.Context())
	if err != nil {
		http.Error(w, "stats error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(stats)
}

func (s *Server) collectStats(ctx context.Context) (Stats, error) {
	var st Stats

	// pages total
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM pages;").Scan(&st.PagesTotal); err != nil {
		return Stats{}, err
	}

	// queue totals by status
	const qQueue = `
SELECT
	 count(*) AS total,
	 count(*) FILTER (WHERE status = 'queued') AS queued,
	 count(*) FILTER (WHERE status = 'processing') AS processing,
	 count(*) FILTER (WHERE status = 'done') AS done,
	 count(*) FILTER (WHERE status = 'error') AS error,
	 min(created_at) AS first_enqueued,
	 min(updated_at) FILTER (WHERE status IN ('processing','done')) AS first_started
FROM crawl_queue;`
	var firstEnqueued pgtype.Timestamptz
	var firstStarted pgtype.Timestamptz
	if err := s.db.QueryRow(ctx, qQueue).Scan(&st.QueueTotal, &st.QueueQueued, &st.QueueProcessing, &st.QueueDone, &st.QueueError, &firstEnqueued, &firstStarted); err != nil {
		return Stats{}, err
	}
	// prefer the actual start moment (first item left 'queued'), fallback to first enqueue
	if firstStarted.Valid {
		st.IndexingStartedAt = firstStarted.Time
	} else if firstEnqueued.Valid {
		st.IndexingStartedAt = firstEnqueued.Time
	}

	// db size in bytes (whole current database)
	if err := s.db.QueryRow(ctx, "SELECT pg_database_size(current_database())::bigint;").Scan(&st.DBSizeBytes); err != nil {
		return Stats{}, err
	}
	st.DBSizePretty = formatBytes(st.DBSizeBytes)

	// indexed percent: done / total (0 if no queue)
	if st.QueueTotal > 0 {
		st.IndexedPercent = math.Round((float64(st.QueueDone)/float64(st.QueueTotal))*1000) / 10.0 // one decimal
	} else {
		st.IndexedPercent = 0
	}

	// defaults for pretty fields
	st.IndexingElapsedPretty = "-"
	st.ETAPretty = "-"
	st.EstimatedFinalDBSizePretty = "-"

	// compute elapsed and eta using the earliest start moment
	now := time.Now()
	if !st.IndexingStartedAt.IsZero() {
		elapsed := now.Sub(st.IndexingStartedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		st.IndexingElapsedSeconds = int64(elapsed.Seconds())

		// format duration as "d hh:mm:ss" (days optional)
		formatDur := func(d time.Duration) string {
			if d < 0 {
				d = -d
			}
			secs := int64(d.Seconds())
			days := secs / (24 * 3600)
			secs = secs % (24 * 3600)
			h := secs / 3600
			secs = secs % 3600
			m := secs / 60
			s := secs % 60
			if days > 0 {
				return sprintf("%dd %02d:%02d:%02d", days, h, m, s)
			}
			return sprintf("%02d:%02d:%02d", h, m, s)
		}
		st.IndexingElapsedPretty = formatDur(elapsed)

		// ETA based on throughput of done items since start
		if st.QueueTotal > 0 && st.QueueDone > 0 && st.IndexingElapsedSeconds > 0 {
			rate := float64(st.QueueDone) / float64(st.IndexingElapsedSeconds) // items per second
			if rate > 0 {
				remain := st.QueueTotal - st.QueueDone
				etaSec := float64(remain) / rate
				if etaSec < 0 {
					etaSec = 0
				}
				st.ETASeconds = int64(math.Round(etaSec))
				st.ETAPretty = formatDur(time.Duration(st.ETASeconds) * time.Second)
			}
		}
	}

	// Estimate final DB size assuming near-linear growth with progress
	if st.QueueTotal > 0 && st.QueueDone > 0 {
		progress := float64(st.QueueDone) / float64(st.QueueTotal)
		if progress > 0 {
			est := int64(float64(st.DBSizeBytes) / progress)
			if est < st.DBSizeBytes {
				est = st.DBSizeBytes
			}
			st.EstimatedFinalDBSizeBytes = est
			st.EstimatedFinalDBSizePretty = formatBytes(est)
		}
	}

	st.GeneratedAt = now
	return st, nil
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func formatBytes(b int64) string {
	if b < 1024 {
		return "0.00 MB"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	size := float64(b)
	idx := 0
	for size >= 1024 && idx < len(units)-1 {
		size /= 1024
		idx++
	}
	// show GB if GB or higher, otherwise MB
	if units[idx] == "GB" || units[idx] == "TB" || units[idx] == "PB" {
		return sprintf("%.2f GB", toGB(b))
	}
	return sprintf("%.2f MB", toMB(b))
}

func toMB(b int64) float64 {
	return float64(b) / 1024.0 / 1024.0
}

func toGB(b int64) float64 {
	return float64(b) / 1024.0 / 1024.0 / 1024.0
}

// sprintf is a tiny wrapper to avoid bringing fmt to hot path in templates
func sprintf(format string, a ...any) string {
	return template.HTMLEscapeString(fmtSprintf(format, a...))
}

// fmtSprintf avoids template escaping double-escaping issues for plain strings
func fmtSprintf(format string, a ...any) string {
	// lightweight inline fmt.Sprintf to keep imports minimal is unnecessary;
	// use standard fmt
	return _sprintf(format, a...)
}

// alias to fmt.Sprintf (kept separate to avoid mistaken HTML escaping)
func _sprintf(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}
