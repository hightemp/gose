package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// writeJSON writes JSON with pretty indentation and status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// normalizeHost lowercases, strips default ports and trailing dot, removes leading www.
func normalizeHost(h string) string {
	host := strings.ToLower(strings.TrimSpace(h))
	// strip port
	if strings.Contains(host, ":") {
		if x, _, err := net.SplitHostPort(host); err == nil {
			host = x
		}
	}
	host = strings.TrimSuffix(host, ".")
	host = strings.TrimPrefix(host, "www.")
	return host
}

// isHostAllowed checks domain against whitelist (exact or subdomain).
func isHostAllowed(host string, whitelist []string) bool {
	for _, d := range whitelist {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// sha256Hex returns hex-encoded SHA256 of a string.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
