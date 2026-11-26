package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (f *Fetcher) fetchThroughProxy(ctx context.Context, rawURL string, requested *url.URL, guard Guard) (FetchResult, error) {
	target := f.proxySafeURL(requested)
	wait := f.waitRetry
	if wait == nil {
		wait = waitForFetchRetry
	}
	delay := time.Second
	for attempt := 1; ; attempt++ {
		result, err := f.fetchOnce(ctx, rawURL, requested, guard)
		if err == nil {
			return result, nil
		}
		var failure fetchAttemptError
		retry := errors.As(err, &failure) && failure.retryable
		reason := redactProxyError(err, f.Proxy)
		if strings.ContainsAny(reason.Error(), "\r\n") {
			reason = errors.New(strings.NewReplacer("\r", " ", "\n", " ").Replace(reason.Error()))
		}
		if attempt > f.FetchRetries || !retry || ctx.Err() != nil || !wait(ctx, delay) || ctx.Err() != nil {
			var landing wrongFetchPage
			if errors.As(reason, &landing) {
				return FetchResult{}, fmt.Errorf("ingest: fetch %s: %w instead of the requested page after %d attempts through the fetch proxy; submit the landing URL if that page is intended", target, reason, attempt)
			}
			return FetchResult{}, fmt.Errorf("ingest: fetch %s: %w after %d attempts through the fetch proxy", target, reason, attempt)
		}
		slog.Warn("ingest: retrying fetch through proxy", "url", target, "attempt", attempt, "reason", reason.Error())
		delay = min(delay*2, 8*time.Second)
	}
}

func waitForFetchRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func sameFetchPage(requested, landing *url.URL) bool {
	return fetchPageHost(requested) == fetchPageHost(landing) &&
		strings.TrimSuffix(requested.EscapedPath(), "/") == strings.TrimSuffix(landing.EscapedPath(), "/")
}

func fetchPageHost(u *url.URL) string {
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch port := u.Port(); port {
	case "", "80", "443":
		return host
	default:
		return net.JoinHostPort(host, port)
	}
}

func (f *Fetcher) proxySafeURL(u *url.URL) string {
	safe := *u
	safe.User = nil
	value := safe.String()
	if containsProxyCredentials(value, f.Proxy) {
		return "[redacted URL]"
	}
	return value
}

type wrongFetchPage string

func (e wrongFetchPage) Error() string { return "landed on " + string(e) }

type fetchAttemptError struct {
	error
	retryable bool
}

func (e fetchAttemptError) Unwrap() error { return e.error }

func fetchHTTPError(status int) fetchAttemptError {
	return fetchAttemptError{error: fmt.Errorf("HTTP %d", status), retryable: retryFetchStatus(status)}
}

func retryFetchStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

type proxiedFetchTransport struct{ *http.Transport }

func newProxiedFetchTransport(transport *http.Transport) proxiedFetchTransport {
	onConnect := transport.OnProxyConnectResponse
	transport.OnProxyConnectResponse = func(ctx context.Context, proxy *url.URL, req *http.Request, res *http.Response) error {
		if onConnect != nil {
			if err := onConnect(ctx, proxy, req, res); err != nil {
				return fetchAttemptError{error: err, retryable: retryFetchStatus(res.StatusCode)}
			}
		}
		if res.StatusCode != http.StatusOK {
			// net/http would otherwise expose only the proxy's reason phrase.
			return fetchHTTPError(res.StatusCode)
		}
		return nil
	}
	return proxiedFetchTransport{transport}
}

func (t proxiedFetchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Transport.RoundTrip(req)
	var failure fetchAttemptError
	if err != nil && !errors.As(err, &failure) {
		err = fetchAttemptError{error: err, retryable: true}
	}
	return resp, err
}
