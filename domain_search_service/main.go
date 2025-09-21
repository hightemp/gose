package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

const defaultConfigPath = "./deploy/domain_search.config.yaml"

type Config struct {
	Version   int             `yaml:"version"`
	Generator GeneratorConfig `yaml:"generator"`
	Limits    LimitsConfig    `yaml:"limits"`
	HTTPCheck HTTPCheckConfig `yaml:"http_check"`
	Run       RunConfig       `yaml:"run"`
}

type GeneratorConfig struct {
	TLDs                 []string `yaml:"tlds"`
	MinLength            int      `yaml:"min_length"`
	MaxLength            int      `yaml:"max_length"`
	Alphabet             string   `yaml:"alphabet"`
	AllowHyphen          bool     `yaml:"allow_hyphen"`
	ForbidLeadingHyphen  bool     `yaml:"forbid_leading_hyphen"`
	ForbidTrailingHyphen bool     `yaml:"forbid_trailing_hyphen"`
	ForbidDoubleHyphen   bool     `yaml:"forbid_double_hyphen"`
}

type LimitsConfig struct {
	Concurrency   int `yaml:"concurrency"`
	RatePerSecond int `yaml:"rate_per_second"`
	MaxCandidates int `yaml:"max_candidates"`
}

type HTTPCheckConfig struct {
	Timeout         Duration `yaml:"timeout"`
	Retry           int      `yaml:"retry"`
	Method          string   `yaml:"method"` // GET recommended
	BodyLimit       ByteSize `yaml:"body_limit"`
	AcceptStatusMin int      `yaml:"accept_status_min"`
	AcceptStatusMax int      `yaml:"accept_status_max"`
	TryHTTPSFirst   bool     `yaml:"try_https_first"`
}

type RunConfig struct {
	Loop bool `yaml:"loop"`
}

// Duration wrapper for YAML
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	du, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = du
	return nil
}

// ByteSize wrapper for YAML values like 32KB, 2MB
type ByteSize struct{ Bytes int64 }

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	ss := strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(ss, "KB"):
		mult = 1024
		ss = strings.TrimSuffix(ss, "KB")
	case strings.HasSuffix(ss, "MB"):
		mult = 1024 * 1024
		ss = strings.TrimSuffix(ss, "MB")
	case strings.HasSuffix(ss, "GB"):
		mult = 1024 * 1024 * 1024
		ss = strings.TrimSuffix(ss, "GB")
	case strings.HasSuffix(ss, "B"):
		mult = 1
		ss = strings.TrimSuffix(ss, "B")
	default:
		// raw number of bytes
	}
	val := strings.TrimSpace(ss)
	var nBytes int64
	_, err := fmt.Sscan(val, &nBytes)
	if err != nil {
		return fmt.Errorf("invalid byte size %q: %w", s, err)
	}
	b.Bytes = nBytes * mult
	return nil
}

