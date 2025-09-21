package main

import (
	"sync"

	"golang.org/x/time/rate"
)

// --- Per-host rate limiter registry ---
var (
	hostLimiterOnce sync.Once
	hostLimiterMap  *sync.Map // map[string]*rate.Limiter
)

func getHostLimiter(host string, rps int, burst int) *rate.Limiter {
	if host == "" {
		return nil
	}
	hostLimiterOnce.Do(func() { hostLimiterMap = &sync.Map{} })
	if v, ok := hostLimiterMap.Load(host); ok {
		return v.(*rate.Limiter)
	}
	if rps <= 0 {
		rps = 10
	}
	if burst <= 0 {
		burst = 20
	}
	lim := rate.NewLimiter(rate.Limit(rps), burst)
	actual, _ := hostLimiterMap.LoadOrStore(host, lim)
	return actual.(*rate.Limiter)
}
