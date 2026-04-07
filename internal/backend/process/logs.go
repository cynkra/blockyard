package process

import (
	"bufio"
	"io"
	"sync"

	"github.com/cynkra/blockyard/internal/backend"
)

// logBuffer captures output from a child process and serves it as a
// LogStream. Lines are stored in a fixed-size circular buffer.
// Subscribers track a global sequence number so their cursor stays
// valid across ring wraps. Each subscriber gets its own notification
// channel so broadcasts wake all viewers, not just one.
type logBuffer struct {
	mu     sync.Mutex
	buf    []string // fixed-size ring buffer
	size   int      // len(buf), set at init
	seq    uint64   // total lines written (monotonic); buf index = seq % size
	closed bool
	subs   []chan struct{} // per-subscriber notification channels
}

func newLogBuffer(maxLines int) *logBuffer {
	return &logBuffer{
		buf:  make([]string, maxLines),
		size: maxLines,
	}
}

// broadcast wakes all subscribers. Called with lb.mu held.
func (lb *logBuffer) broadcast() {
	for _, ch := range lb.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// subscribe registers a notification channel. Returns an unsubscribe func.
func (lb *logBuffer) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	lb.mu.Lock()
	lb.subs = append(lb.subs, ch)
	lb.mu.Unlock()
	return ch, func() {
		lb.mu.Lock()
		for i, c := range lb.subs {
			if c == ch {
				lb.subs = append(lb.subs[:i], lb.subs[i+1:]...)
				break
			}
		}
		lb.mu.Unlock()
	}
}

// ingest reads lines from r until EOF and writes them to the ring.
func (lb *logBuffer) ingest(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lb.mu.Lock()
		lb.buf[lb.seq%uint64(lb.size)] = scanner.Text()
		lb.seq++
		lb.broadcast()
		lb.mu.Unlock()
	}
	lb.mu.Lock()
	lb.closed = true
	lb.broadcast()
	lb.mu.Unlock()
}

// stream returns a LogStream that replays buffered lines and follows.
func (lb *logBuffer) stream() backend.LogStream {
	ch := make(chan string, 64)
	done := make(chan struct{})
	notify, unsub := lb.subscribe()

	go func() {
		defer close(ch)
		defer unsub()

		// Start cursor at the oldest available line. If the ring has
		// wrapped, that's seq - size; otherwise 0.
		lb.mu.Lock()
		var cursor uint64
		if lb.seq > uint64(lb.size) {
			cursor = lb.seq - uint64(lb.size)
		}
		lb.mu.Unlock()

		for {
			lb.mu.Lock()
			seq := lb.seq
			closed := lb.closed
			// Copy out any lines between cursor and current seq.
			//
			// The oldest still-resident line has global sequence number
			// max(0, seq-size). Computing seq-size on uint64 when seq <
			// size would underflow to a huge value, hiding all lines —
			// guard against that explicitly.
			var oldest uint64
			if seq > uint64(lb.size) {
				oldest = seq - uint64(lb.size)
			}
			var pending []string
			for cursor < seq {
				// The line at global sequence number `cursor` lives at
				// buf[cursor % size] — but only if it hasn't been
				// overwritten (cursor >= oldest).
				if cursor >= oldest {
					pending = append(pending, lb.buf[cursor%uint64(lb.size)])
				}
				cursor++
			}
			lb.mu.Unlock()

			for _, line := range pending {
				select {
				case ch <- line:
				case <-done:
					return
				}
			}
			if closed && cursor >= seq {
				return
			}
			// Wait for new data.
			select {
			case <-notify:
			case <-done:
				return
			}
		}
	}()

	return backend.LogStream{
		Lines: ch,
		Close: func() {
			// done may already have been closed by the goroutine; ignore panics
			defer func() { _ = recover() }()
			close(done)
		},
	}
}
