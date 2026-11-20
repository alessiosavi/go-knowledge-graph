package llmauth_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
	"github.com/alessiosavi/go-knowledge-graph/internal/llmauth"
)

func TestJudgeCallerCancellationDuringMetadataFetch(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	llmauth.UseFakeGCE(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			return
		case <-release:
			fmt.Fprint(w, `{"access_token":"fake-metadata-token","expires_in":3600}`)
		}
	})
	defer close(release)

	client := judge.NewClient(config.Config{Judge: config.JudgeConfig{
		Auth: config.AuthGCP, BaseURL: "http://127.0.0.1:0",
	}})
	client.SetAttemptGate(func() error {
		t.Error("judge tried a provider call without a token")
		return errors.New("unexpected provider attempt")
	})
	client.SetBackoff(nil)
	body := ingest.CanonicalJudgeInput(ingest.FactInput{Head: "test"})
	payload := ingest.JudgePayload{Bytes: body, InputHash: fmt.Sprintf("%x", sha256.Sum256(body))}
	deadline, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(deadline)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.VerifyPayload(ctx, payload)
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("judge returned before fetching metadata: %v", err)
	case <-deadline.Done():
		t.Fatal("judge did not fetch metadata")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || judge.IsProviderStop(err) || llmauth.Class(err) != "" {
			t.Fatalf("judge error = %v, want caller cancellation without a provider stop or auth class", err)
		}
	case <-deadline.Done():
		t.Fatal("judge waited on the shared metadata fetch after caller cancellation")
	}
}
