package main

import (
	"context"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath = "./deploy/search_ui.config.yaml"
)

type Config struct {
	Version int       `yaml:"version"`
	HTTP    HTTPConf  `yaml:"http"`
	Search  SearchCfg `yaml:"search"`
	UI      UIConf    `yaml:"ui"`
}

type HTTPConf struct {
	Addr string `yaml:"addr"`
}

type SearchCfg struct {
	PageSize       int      `yaml:"page_size"`
	SnippetWords   int      `yaml:"snippet_words"`
	HighlightStart string   `yaml:"highlight_start"`
	HighlightEnd   string   `yaml:"highlight_end"`
	Languages      []string `yaml:"languages"`
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

func main() {
	cfgPath := getenv("SEARCH_UI_CONFIG_PATH", defaultConfigPath)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("failed to load config %q: %v", cfgPath, err)
	}

	// DB DSN must come from environment (.env)
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
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"mul": func(a, b int) int { return a * b },
		// Mark snippet HTML (ts_headline with StartSel/StopSel) as safe for rendering
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
	mux.HandleFunc("/search", srv.handleSearch)
	mux.HandleFunc("/page", srv.handlePage)
	mux.HandleFunc("/view", srv.handleView)

	addr := cfg.HTTP.Addr
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("search ui listening on %s (config: %s, templates: %s)", addr, cfgPath, templatesDir)

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
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	site := strings.TrimSpace(r.URL.Query().Get("site"))
	sort := strings.TrimSpace(r.URL.Query().Get("sort"))
	if q == "" {
		// Render empty form page
		data := map[string]any{
			"Title": s.title,
			"Q":     "",
			"Site":  site,
			"Sort":  sort,
		}
		s.render(w, "index.html", data)
		return
	}
	// If q present on index, render full page with results block
	results, total, err := s.query(r.Context(), q, page, s.pageSize(), site, sort)
	if err != nil {
		http.Error(w, "search error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Title":    s.title,
		"Q":        q,
		"Results":  results,
		"Page":     page,
		"PageSize": s.pageSize(),
		"Total":    total,
		"Site":     site,
		"Sort":     sort,
	}
	s.render(w, "index.html", data)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	site := strings.TrimSpace(r.URL.Query().Get("site"))
	sort := strings.TrimSpace(r.URL.Query().Get("sort"))
	if q == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("query is required"))
		return
	}
	results, total, err := s.query(r.Context(), q, page, s.pageSize(), site, sort)
	if err != nil {
		http.Error(w, "search error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Q":        q,
		"Results":  results,
		"Page":     page,
		"PageSize": s.pageSize(),
		"Total":    total,
		"Site":     site,
		"Sort":     sort,
	}
	s.render(w, "results.html", data)
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	urlParam := strings.TrimSpace(r.URL.Query().Get("url"))
	if urlParam == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	const q = `
SELECT
  url,
  COALESCE(NULLIF(title, ''), url) AS title,
  COALESCE(description, '') AS description,
  fetched_at
FROM pages
WHERE url = $1
LIMIT 1;`
	var pv struct {
		URL         string
		Title       string
		Description string
		FetchedAt   time.Time
	}
	if err := s.db.QueryRow(r.Context(), q, urlParam).Scan(&pv.URL, &pv.Title, &pv.Description, &pv.FetchedAt); err != nil {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}
	data := map[string]any{
		"Title": s.title,
		"Page":  pv,
	}
	s.render(w, "page.html", data)
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	urlParam := strings.TrimSpace(r.URL.Query().Get("url"))
	if urlParam == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	const q = `
SELECT
  COALESCE(html, '') AS html,
  COALESCE(NULLIF(content_type, ''), 'text/html; charset=utf-8') AS content_type
FROM pages
WHERE url = $1
LIMIT 1;`
	var html string
	var contentType string
	if err := s.db.QueryRow(r.Context(), q, urlParam).Scan(&html, &contentType); err != nil {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write([]byte(html))
}

type Result struct {
	URL       string
	Title     string
	Snippet   string
	FetchedAt time.Time
}

func (s *Server) query(ctx context.Context, q string, page, pageSize int, site, sort string) ([]Result, int, error) {
	offset := (page - 1) * pageSize

	where := "(tsv_ru @@ websearch_to_tsquery('russian', $1) OR tsv_en @@ websearch_to_tsquery('english', $1))"
	args := []any{q}
	join := ""
	if strings.TrimSpace(site) != "" {
		join = "JOIN sites s ON s.id = pages.site_id"
		where += " AND s.domain = $2"
		args = append(args, site)
	}

	// Count
	countSQL := "SELECT count(*) FROM pages " + join + " WHERE " + where + ";"
	var total int
	if err := s.db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Results
	order := "ORDER BY GREATEST(rank_ru, rank_en) DESC, fetched_at DESC"
	if strings.EqualFold(sort, "fresh") {
		order = "ORDER BY fetched_at DESC"
	}

	// Build LIMIT/OFFSET placeholders depending on presence of site filter
	limitIdx := len(args) + 1
	offsetIdx := len(args) + 2

	searchSQL := `
SELECT
	 url,
	 title,
	 fetched_at,
	 rank_ru,
	 rank_en,
	 snippet_ru,
	 snippet_en
FROM (
	 SELECT
	   url,
	   COALESCE(NULLIF(title, ''), url) AS title,
	   fetched_at,
	   ts_rank_cd(COALESCE(tsv_ru, to_tsvector('russian','')), websearch_to_tsquery('russian', $1)) AS rank_ru,
	   ts_rank_cd(COALESCE(tsv_en, to_tsvector('english','')), websearch_to_tsquery('english', $1)) AS rank_en,
	   ts_headline('russian', text, websearch_to_tsquery('russian', $1), 'StartSel=<mark>,StopSel=</mark>,MaxFragments=2,MaxWords=20,MinWords=10') AS snippet_ru,
	   ts_headline('english', text, websearch_to_tsquery('english', $1), 'StartSel=<mark>,StopSel=</mark>,MaxFragments=2,MaxWords=20,MinWords=10') AS snippet_en
	 FROM pages
	 ` + join + `
	 WHERE ` + where + `
) sub
` + order + `
LIMIT $` + strconv.Itoa(limitIdx) + ` OFFSET $` + strconv.Itoa(offsetIdx) + `;`

	args = append(args, pageSize, offset)
	rows, err := s.db.Query(ctx, searchSQL, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]Result, 0, pageSize)
	for rows.Next() {
		var url, title, snippetRu, snippetEn string
		var fetchedAt time.Time
		var rankRu, rankEn float32
		if err := rows.Scan(&url, &title, &fetchedAt, &rankRu, &rankEn, &snippetRu, &snippetEn); err != nil {
			return nil, 0, err
		}
		snippet := firstNonEmpty(snippetRu, snippetEn)
		if snippet == "" {
			snippet = title
		}
		out = append(out, Result{
			URL:       url,
			Title:     title,
			Snippet:   snippet,
			FetchedAt: fetchedAt,
		})
	}
	if rows.Err() != nil {
		return nil, 0, rows.Err()
	}
	return out, total, nil
}

func (s *Server) pageSize() int {
	if s.cfg.Search.PageSize > 0 {
		return s.cfg.Search.PageSize
	}
	return 10
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

func parsePositiveInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
