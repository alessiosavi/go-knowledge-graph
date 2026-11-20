// Package llmauth supplies GCE access tokens and classifies LLM authentication failures.
package llmauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// TokenProvider supplies a credential for a single provider attempt.
type TokenProvider interface {
	Token(context.Context) (token string, expiry time.Time, err error)
}

const (
	PermanentClass = "auth_permanent"
	TransientClass = "auth_transient"
)

// AuthPermanent indicates that credentials or IAM permissions need operator attention.
type AuthPermanent struct{ Detail string }

func (e *AuthPermanent) Error() string { return PermanentClass + ": " + e.Detail }

// AuthTransient indicates a recoverable failure obtaining a usable credential.
type AuthTransient struct{ Detail string }

func (e *AuthTransient) Error() string { return TransientClass + ": " + e.Detail }

// Class returns the bounded metric and response label for an authentication error.
func Class(err error) string {
	if _, ok := errors.AsType[*AuthPermanent](err); ok {
		return PermanentClass
	}
	if _, ok := errors.AsType[*AuthTransient](err); ok {
		return TransientClass
	}
	return ""
}

// VertexStatus classifies authentication failures and includes a redacted preview.
func VertexStatus(resp *http.Response, redact func(string) string) error {
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return nil
	}
	detail := fmt.Sprintf("Vertex rejected the credential (HTTP %d)", resp.StatusCode)
	const bodyLimit = 32 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	// Only preview complete bodies so redaction can see every echoed credential.
	if err == nil && len(body) > 0 && len(body) <= bodyLimit {
		preview := redact(string(body))
		if len(preview) > PreviewPrefixBytes {
			preview = TrimPreview(preview, PreviewPrefixBytes-len("…"))
		}
		detail += ": " + preview
	}
	return &AuthPermanent{Detail: detail}
}

// PreviewPrefixBytes is the usual provider preview limit before the ellipsis.
const PreviewPrefixBytes = 200

// TrimPreview bounds a sanitized provider diagnostic.
func TrimPreview(s string, prefixBytes int) string {
	if len(s) > prefixBytes {
		return s[:prefixBytes] + "…"
	}
	return s
}

// RejectRedirect returns a redirect response without sending another request.
func RejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

var bearerToken = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

// Bearer checks the provider's token and returns the complete authorization value.
func Bearer(ctx context.Context, provider TokenProvider) (string, error) {
	token, _, err := provider.Token(ctx)
	if err != nil {
		return "", err
	}
	if !bearerToken.MatchString(token) {
		return "", &AuthTransient{Detail: "invalid access token"}
	}
	return "Bearer " + token, nil
}

// Redactor captures the credential sent on an attempt, including common
// representations of an echoed Authorization header.
func Redactor(authorization string) func(string) string {
	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == "" {
		return func(s string) string { return s }
	}
	var pairs []string
	for _, secret := range []string{
		"Authorization: " + authorization,
		"authorization: " + authorization,
		`"Authorization":"` + authorization + `"`,
		`"authorization":"` + authorization + `"`,
		authorization,
		token,
	} {
		for _, variant := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret),
			strings.ReplaceAll(secret, "/", `\/`), strings.ReplaceAll(secret, "/", `\u002f`), strings.ReplaceAll(secret, "/", `\u002F`)} {
			pairs = append(pairs, variant, "[redacted]")
		}
	}
	return strings.NewReplacer(pairs...).Replace
}
