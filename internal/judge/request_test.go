package judge_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestV2Fingerprint(t *testing.T) {
	defs := map[string]map[string]string{
		"general":       {"uses": "general uses", "about": "general about"},
		"documentation": {"uses": "documentation uses"},
	}
	prompt, model, schema := "prompt", "model", "schema"
	want := judge.Fingerprint(prompt, defs, model, schema)
	if decoded, err := hex.DecodeString(want); err != nil || len(decoded) != 8 {
		t.Fatalf("fingerprint = %q, want 16 hex characters", want)
	}
	reordered := map[string]map[string]string{
		"documentation": {"uses": "documentation uses"},
		"general":       {"about": "general about", "uses": "general uses"},
	}
	if got := judge.Fingerprint(prompt, reordered, model, schema); got != want {
		t.Errorf("reordered definition set got %s, want %s", got, want)
	}
	req := []byte("fact")
	wantKey := judge.RequestKeyV2(req, want)
	for name, changed := range map[string]string{
		"prompt": judge.Fingerprint(prompt+"\n", defs, model, schema),
		"definition": judge.Fingerprint(prompt, map[string]map[string]string{
			"general":       {"uses": "general uses", "about": "revised about"},
			"documentation": {"uses": "documentation uses"},
		}, model, schema),
		"preset": judge.Fingerprint(prompt, map[string]map[string]string{
			"other": defs["general"], "documentation": defs["documentation"],
		}, model, schema),
		"relation": judge.Fingerprint(prompt, map[string]map[string]string{
			"general":       {"uses": "general uses", "describes": "general about"},
			"documentation": defs["documentation"],
		}, model, schema),
		"model":  judge.Fingerprint(prompt, defs, model+"2", schema),
		"schema": judge.Fingerprint(prompt, defs, model, schema+"2"),
	} {
		if changed == want || judge.RequestKeyV2(req, changed) == wantKey {
			t.Errorf("changing %s left the fingerprint or key unchanged", name)
		}
	}
}

