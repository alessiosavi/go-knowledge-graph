package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/llmauth"
)

func TestEvalAgentErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		err        error
		permanent  bool
	}{
		{name: "success"},
		{name: "permanent auth", want: "auth", err: &llmauth.AuthPermanent{}, permanent: true},
		{name: "transient auth", want: "auth", err: &llmauth.AuthTransient{}},
		{name: "deadline", want: "timeout", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded)},
		{name: "429", want: "rate_limited", err: &providerError{status: 429}},
		{name: "5xx", want: "provider_5xx", err: &providerError{status: 503}},
		{name: "4xx", want: "provider_4xx", err: &providerError{status: 404}},
		{name: "settings rejected", want: "provider_4xx", err: &generationRejection{&providerError{status: 422}}},
		{name: "status zero", want: "unreachable", err: &providerError{}},
		{name: "other", want: "other", err: errors.New("decode error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, permanent := EvalErrorClass(tc.err); got != tc.want || permanent != tc.permanent {
				t.Fatalf("class=%q permanent=%t, want %q %t", got, permanent, tc.want, tc.permanent)
			}
		})
	}
}
