package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath = "./deploy/crawler.config.yaml"
)

// Global service start time (used in /healthz)
var (
	startedAt = time.Now()
)

// Config is the main crawler service config (YAML).
type Config struct {
	Version  int            `yaml:"version"`
	Postgres PostgresConfig `yaml:"postgres"`
	HTTP     HTTPConfig     `yaml:"http"`
	Crawler  CrawlerConfig  `yaml:"crawler"`
	Robots   RobotsConfig   `yaml:"robots"`
	Sitemap  SitemapConfig  `yaml:"sitemap"`
	Proxies  ProxiesRef     `yaml:"proxies"`
}

type PostgresConfig struct {
	DSN string `yaml:"dsn"`
}

type HTTPConfig struct {
	Addr string `yaml:"addr"`
}

type CrawlerConfig struct {
	WhitelistDomains []string `yaml:"whitelist_domains"`
	SeedURLs         []string `yaml:"seed_urls"`
	DepthLimit       int      `yaml:"depth_limit"`
	RPSPerHost       int      `yaml:"rps_per_host"`
	RPSBurst         int      `yaml:"rps_burst"`
	Workers          int      `yaml:"workers"` // 0 or missing -> default: min(runtime.NumCPU()*4, 64)
	HTMLFetchTimeout Duration `yaml:"html_fetch_timeout"`
	HTMLMaxSize      ByteSize `yaml:"html_max_size"`
	UserAgent        string   `yaml:"user_agent"`
	ContentTypes     []string `yaml:"content_types"`
	Languages        []string `yaml:"languages"`
}

type RobotsConfig struct {
	Respect   bool     `yaml:"respect"`
	CacheTTL  Duration `yaml:"cache_ttl"`
	UserAgent string   `yaml:"user_agent"`
}

type SitemapConfig struct {
	Enabled         bool     `yaml:"enabled"`
	RefreshInterval Duration `yaml:"refresh_interval"`
}

type ProxiesRef struct {
	ConfigPath string `yaml:"config_path"`
}

// ProxiesConfig is the YAML loaded from proxies.yaml
type ProxiesConfig struct {
	Version     int               `yaml:"version"`
	Rotation    string            `yaml:"rotation"` // round_robin, random (MVP: round_robin)
	BanPolicy   BanPolicyConfig   `yaml:"ban_policy"`
	Healthcheck HealthcheckConfig `yaml:"healthcheck"`
	Proxies     []string          `yaml:"proxies"`
}

type BanPolicyConfig struct {
	ConsecutiveErrors int      `yaml:"consecutive_errors"`
	BanDuration       Duration `yaml:"ban_duration"`
}

type HealthcheckConfig struct {
	Method   string   `yaml:"method"`
	URL      string   `yaml:"url"`
	Timeout  Duration `yaml:"timeout"`
	Interval Duration `yaml:"interval"`
}

// Duration is a thin wrapper to parse Go durations from YAML.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	du, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = du
	return nil
}

// ByteSize parses sizes like "2MB", "512KB".
type ByteSize struct {
	Bytes int64
}

func (b *ByteSize) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	n, err := parseByteSize(s)
	if err != nil {
		return err
	}
	b.Bytes = n
	return nil
}

func parseByteSize(s string) (int64, error) {
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
		// raw bytes (no suffix)
	}
	val := strings.TrimSpace(ss)
	var n int64
	_, err := fmt.Sscan(val, &n)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return n * mult, nil
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

func loadProxies(path string) (ProxiesConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ProxiesConfig{}, err
	}
	var cfg ProxiesConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ProxiesConfig{}, err
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolveRelativePath tries to resolve p relative to basePath's directory if p doesn't exist.
// Useful for configs referenced from the main config.
func resolveRelativePath(basePath string, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}
	base := filepath.Dir(basePath)
	pp := filepath.Join(base, p)
	if _, err := os.Stat(pp); err == nil {
		return pp
	}
	return p
}