func TestV2VerifyRequestWireAndCache(t *testing.T) {
	var calls atomic.Int32
	requestBytes := []byte(" { \"title\": \"è <>&\", \"rel\": \"uses\" }\n")
	definitions := map[string]map[string]string{"general": {"uses": "original"}}
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
			t.Errorf("unexpected messages %+v", body.Messages)
			return
		}
		if body.Messages[0].Content != judge.PromptV2 {
			t.Error("v2 system prompt changed in transit")
		}
		want := requestBytes
		if calls.Add(1) == 2 {
			want = append([]byte(" "), want...)
		}
		if got := body.Messages[1].Content; got != string(want) {
			t.Errorf("user content = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed","reason":"supported"}`)))
	})
	req := judge.Request{
		Profile:      judge.NewV2Profile(j.Model(), definitions),
		RequestBytes: requestBytes,
		DocID:        "doc-one",
	}
	wantKey := fmt.Sprintf("%x", sha256.Sum256(append(slices.Clone(requestBytes), req.Profile.Fingerprint...)))
	if got := judge.RequestKeyV2(req.RequestBytes, req.Profile.Fingerprint); got != wantKey {
		t.Errorf("v2 key = %s, want %s", got, wantKey)
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip judge.Request
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Profile != req.Profile || roundTrip.DocID != req.DocID || !slices.Equal(roundTrip.RequestBytes, requestBytes) {
		t.Fatalf("envelope round trip = %+v, want %+v", roundTrip, req)
	}

	st := &memoryVerdicts{values: map[string]graph.JudgeVerdict{}}
	cached := judge.NewCached(j, st, j.Model())
	if v, err := cached.VerifyRequest(t.Context(), roundTrip, definitions); err != nil || v.Status != "confirmed" {
		t.Fatalf("miss = %+v, %v", v, err)
	}
	if saved, ok := st.values[wantKey]; !ok || saved.PromptVersion != req.Profile.Fingerprint || saved.Model != j.Model() || saved.DocID != "doc-one" {
		t.Errorf("v2 cache entry = %+v, present = %v", saved, ok)
	}
	roundTrip.DocID = "doc-two"
	if v, err := cached.VerifyRequest(t.Context(), roundTrip, definitions); err != nil || v.Status != "confirmed" || calls.Load() != 1 || st.puts != 1 {
		t.Fatalf("hit = %+v, %v, calls %d, writes %d", v, err, calls.Load(), st.puts)
	}
	roundTrip.RequestBytes = append([]byte(" "), requestBytes...)
	if v, err := cached.VerifyRequest(t.Context(), roundTrip, definitions); err != nil || v.Status != "confirmed" || calls.Load() != 2 || st.puts != 2 {
		t.Fatalf("second miss = %+v, %v, calls %d, writes %d", v, err, calls.Load(), st.puts)
	}
	roundTrip.RequestBytes = requestBytes
	revised := map[string]map[string]string{"general": {"uses": "revised"}}
	roundTrip.Profile = judge.NewV2Profile(j.Model(), revised)
	if v, err := cached.VerifyRequest(t.Context(), roundTrip, revised); err != nil || v.Status != "confirmed" || calls.Load() != 3 || st.puts != 3 {
		t.Fatalf("profile miss = %+v, %v, calls %d, writes %d", v, err, calls.Load(), st.puts)
	}
}

func TestV2VerifyRequestRejectsInvalidEnvelope(t *testing.T) {
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid envelope reached the provider")
		http.Error(w, "unexpected request", http.StatusBadRequest)
	})
	good := judge.NewV2Profile(j.Model(), nil)
	wrongVersion := good
	wrongVersion.Version = "unknown"
	wrongFingerprint := good
	wrongFingerprint.Fingerprint = "not hexadecimal"
	for _, req := range []judge.Request{
		{Profile: wrongVersion, RequestBytes: []byte("{}")},
		{Profile: wrongFingerprint, RequestBytes: []byte("{}")},
		{Profile: judge.NewV2Profile("other model", nil), RequestBytes: []byte("{}")},
		{Profile: good, RequestBytes: []byte{0xff}},
		{Profile: good},
	} {
		if _, err := j.VerifyRequest(t.Context(), req, nil); err == nil {
			t.Errorf("invalid envelope accepted: %+v", req)
		}
	}
}

func TestV2VerifyRequestRejectsStaleProfile(t *testing.T) {
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("stale request reached the provider")
		http.Error(w, "unexpected request", http.StatusBadRequest)
	})
	definitions := map[string]map[string]string{"general": {"uses": "original definition"}}
	planned := judge.NewV2Profile(j.Model(), definitions)
	editedDefinitions := map[string]map[string]string{"general": {"uses": "edited definition"}}
	oldPrompt := judge.PromptV2 + "\n"
	stalePrompt := planned
	stalePrompt.PromptDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(oldPrompt)))
	stalePrompt.Fingerprint = judge.Fingerprint(oldPrompt, definitions, j.Model(), "2")
	wrongFingerprint := planned
	wrongFingerprint.Fingerprint = stalePrompt.Fingerprint
	for _, tc := range []struct {
		name, errorText string
		profile         judge.Profile
		definitions     map[string]map[string]string
	}{
		{"definition edit", "definitions digest", planned, editedDefinitions},
		{"prompt edit", "prompt digest", stalePrompt, definitions},
		{"fingerprint mismatch", "fingerprint", wrongFingerprint, definitions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := judge.Request{Profile: tc.profile, RequestBytes: []byte(`{"relation_definition":"original definition"}`)}
			key := judge.RequestKeyV2(req.RequestBytes, tc.profile.Fingerprint)
			store := &memoryVerdicts{values: map[string]graph.JudgeVerdict{key: {Status: "confirmed"}}}
			cached := judge.NewCached(j, store, j.Model())
			if _, err := cached.VerifyRequest(t.Context(), req, tc.definitions); err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Errorf("cache returned %v, want %q validation error", err, tc.errorText)
			}
			if _, err := j.VerifyRequest(t.Context(), req, tc.definitions); err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Errorf("client returned %v, want %q validation error", err, tc.errorText)
			}
		})
	}
}

func TestV2CacheErrors(t *testing.T) {
	var calls atomic.Int32
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
	})
	req := judge.Request{Profile: judge.NewV2Profile(j.Model(), nil), RequestBytes: []byte("{}")}
	failure := errors.New("cache unavailable")
	st := &memoryVerdicts{readErr: failure}
	c := judge.NewCached(j, st, j.Model())
	if _, err := c.VerifyRequest(t.Context(), req, nil); !errors.Is(err, failure) || calls.Load() != 0 {
		t.Fatalf("lookup failure = %v, calls %d", err, calls.Load())
	}
	st.readErr, st.writeErr = nil, failure
	if v, err := c.VerifyRequest(t.Context(), req, nil); !errors.Is(err, failure) || v.Status != "confirmed" || calls.Load() != 1 {
		t.Fatalf("write failure = %+v, %v, calls %d", v, err, calls.Load())
	}
}
