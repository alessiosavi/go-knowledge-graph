package extract

import (
	"context"
	"strings"
	"sync"
)

// Mock is the deterministic test provider: per chunk it emits the first and
// last word as mentions (both under the first requested entity label) and
// one relation candidate between them at Score. It never fails unless Err
// is set.
type Mock struct {
	Score float64
	Err   error

	mu         sync.Mutex
	calls      int
	lastChunks int
}

// Name implements Provider.
func (m *Mock) Name() string { return "mock" }

// Version implements Provider.
func (m *Mock) Version() string { return "mock:1" }

// Extract implements Provider.
func (m *Mock) Extract(_ context.Context, req Request) ([]ChunkResult, error) {
	m.mu.Lock()
	m.calls++
	m.lastChunks = len(req.Chunks)
	err := m.Err
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}

	score := m.Score
	if score == 0 {
		score = 0.9
	}
	label := "entity"
	if len(req.Entities) > 0 && req.Entities[0].Name != "" {
		label = req.Entities[0].Name
	}
	rel := "relates"
	if len(req.Relations) > 0 && req.Relations[0].Name != "" {
		rel = req.Relations[0].Name
	}

	out := make([]ChunkResult, 0, len(req.Chunks))
	for _, c := range req.Chunks {
		res := ChunkResult{ChunkID: c.ID}
		words := strings.Fields(c.Text)
		if len(words) > 0 {
			first := words[0]
			start := strings.Index(c.Text, first)
			res.Mentions = append(res.Mentions, Mention{
				Label: label, Text: first, Start: start, End: start + len(first), Score: score,
			})
			last := words[len(words)-1]
			if last != first || len(words) > 1 {
				lstart := strings.LastIndex(c.Text, last)
				res.Mentions = append(res.Mentions, Mention{
					Label: label, Text: last, Start: lstart, End: lstart + len(last), Score: score,
				})
			}
		}
		if len(res.Mentions) >= 2 {
			res.Candidates = append(res.Candidates, Candidate{Label: rel, Head: 0, Tail: 1, Score: score})
		}
		out = append(out, res)
	}
	return out, nil
}

// Calls returns how many Extract invocations happened.
func (m *Mock) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// LastChunkCount returns the chunk count seen by the last Extract call.
func (m *Mock) LastChunkCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastChunks
}
