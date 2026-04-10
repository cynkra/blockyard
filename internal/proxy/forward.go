package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"
)

// forwardHTTP proxies an HTTP request to the worker at addr. The
// /app/{name} prefix is stripped from the path before forwarding.
// httpTimeout caps the total request lifetime (dial + headers + body)
// to prevent a worker from holding connections indefinitely.
func forwardHTTP(w http.ResponseWriter, r *http.Request, addr, appName, externalURL string, transport http.RoundTripper, httpTimeout time.Duration) {
	// Apply a deadline so the entire round-trip is bounded. This
	// prevents a worker from holding resources by trickling response
	// bytes indefinitely.
	ctx, cancel := r.Context(), func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		ctx, cancel = context.WithTimeout(ctx, httpTimeout)
	}
	defer cancel()
	r = r.WithContext(ctx)

	slog.Debug("proxy: forwarding HTTP", //nolint:gosec // G706: slog structured logging handles this
		"app", appName, "backend", addr,
		"path", stripAppPrefix(r.URL.Path, appName))
	target := &url.URL{
		Scheme: "http",
		Host:   addr,
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = path.Clean(stripAppPrefix(pr.In.URL.Path, appName))
			pr.Out.URL.RawPath = ""
			pr.Out.Host = addr

			// SetXForwarded sets X-Forwarded-For, -Host, -Proto from
			// the inbound request. Override -Proto afterward so Shiny
			// apps behind TLS-terminating proxies see the correct scheme.
			pr.SetXForwarded()
			proto := "http"
			if strings.HasPrefix(externalURL, "https://") {
				proto = "https"
			}
			pr.Out.Header.Set("X-Forwarded-Proto", proto)
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("proxy: backend error", "app", appName, "error", err) //nolint:gosec // G706: slog structured logging handles this
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}

// stripAppPrefix removes /app/{name} from the start of a URL path.
// Always returns a path starting with /.
func stripAppPrefix(path, appName string) string {
	prefix := "/app/" + appName
	stripped := strings.TrimPrefix(path, prefix)
	if stripped == "" || stripped[0] != '/' {
		return "/" + stripped
	}
	return stripped
}