func main() {
	cfgPath := getenv("DOMAIN_SEARCH_CONFIG_PATH", defaultConfigPath)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("config load error: %v", err)
	}

	if err := validateConfig(cfg); err != nil {
		log.Fatalf("config validation error: %v", err)
	}

	// DB DSN comes from environment (.env), consistent with other services
	dsn := os.Getenv("PG_DSN")
	if strings.TrimSpace(dsn) == "" {
		log.Fatalf("PG_DSN is required in environment")
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("pgx pool error: %v", err)
	}
	defer db.Close()

	log.Printf("domain_search_service started (config: %s), RPS=%d, Concurrency=%d, Loop=%v",
		cfgPath, cfg.Limits.RatePerSecond, cfg.Limits.Concurrency, cfg.Run.Loop)

	httpClient := &http.Client{
		Timeout: cfg.HTTPCheck.Timeout.Duration,
		Transport: &http.Transport{
			MaxIdleConns:        1000,
			MaxConnsPerHost:     0, // unlimited but governed by our limiter
			MaxIdleConnsPerHost: int(math.Max(2, float64(cfg.Limits.Concurrency/10))),
			DisableCompression:  false,
			Proxy:               nil,
			DialContext: (&net.Dialer{
				Timeout:   cfg.HTTPCheck.Timeout.Duration,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: cfg.HTTPCheck.Timeout.Duration,
			ForceAttemptHTTP2:   true,
		},
	}

	for {
		if err := runOnce(ctx, db, httpClient, cfg); err != nil {
			log.Printf("runOnce error: %v", err)
		}
		if !cfg.Run.Loop {
			break
		}
	}
	log.Printf("domain_search_service finished")
}

func runOnce(ctx context.Context, db *pgxpool.Pool, httpClient *http.Client, cfg Config) error {
	candidates := make(chan string, cfg.Limits.Concurrency*2)
	wg := &sync.WaitGroup{}

	// Rate limiter: token channel refilled every second
	rlTokens := make(chan struct{}, cfg.Limits.RatePerSecond)
	fillTokens := func() {
		for i := 0; i < cfg.Limits.RatePerSecond; i++ {
			select {
			case rlTokens <- struct{}{}:
			default:
				// channel full
				return
			}
		}
	}
	fillTokens()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			fillTokens()
		}
	}()

	// Workers
	worker := func() {
		defer wg.Done()
		for name := range candidates {
			// Rate limit
			select {
			case <-rlTokens:
			case <-ctx.Done():
				return
			}

			// Build URL to check: try https, then http if configured
			ok, finalURL := checkDomain(ctx, httpClient, name, cfg.HTTPCheck)
			if !ok {
				continue
			}
			// Insert to DB: ensure site + enqueue "/" URL
			host := strings.TrimPrefix(strings.TrimPrefix(finalURL, "https://"), "http://")
			if i := strings.IndexByte(host, '/'); i >= 0 {
				host = host[:i]
			}
			siteID, err := ensureSite(ctx, db, host, cfg)
			if err != nil {
				log.Printf("ensureSite(%s) error: %v", host, err)
				continue
			}
			rootURL := finalURL // already has scheme and trailing slash
			if !strings.HasSuffix(rootURL, "/") {
				rootURL += "/"
			}
			urlHash := sha256Hex(rootURL)
			enq, err := enqueueIfNotExists(ctx, db, siteID, rootURL, urlHash, 0)
			if err != nil {
				log.Printf("enqueue error %s: %v", rootURL, err)
				continue
			}
			if enq {
				log.Printf("enqueued %s (site=%d)", rootURL, siteID)
			}
		}
	}

	// Start workers
	nw := cfg.Limits.Concurrency
	if nw <= 0 {
		nw = 1
	}
	wg.Add(nw)
	for i := 0; i < nw; i++ {
		go worker()
	}

	// Generate candidates
	total := 0
	genErr := generateCandidates(cfg.Generator, func(name string) bool {
		select {
		case candidates <- name:
			total++
			return cfg.Limits.MaxCandidates <= 0 || total < cfg.Limits.MaxCandidates
		case <-ctx.Done():
			return false
		}
	})

	close(candidates)
	wg.Wait()
	if genErr != nil {
		return genErr
	}
	return nil
}

// checkDomain performs HTTP GET (or configured method) to determine if a domain is "working".
func checkDomain(ctx context.Context, client *http.Client, domain string, hc HTTPCheckConfig) (bool, string) {
	method := hc.Method
	if method == "" {
		method = http.MethodGet
	}
	schemes := []string{"https", "http"}
	if !hc.TryHTTPSFirst {
		schemes = []string{"http", "https"}
	}
	bodyLimit := hc.BodyLimit.Bytes
	if bodyLimit <= 0 {
		bodyLimit = 32 * 1024
	}
	ok := false
	var finalURL string

	try := func(scheme string) bool {
		url := scheme + "://" + domain + "/"
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		// read limited
		_, _ = io.CopyN(io.Discard, resp.Body, bodyLimit)
		if resp.StatusCode >= hc.AcceptStatusMin && resp.StatusCode <= hc.AcceptStatusMax {
			finalURL = url
			return true
		}
		return false
	}

	for attempt := 0; attempt <= hc.Retry; attempt++ {
		for _, scheme := range schemes {
			if try(scheme) {
				ok = true
				break
			}
		}
		if ok {
			break
		}
	}
	return ok, finalURL
}

