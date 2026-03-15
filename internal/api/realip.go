package api

import (
	"net"
	"net/http"
	"strings"
)

// realIPMiddleware rewrites r.RemoteAddr to the client's real IP when
// the request arrives from a trusted reverse proxy. It reads
// X-Forwarded-For and walks right-to-left, skipping trusted proxy
// entries, returning the first untrusted IP as the real client.
//
// When trustedNets is empty the middleware is a no-op — r.RemoteAddr
// is left unchanged, which is safe for deployments without a proxy.
func realIPMiddleware(trustedCIDRs []string) func(http.Handler) http.Handler {
	if len(trustedCIDRs) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	nets := make([]*net.IPNet, 0, len(trustedCIDRs))
	for _, cidr := range trustedCIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			// Config validation already caught this; skip.
			continue
		}
		nets = append(nets, n)
	}

	isTrusted := func(ip net.IP) bool {
		for _, n := range nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peerIP := parseIP(r.RemoteAddr)
			if peerIP == nil || !isTrusted(peerIP) {
				// Direct client (not from a trusted proxy) — leave as-is.
				next.ServeHTTP(w, r)
				return
			}

			// Walk X-Forwarded-For right-to-left to find the first
			// entry not in the trusted set.
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				for i := len(parts) - 1; i >= 0; i-- {
					candidate := strings.TrimSpace(parts[i])
					ip := net.ParseIP(candidate)
					if ip == nil {
						continue
					}
					if !isTrusted(ip) {
						r.RemoteAddr = candidate
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			// Fallback: check X-Real-IP.
			if xri := r.Header.Get("X-Real-IP"); xri != "" {
				ip := net.ParseIP(strings.TrimSpace(xri))
				if ip != nil && !isTrusted(ip) {
					r.RemoteAddr = strings.TrimSpace(xri)
					next.ServeHTTP(w, r)
					return
				}
			}

			// All forwarded entries are trusted (or header missing) —
			// keep the original RemoteAddr.
			next.ServeHTTP(w, r)
		})
	}
}

// parseIP extracts the IP from a host:port or bare IP string.
func parseIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return net.ParseIP(host)
}
