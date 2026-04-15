package proxy

import (
	"net/http"
	"sync"
)

// statusRecorder wraps http.ResponseWriter and fires onStatus exactly
// once, the moment the response status is decided (first WriteHeader
// or first Write — net/http defaults Write-before-WriteHeader to 200).
//
// Firing on status decision rather than on handler return matters for
// long-lived responses: a hijacked WebSocket can stay open for hours,
// so a deferred-at-handler-exit increment would leave
// blockyard_proxy_requests_total stale until the client disconnects.
//
// Hijack and Flush are deliberately NOT implemented directly. Callers
// (coder/websocket, httputil.ReverseProxy) walk the wrapper chain via
// Unwrap and pick up the underlying writer's implementations.
// Implementing Hijack here would short-circuit that walk and force us
// to walk the inner chain ourselves — easy to get wrong because the
// api router wraps with another middleware that does not implement
// Hijack, so a naive type assertion fails there.
type statusRecorder struct {
	http.ResponseWriter
	status   int
	once     sync.Once
	onStatus func(code int)
}

func newStatusRecorder(w http.ResponseWriter, onStatus func(code int)) *statusRecorder {
	return &statusRecorder{
		ResponseWriter: w,
		status:         http.StatusOK,
		onStatus:       onStatus,
	}
}

func (s *statusRecorder) WriteHeader(code int) {
	s.fire(code)
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	// net/http treats a Write before WriteHeader as an implicit 200.
	s.fire(http.StatusOK)
	return s.ResponseWriter.Write(b)
}

// fireDefault records http.StatusOK if no real status was ever
// written. The handler defers this so a request that never produces
// a response (panic recovered upstream, hijack without WriteHeader,
// etc.) still contributes one sample.
func (s *statusRecorder) fireDefault() {
	s.fire(http.StatusOK)
}

func (s *statusRecorder) fire(code int) {
	s.once.Do(func() {
		s.status = code
		s.onStatus(code)
	})
}

// Unwrap lets http.ResponseController and the coder/websocket
// hijacker walk past us to the underlying writer's optional methods
// (Hijack, SetReadDeadline, Flush, …).
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
