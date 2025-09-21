package main

import (
	"fmt"
	"net/url"
	"sync/atomic"
)

// ProxyPool is a minimal round-robin pool (MVP).
type ProxyPool struct {
	rotation string
	proxies  []*url.URL
	counter  uint64
}

func NewProxyPool(cfg ProxiesConfig) (*ProxyPool, error) {
	if len(cfg.Proxies) == 0 {
		return &ProxyPool{rotation: cfg.Rotation}, nil
	}
	var parsed []*url.URL
	for _, p := range cfg.Proxies {
		u, err := url.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", p, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5":
			parsed = append(parsed, u)
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}
	rot := cfg.Rotation
	if rot == "" {
		rot = "round_robin"
	}
	return &ProxyPool{rotation: rot, proxies: parsed}, nil
}

func (p *ProxyPool) Len() int {
	return len(p.proxies)
}

func (p *ProxyPool) Next() *url.URL {
	if len(p.proxies) == 0 {
		return nil
	}
	i := atomic.AddUint64(&p.counter, 1)
	return p.proxies[(int(i)-1)%len(p.proxies)]
}
