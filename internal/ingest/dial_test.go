package ingest

import (
	"context"
	"errors"
	"fmt"
	"net"
	neturl "net/url"
	"testing"
	"time"
)

// dialStub is a DialChecker for the white-box dial tests.
type dialStub struct {
	blocked map[string]bool
}

func (d dialStub) CheckURL(context.Context, *neturl.URL) error { return nil }

func (d dialStub) CheckDial(_, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if d.blocked[host] {
		return fmt.Errorf("netguard: blocked dial target %s", host)
	}
	return nil
}

// TestGuardedDialSplitsBudgetAcrossCandidates pins that a slow (blackholed)
// first candidate cannot consume the whole fetch budget: each attempt gets
// a share of the remaining deadline, so later candidates are still tried.
func TestGuardedDialSplitsBudgetAcrossCandidates(t *testing.T) {
	var attempts []string
	base := func(ctx context.Context, _, addr string) (net.Conn, error) {
		attempts = append(attempts, addr)
		if len(attempts) == 1 {
			<-ctx.Done() // simulate a blackholed address
			return nil, ctx.Err()
		}
		c1, c2 := net.Pipe()
		t.Cleanup(func() { _ = c2.Close() })
		return c1, nil
	}
	dial := guardedDial(dialStub{}, base, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	conn, err := dial(ctx, "tcp", "localhost:9999")
	if err != nil {
		t.Fatalf("dial = %v, want the second candidate reached within the budget", err)
	}
	_ = conn.Close()
	if len(attempts) < 2 {
		t.Fatalf("attempts = %v, want the slow first candidate to be abandoned for the next", attempts)
	}
}

// TestGuardedDialTriesAllAllowedAddresses pins multi-address fallback: the
// first allowed candidate may be unreachable while a later one connects.
func TestGuardedDialTriesAllAllowedAddresses(t *testing.T) {
	var attempts []string
	base := func(_ context.Context, _, addr string) (net.Conn, error) {
		attempts = append(attempts, addr)
		if len(attempts) == 1 {
			return nil, errors.New("connection refused")
		}
		c1, c2 := net.Pipe()
		t.Cleanup(func() { _ = c2.Close() })
		return c1, nil
	}
	dial := guardedDial(dialStub{}, base, time.Second)
	conn, err := dial(context.Background(), "tcp", "localhost:9999")
	if err != nil {
		t.Fatalf("dial = %v, want fallback to the second address", err)
	}
	_ = conn.Close()
	if len(attempts) < 2 {
		t.Fatalf("attempts = %v, want every resolved address tried", attempts)
	}
	for _, a := range attempts {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			t.Fatalf("attempt %q: %v", a, err)
		}
		if net.ParseIP(host) == nil {
			t.Errorf("attempt %q is not pinned to an IP", a)
		}
	}
}

func TestGuardedDialFailsClosedWhenAllCandidatesFail(t *testing.T) {
	calls := 0
	base := func(_ context.Context, _, _ string) (net.Conn, error) {
		calls++
		return nil, errors.New("connection refused")
	}
	dial := guardedDial(dialStub{}, base, time.Second)
	if _, err := dial(context.Background(), "tcp", "localhost:9999"); err == nil {
		t.Fatal("dial with all candidates failing: want error")
	}
	if calls == 0 {
		t.Error("no dial attempts made")
	}
}

func TestGuardedDialSkipsBlockedCandidates(t *testing.T) {
	var attempts []string
	base := func(_ context.Context, _, addr string) (net.Conn, error) {
		attempts = append(attempts, addr)
		c1, c2 := net.Pipe()
		t.Cleanup(func() { _ = c2.Close() })
		return c1, nil
	}
	// Block every loopback form: localhost resolves to blocked addresses
	// only, so the dial must fail closed without any attempt.
	dial := guardedDial(dialStub{blocked: map[string]bool{"127.0.0.1": true, "::1": true}}, base, time.Second)
	if _, err := dial(context.Background(), "tcp", "localhost:9999"); err == nil {
		t.Fatal("dial with all candidates blocked: want error")
	}
	if len(attempts) != 0 {
		t.Errorf("attempts = %v, want none for blocked candidates", attempts)
	}
}

func TestGuardedDialRespectsCancelledContext(t *testing.T) {
	base := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("dial: %w", ctx.Err())
	}
	dial := guardedDial(dialStub{}, base, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := dial(ctx, "tcp", "localhost:9999"); err == nil {
		t.Fatal("cancelled dial: want error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("dial took %s, want prompt cancellation", elapsed)
	}
}
