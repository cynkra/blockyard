package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPgCredsProvider_RotateUpdatesCache(t *testing.T) {
	var count atomic.Int32
	rot := RotatorFunc(func(ctx context.Context) (string, string, error) {
		n := count.Add(1)
		return fmt.Sprintf("u%d", n), fmt.Sprintf("p%d", n), nil
	})
	p := newPgCredsProvider(rot)

	if u, _ := p.current(); u != "" {
		t.Fatalf("fresh provider should be empty, got user=%q", u)
	}
	if !p.hasRotator() {
		t.Fatal("hasRotator should be true")
	}

	if err := p.rotate(context.Background()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	u, pw := p.current()
	if u != "u1" || pw != "p1" {
		t.Errorf("after first rotate: user=%q pass=%q", u, pw)
	}

	if err := p.rotate(context.Background()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	u, pw = p.current()
	if u != "u2" || pw != "p2" {
		t.Errorf("after second rotate: user=%q pass=%q", u, pw)
	}
}

func TestPgCredsProvider_NoRotator(t *testing.T) {
	p := newPgCredsProvider(nil)
	if p.hasRotator() {
		t.Fatal("hasRotator should be false with nil rotator")
	}
	err := p.rotate(context.Background())
	if err == nil {
		t.Fatal("rotate without rotator should error")
	}
}

func TestPgCredsProvider_RotatePropagatesError(t *testing.T) {
	want := errors.New("vault unreachable")
	rot := RotatorFunc(func(ctx context.Context) (string, string, error) {
		return "", "", want
	})
	p := newPgCredsProvider(rot)
	err := p.rotate(context.Background())
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("want wrapped %v, got %v", want, err)
	}
	// On failure the cached creds must not be clobbered.
	if u, _ := p.current(); u != "" {
		t.Errorf("creds mutated on failure: user=%q", u)
	}
}

func TestPgCredsProvider_ConcurrentRotateAndRead(t *testing.T) {
	// Race-detector sanity check: readers and rotators interleaving
	// must not tear the (user, password) pair apart.
	rot := RotatorFunc(func(ctx context.Context) (string, string, error) {
		return "alice", "secret-alice", nil
	})
	p := newPgCredsProvider(rot)
	if err := p.rotate(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					u, pw := p.current()
					if (u == "" && pw != "") || (u != "" && pw == "") {
						t.Errorf("torn pair: user=%q pass=%q", u, pw)
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		_ = p.rotate(context.Background())
	}
	close(stop)
	wg.Wait()
}

func TestIsPostgresAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"pgError 28P01", &pgconn.PgError{Code: "28P01", Message: "invalid password"}, true},
		{"pgError 28000", &pgconn.PgError{Code: "28000"}, true},
		{"pgError other", &pgconn.PgError{Code: "42P01"}, false},
		{"string SQLSTATE 28P01", errors.New("connect: FATAL: password authentication failed (SQLSTATE 28P01)"), true},
		{"string role does not exist", errors.New("FATAL: role \"v-token-XYZ\" does not exist"), true},
		{"unrelated error", errors.New("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPostgresAuthError(tt.err); got != tt.want {
				t.Errorf("isPostgresAuthError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
