package preflight

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

// TestCheckRedisAuthEmptyConfig — no Redis configured at all; check
// should short-circuit OK without dialing anywhere.
func TestCheckRedisAuthEmptyConfig(t *testing.T) {
	res := CheckRedisAuth(nil)
	if res.Severity != SeverityOK {
		t.Errorf("nil config: severity = %v, want OK", res.Severity)
	}
	res = CheckRedisAuth(&config.RedisConfig{URL: ""})
	if res.Severity != SeverityOK {
		t.Errorf("empty URL: severity = %v, want OK", res.Severity)
	}
}

// TestCheckRedisAuthTLS — rediss:// short-circuits to Info without
// dialing. A plain-TCP probe against a TLS server would write
// garbage at the handshake layer and get an unhelpful reply.
func TestCheckRedisAuthTLS(t *testing.T) {
	res := CheckRedisAuth(&config.RedisConfig{URL: "rediss://redis.example:6379"})
	if res.Severity != SeverityInfo {
		t.Errorf("rediss: severity = %v, want Info", res.Severity)
	}
	if !strings.Contains(res.Message, "TLS") {
		t.Errorf("message should mention TLS: %q", res.Message)
	}
}

// TestCheckRedisAuthParseFail — a URL that TCPAddrFromRedisURL
// cannot extract a host:port from must report Info, not a spurious
// OK that would mask a real config mistake. Uses a raw control
// character, which url.Parse rejects outright.
func TestCheckRedisAuthParseFail(t *testing.T) {
	res := CheckRedisAuth(&config.RedisConfig{URL: "redis://\x7f:6379"})
	if res.Severity == SeverityOK {
		t.Errorf("unparseable URL should not produce OK; got: %q", res.Message)
	}
}

// TestCheckRedisAuthUnreachable — unreachable target reports Info
// (reachability is surfaced by checkRedisOnServiceNetwork, not this
// check).
func TestCheckRedisAuthUnreachable(t *testing.T) {
	// 127.0.0.1:1 is privileged and typically unused; dial fails fast.
	res := CheckRedisAuth(&config.RedisConfig{URL: "redis://127.0.0.1:1"})
	if res.Severity != SeverityInfo {
		t.Errorf("unreachable: severity = %v, want Info; message: %q",
			res.Severity, res.Message)
	}
	if !strings.Contains(res.Message, "not reachable") {
		t.Errorf("message should say not reachable: %q", res.Message)
	}
}

// fakeRedis is a minimal TCP server that returns a fixed reply to
// the first RESP PING it reads. Each test wires up its own server,
// points CheckRedisAuth at it, and asserts the classification.
type fakeRedis struct {
	ln    net.Listener
	reply []byte
}

func newFakeRedis(t *testing.T, reply string) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRedis{ln: ln, reply: []byte(reply)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Drain whatever the client sends; don't care what it is.
				_, _ = io.CopyN(io.Discard, c, int64(len("*1\r\n$4\r\nPING\r\n")))
				_, _ = c.Write([]byte(reply))
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return fr
}

func (fr *fakeRedis) url() string {
	_, port, _ := net.SplitHostPort(fr.ln.Addr().String())
	return fmt.Sprintf("redis://127.0.0.1:%s", port)
}

func TestCheckRedisAuthPong(t *testing.T) {
	fr := newFakeRedis(t, "+PONG\r\n")
	res := CheckRedisAuth(&config.RedisConfig{URL: fr.url()})
	if res.Severity != SeverityError {
		t.Errorf("+PONG: severity = %v, want Error; message: %q",
			res.Severity, res.Message)
	}
	if !strings.Contains(res.Message, "without authentication") {
		t.Errorf("Error message should name the footgun: %q", res.Message)
	}
}

func TestCheckRedisAuthNoauth(t *testing.T) {
	fr := newFakeRedis(t, "-NOAUTH Authentication required.\r\n")
	res := CheckRedisAuth(&config.RedisConfig{URL: fr.url()})
	if res.Severity != SeverityOK {
		t.Errorf("-NOAUTH: severity = %v, want OK; message: %q",
			res.Severity, res.Message)
	}
}

func TestCheckRedisAuthGenericError(t *testing.T) {
	fr := newFakeRedis(t, "-ERR max number of clients reached\r\n")
	res := CheckRedisAuth(&config.RedisConfig{URL: fr.url()})
	if res.Severity != SeverityInfo {
		t.Errorf("-ERR: severity = %v, want Info; message: %q",
			res.Severity, res.Message)
	}
	if !strings.Contains(res.Message, "unexpected reply") {
		t.Errorf("Info message should say unexpected reply: %q", res.Message)
	}
}

// TestCheckRedisAuthEmptyReply — a server that closes without
// responding (e.g. a TLS-only server rejecting plain-TCP bytes).
// Surfaced as Info with the raw reply visible to the operator.
func TestCheckRedisAuthEmptyReply(t *testing.T) {
	fr := newFakeRedis(t, "")
	res := CheckRedisAuth(&config.RedisConfig{URL: fr.url()})
	if res.Severity != SeverityInfo {
		t.Errorf("empty reply: severity = %v, want Info; message: %q",
			res.Severity, res.Message)
	}
}
