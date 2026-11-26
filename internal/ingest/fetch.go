package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
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
// caps. Zero byte, redirect and time caps use 10 MiB, 5 redirects and 30s.
type Fetcher struct {
	Client       *http.Client
	Guard        Guard
	MaxBytes     int64
	MaxRedirects int
	TimeoutS     int

	// Proxy routes fetches through an explicit HTTP proxy. Target URLs
	// and redirects still pass CheckURL. The guarded dial validates and pins
	// the proxy address. The trusted proxy controls its connection to the target.
	// Each attempt uses and closes a fresh transport so a rotating proxy can
	// assign a new exit on a new connection.
	Proxy *url.URL

	// FetchRetries is the number of additional attempts for proxied fetches.
	// Zero still checks that the proxy lands on the requested page.
	FetchRetries int
	waitRetry    func(context.Context, time.Duration) bool
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
	if f.Proxy != nil {
		return f.fetchThroughProxy(ctx, rawURL, u, guard)
	}
	result, err := f.fetchOnce(ctx, rawURL, u, guard)
	if err != nil {
		return FetchResult{}, fmt.Errorf("ingest: fetch %s: %w", rawURL, err)
	}
	return result, nil
}

func (f *Fetcher) fetchOnce(ctx context.Context, rawURL string, u *url.URL, guard Guard) (FetchResult, error) {
	timeout := time.Duration(f.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if f.Proxy != nil {
		// Cap the entire attempt, including the initial URL guard and body.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if err := guard.CheckURL(ctx, u); err != nil {
		return FetchResult{}, err
	}

	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	maxRedirects := f.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = 5
	}
	var client http.Client
	if f.Client != nil {
		client = *f.Client
	}
	client.Timeout = timeout
	var transport *http.Transport
	switch t := client.Transport.(type) {
	case nil:
		transport = newFetchTransport()
	case *http.Transport:
		transport = t.Clone()
	default:
		if f.Proxy != nil {
			return FetchResult{}, errors.New("fetch proxy requires an HTTP transport")
		}
	}
	if transport != nil {
		transport.Proxy = nil
		if f.Proxy != nil {
			transport.Proxy = http.ProxyURL(f.Proxy)
		}
		if dg, ok := guard.(DialChecker); ok {
			transport.DialContext = guardedDial(dg, transport.DialContext, timeout)
			if f.Proxy != nil {
				// HTTPS proxy connections must use the guarded TCP dial before TLS.
				transport.DialTLSContext = nil
				transport.DialTLS = nil
			}
		}
		client.Transport = transport
		defer transport.CloseIdleConnections()
		if f.Proxy != nil {
			client.Timeout = 0 // The attempt context supplies the deadline.
			client.Transport = newProxiedFetchTransport(transport)
		}
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		var err error
		if len(via) > maxRedirects {
			err = fmt.Errorf("too many redirects (>%d)", maxRedirects)
		} else if blocked := guard.CheckURL(req.Context(), req.URL); blocked != nil {
			err = fmt.Errorf("redirect blocked: %w", blocked)
		}
		if err != nil && f.Proxy == nil {
			return fmt.Errorf("ingest: fetch %s: %w", rawURL, err)
		}
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("User-Agent", "gkg/0.1 (+https://github.com/alessiosavi/go-knowledge-graph)")
	resp, err := client.Do(req)
	if err != nil {
		if reqErr, ok := err.(*url.Error); ok && f.Proxy != nil {
			err = reqErr.Err // Report the cause without echoing the request URL twice.
		}
		return FetchResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return FetchResult{}, fetchHTTPError(resp.StatusCode)
	}

	if f.Proxy != nil && resp.Request != nil && resp.Request.URL != nil {
		landing := resp.Request.URL
		if !sameFetchPage(u, landing) {
			return FetchResult{}, fetchAttemptError{error: wrongFetchPage(f.proxySafeURL(landing)), retryable: true}
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return FetchResult{}, fetchAttemptError{error: fmt.Errorf("read body: %w", err), retryable: true}
	}
	if int64(len(body)) > maxBytes {
		return FetchResult{}, fmt.Errorf("body exceeds %d bytes", maxBytes)
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

func newFetchTransport() *http.Transport {
	return &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Transport errors can echo a proxy's response text, including credentials.
func redactProxyError(err error, proxy *url.URL) error {
	if containsProxyCredentials(err.Error(), proxy) {
		return errors.New("proxy request failed (credentials omitted)")
	}
	return err
}

func containsProxyCredentials(msg string, proxy *url.URL) bool {
	if proxy == nil || proxy.User == nil {
		return false
	}
	username := proxy.User.Username()
	password, _ := proxy.User.Password()
	secrets := []string{
		proxy.User.String() + "@",
		base64.StdEncoding.EncodeToString([]byte(username + ":" + password)),
		url.User(username).String(), url.UserPassword("", password).String()[1:],
		username, password,
		url.QueryEscape(username), url.QueryEscape(password),
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(msg, secret) {
			return true
		}
	}
	return false
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
				return nil, fetchAttemptError{error: err}
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
				return nil, fetchAttemptError{error: fmt.Errorf("netguard: no allowed address for %q", host)}
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
