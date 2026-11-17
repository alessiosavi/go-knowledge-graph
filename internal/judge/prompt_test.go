package judge

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

// Updating the prompt requires a new cache namespace, recorded with its hash.
func TestPromptVersion(t *testing.T) {
	hashes := map[string]string{"1": "5b26bf935ab08067f8ee892680f6360b41935653d6f47f1cc906550774551128"}
	got := fmt.Sprintf("%x", sha256.Sum256([]byte(judgePrompt)))
	if got != hashes[PromptVersion] {
		t.Fatalf("judgePrompt hash=%s for PromptVersion=%s: bump PromptVersion and record the new prompt hash", got, PromptVersion)
	}
}