// generateCandidates runs lexicographic enumeration per length and emits "domain" (without TLD).
// We will attach TLD outside here? Simpler: this generator returns fully qualified domain names including TLD.
func generateCandidates(gen GeneratorConfig, emit func(string) bool) error {
	if gen.MinLength < 1 || gen.MaxLength < gen.MinLength {
		return fmt.Errorf("invalid lengths: min=%d max=%d", gen.MinLength, gen.MaxLength)
	}
	alpha := gen.Alphabet
	if alpha == "" {
		alpha = "abcdefghijklmnopqrstuvwxyz0123456789-"
	}
	alphaRunes := []rune(alpha)
	alMap := make(map[rune]bool, len(alphaRunes))
	for _, r := range alphaRunes {
		alMap[r] = true
	}
	isAllowed := func(r rune) bool { return alMap[r] }

	// For each length and for each TLD
	for ln := gen.MinLength; ln <= gen.MaxLength; ln++ {
		// Counter array of positions in alphabet
		idx := make([]int, ln)
		totalComb := int64(1)
		for i := 0; i < ln; i++ {
			totalComb *= int64(len(alphaRunes))
			if totalComb > math.MaxInt32 {
				// high bound, but we'll naturally stop by emit false when reaching max_candidates
			}
		}
		for {
			// Build name
			var b strings.Builder
			b.Grow(ln)
			valid := true
			prevHyphen := false
			for i := 0; i < ln; i++ {
				r := alphaRunes[idx[i]]
				if !isAllowed(r) {
					valid = false
					break
				}
				if r == '-' {
					if !gen.AllowHyphen {
						valid = false
						break
					}
					if (!gen.AllowHyphen) ||
						(gen.ForbidLeadingHyphen && i == 0) ||
						(gen.ForbidTrailingHyphen && i == ln-1) ||
						(gen.ForbidDoubleHyphen && prevHyphen) {
						valid = false
						break
					}
					prevHyphen = true
				} else {
					prevHyphen = false
				}
				b.WriteRune(r)
			}
			if valid {
				name := b.String()
				// emit for each TLD
				for _, tld := range gen.TLDs {
					tld = strings.ToLower(strings.TrimSpace(tld))
					if tld == "" || tld[0] != '.' {
						continue
					}
					domain := name + tld
					if !emit(domain) {
						return nil
					}
				}
			}

			// increment idx like odometer
			carry := 1
			for i := ln - 1; i >= 0 && carry > 0; i-- {
				idx[i] += carry
				if idx[i] >= len(alphaRunes) {
					idx[i] = 0
					carry = 1
				} else {
					carry = 0
				}
			}
			if carry > 0 {
				break // overflow -> done for this length
			}
		}
	}
	return nil
}

func ensureSite(ctx context.Context, db *pgxpool.Pool, domain string, cfg Config) (int64, error) {
	var id int64
	const q = `
INSERT INTO sites (domain, enabled, rps_limit, rps_burst, depth_limit)
VALUES ($1, TRUE, $2, $3, $4)
ON CONFLICT (domain) DO UPDATE SET updated_at = now()
RETURNING id;`
	err := db.QueryRow(ctx, q, domain, nonZero(cfg.Limits.RatePerSecond, 10), nonZero(cfg.Limits.RatePerSecond*2, 20), nonZero(cfg.Generator.MaxLength, 2)).Scan(&id)
	return id, err
}

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
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func nonZero(v int, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// try relative to executable/workdir fallbacks
		if !filepath.IsAbs(path) {
			alt := filepath.Clean(path)
			data2, err2 := os.ReadFile(alt)
			if err2 == nil {
				var c Config
				if err3 := yaml.Unmarshal(data2, &c); err3 != nil {
					return Config{}, err3
				}
				return c, nil
			}
		}
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if len(cfg.Generator.TLDs) == 0 {
		return errors.New("generator.tlds must not be empty")
	}
	if cfg.Generator.MinLength <= 0 || cfg.Generator.MaxLength < cfg.Generator.MinLength {
		return fmt.Errorf("invalid lengths: %d..%d", cfg.Generator.MinLength, cfg.Generator.MaxLength)
	}
	if cfg.Limits.Concurrency <= 0 {
		return errors.New("limits.concurrency must be > 0")
	}
	if cfg.Limits.RatePerSecond <= 0 {
		return errors.New("limits.rate_per_second must be > 0")
	}
	if cfg.HTTPCheck.AcceptStatusMin <= 0 || cfg.HTTPCheck.AcceptStatusMax < cfg.HTTPCheck.AcceptStatusMin {
		return errors.New("invalid http_check accept status range")
	}
	return nil
}
