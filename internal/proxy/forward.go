package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// forwardHTTP proxies an HTTP request to the worker at addr. The
// /app/{name} prefix is stripped from the path before forwarding.
func forwardHTTP(w http.ResponseWriter, r *http.Request, addr, appName string, transport http.RoundTripper) {
	target := &url.URL{
		Scheme: "http",
		Host:   addr,
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport

	// Rewrite the request: strip prefix, set host, add forwarded headers
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = stripAppPrefix(req.URL.Path, appName)
		req.URL.RawPath = ""
		req.Host = addr

		// Preserve the original protocol so Shiny apps behind a
		// TLS-terminating reverse proxy see the correct scheme.
		proto := req.Header.Get("X-Forwarded-Proto")
		if proto != "https" {
			proto = "http"
		}
		req.Header.Set("X-Forwarded-Proto", proto)
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
