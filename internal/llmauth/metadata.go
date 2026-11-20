package llmauth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/sync/singleflight"
)

const (
	metadataIP     = "169.254.169.254"
	tokenPath      = "instance/service-accounts/default/token"
	refreshTimeout = 10 * time.Second
	renewBefore    = 60 * time.Second
)

type metadataProvider struct {
	client  *metadata.Client
	timeout time.Duration
	now     func() time.Time

	refresh singleflight.Group
	mu      sync.Mutex
	cached  credential
}

type credential struct {
	token  string
	expiry time.Time
}

// metadataTransport keeps the library's GCE_METADATA_HOST test override and
// proxy settings from changing the production destination.
type metadataTransport struct{ transport *http.Transport }

func (t metadataTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "http" || req.URL.Host != metadataIP || req.URL.User != nil ||
		req.URL.Path != "/computeMetadata/v1/"+tokenPath || req.URL.RawQuery != "" {
		return nil, errors.New("unexpected metadata endpoint")
	}
	return t.transport.RoundTrip(req)
}

var gceProvider = newMetadataProvider(metadata.NewClient(&http.Client{
	Transport:     metadataTransport{&http.Transport{}},
	CheckRedirect: RejectRedirect,
}))

// GCE returns the shared, lazily refreshed default service account provider.
func GCE() TokenProvider { return gceProvider }

func newMetadataProvider(client *metadata.Client) *metadataProvider {
	return &metadataProvider{client: client, now: time.Now, timeout: refreshTimeout}
}

func (p *metadataProvider) current() (credential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cached, p.cached.expiry.After(p.now().Add(renewBefore))
}

func (p *metadataProvider) Token(ctx context.Context) (string, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return "", time.Time{}, err
	}
	if cached, fresh := p.current(); fresh {
		return cached.token, cached.expiry, nil
	}
	result := p.refresh.DoChan("token", func() (any, error) {
		if cached, fresh := p.current(); fresh {
			return cached, nil
		}
		refreshCtx, cancel := context.WithTimeout(context.Background(), p.timeout)
		defer cancel()
		next, err := p.fetch(refreshCtx)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		p.cached = next
		p.mu.Unlock()
		return next, nil
	})
	select {
	case <-ctx.Done():
		return "", time.Time{}, ctx.Err()
	case res := <-result:
		if err := ctx.Err(); err != nil {
			return "", time.Time{}, err
		}
		if res.Err != nil {
			return "", time.Time{}, res.Err
		}
		next := res.Val.(credential)
		return next.token, next.expiry, nil
	}
}

func (p *metadataProvider) fetch(ctx context.Context) (credential, error) {
	start := p.now()
	body, err := p.client.GetWithContext(ctx, tokenPath)
	if err != nil {
		var missing metadata.NotDefinedError
		var status *metadata.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return credential{}, &AuthTransient{Detail: "GCE metadata token request timed out"}
		case errors.As(err, &missing):
			return credential{}, &AuthPermanent{Detail: "default GCE service account is missing"}
		case errors.As(err, &status) && (status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden):
			return credential{}, &AuthPermanent{Detail: "metadata server denied the default GCE service account token"}
		default:
			return credential{}, &AuthTransient{Detail: "could not fetch the GCE metadata token"}
		}
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil ||
		!bearerToken.MatchString(payload.AccessToken) || payload.ExpiresIn <= 0 ||
		payload.ExpiresIn > math.MaxInt64/int64(time.Second) ||
		(payload.TokenType != "" && !strings.EqualFold(payload.TokenType, "Bearer")) {
		return credential{}, &AuthTransient{Detail: "invalid GCE metadata token response"}
	}
	expiry := start.Add(time.Duration(payload.ExpiresIn) * time.Second)
	if !expiry.After(p.now()) {
		return credential{}, &AuthTransient{Detail: "GCE metadata token has expired"}
	}
	return credential{token: payload.AccessToken, expiry: expiry}, nil
}
