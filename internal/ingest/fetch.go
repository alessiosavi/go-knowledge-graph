package ingest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/ingest/netguard"
)

// Guard is the SSRF boundary used by the fetcher: CheckURL runs before the
// initial request and again on every redirect hop. netguard.Guard is the
// production implementation; a guard may additionally implement
// DialChecker (netguard.Guard does) to validate the actual connection
// target at dial time.
type Guard interface {
	CheckURL(ctx context.Context, u *url.URL) error
}

// DialChecker validates the host:port a connection is about to dial. It is
// the rebinding-proof half of the guard: checking it at connect time closes
// the window between check-time resolution and dial-time resolution.
type DialChecker interface {
	CheckDial(network, address string) error
}

// FetchResult carries the raw body and where it came from; decoding (HTML,
// PDF, binary rejection) is ingest.Decode's job (T11).
type FetchResult struct {
	Body        []byte
	FinalURL    string
	ContentType string
}

// Fetcher performs SSRF-guarded URL fetches with size, redirect and time
// caps. Zero values fall back to the defaults (10 MiB, 5 redirects, 30s).
type Fetcher struct {
	Client       *http.Client
	Guard        Guard
	MaxBytes     int64
	MaxRedirects int
	TimeoutS     int
}

// Fetch GETs the URL through the guard, following at most MaxRedirects
// hops — each re-checked by the guard — and returns at most MaxBytes. A nil
// Guard falls back to the production netguard, so the SSRF boundary is
// fail-closed for zero-value Fetchers.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (FetchResult, error) {
	guard := f.Guard
	if guard == nil {
		guard = &netguard.Guard{}
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return FetchResult{}, fmt.Errorf("ingest: fetch: parse %q: %w", rawURL, err)
	}
	if err := guard.CheckURL(ctx, u); err != nil {
		return FetchResult{}, fmt.Errorf("ingest: fetch %s: %w", rawURL, err)
	}

	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	maxRedirects := f.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = 5
	}
	timeout := time.Duration(f.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	var client *http.Client
	if f.Client != nil {
		c := *f.Client
		// The fetcher's config owns the total time cap and the redirect
		// policy; a caller client only contributes its transport options.
		c.Timeout = timeout
		if dg, ok := guard.(DialChecker); ok {
			switch t := c.Transport.(type) {
			case nil:
				c.Transport = newGuardedTransport(dg, timeout)
			case *http.Transport:
				tc := t.Clone()
				tc.DialContext = guardedDial(dg, tc.DialContext, timeout)
				c.Transport = tc
			}
		}
		client = &c
	} else {
		client = &http.Client{Timeout: timeout}
		if dg, ok := guard.(DialChecker); ok {
			client.Transport = newGuardedTransport(dg, timeout)
		}
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("ingest: fetch %s: too many redirects (>%d)", rawURL, maxRedirects)
		}
		if err := guard.CheckURL(req.Context(), req.URL); err != nil {
			return fmt.Errorf("ingest: fetch %s: redirect blocked: %w", rawURL, err)
		}
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("ingest: fetch %s: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", "gkg/0.1 (+https://github.com/alessiosavi/go-knowledge-graph)")
	resp, err := client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("ingest: fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return FetchResult{}, fmt.Errorf("ingest: fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return FetchResult{}, fmt.Errorf("ingest: fetch %s: read body: %w", rawURL, err)
	}
	if int64(len(body)) > maxBytes {
		return FetchResult{}, fmt.Errorf("ingest: fetch %s: body exceeds %d bytes", rawURL, maxBytes)
	}
	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return FetchResult{
		Body:        body,
		FinalURL:    final,
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

// newGuardedTransport is the hardened default transport: no environment
// proxy, and every dial goes through the guard's resolve-validate-pin path.
func newGuardedTransport(dg DialChecker, timeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext:           guardedDial(dg, nil, timeout),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// guardedDial resolves the connection address, rejects blocked targets and
// dials the vetted IPs directly (pinning them, in resolution order), so the
// target cannot be rebound between the check and the connect. All approved
// candidates are attempted until one connects — a single stale address in
// a multi-address resolution must not fail the fetch. base is the wrapped
// transport's original dialer, when one exists.
func guardedDial(dg DialChecker, base func(context.Context, string, string) (net.Conn, error), timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("netguard: dial address %q: %w", addr, err)
		}
		var candidates []net.IP
		if ip := net.ParseIP(host); ip != nil {
			if err := dg.CheckDial(network, addr); err != nil {
				return nil, err
			}
			candidates = []net.IP{ip}
		} else {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("netguard: resolve %q: %w", host, err)
			}
			for _, cand := range addrs {
				if err := dg.CheckDial(network, net.JoinHostPort(cand.IP.String(), port)); err == nil {
					candidates = append(candidates, cand.IP)
				}
			}
			if len(candidates) == 0 {
				return nil, fmt.Errorf("netguard: no allowed address for %q", host)
			}
		}

		var lastErr error
		for i, ip := range candidates {
			// Split the remaining deadline across the remaining candidates
			// so a blackholed address cannot consume the whole budget.
			attemptTimeout := timeout
			if deadline, ok := ctx.Deadline(); ok {
				share := time.Until(deadline) / time.Duration(len(candidates)-i)
				if share < attemptTimeout {
					attemptTimeout = share
				}
			}
			attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
			pinned := net.JoinHostPort(ip.String(), port)
			var conn net.Conn
			if base != nil {
				conn, lastErr = base(attemptCtx, network, pinned)
			} else {
				d := &net.Dialer{Timeout: attemptTimeout}
				conn, lastErr = d.DialContext(attemptCtx, network, pinned)
			}
			cancel()
			if lastErr == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, lastErr
			}
		}
		return nil, lastErr
	}
}
