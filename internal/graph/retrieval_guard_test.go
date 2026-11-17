package graph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// The live query filters missing ids out via ch.id > $last. Supply malformed
// driver pages directly to exercise the defensive cursor guard.
func TestDoneChunkTextsRejectsEmptyLastID(t *testing.T) {
	for _, size := range []int{1, 1000} {
		for _, id := range []any{nil, ""} {
			t.Run(fmt.Sprintf("size-%d-id-%v", size, id), func(t *testing.T) {
				recs := make([]*neo4j.Record, size)
				for i := range recs {
					recs[i] = &neo4j.Record{Keys: []string{"id", "text"}, Values: []any{fmt.Sprintf("acme-c%04d", i), "Zorbo"}}
				}
				recs[size-1].Values[0] = id
				sess := &chunkPageSession{records: recs}
				st := &Store{driver: chunkPageDriver{session: sess}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				err := st.DoneChunkTexts(ctx, "", func(string, string) error { return nil })
				if err == nil || !strings.Contains(err.Error(), "empty chunk id") || sess.reads != 1 {
					t.Fatalf("err=%v reads=%d; want empty-id error before another page", err, sess.reads)
				}
			})
		}
	}
}

type chunkPageDriver struct {
	neo4j.Driver
	session *chunkPageSession
}

func (d chunkPageDriver) NewSession(context.Context, neo4j.SessionConfig) neo4j.Session {
	return d.session
}

type chunkPageSession struct {
	neo4j.Session
	records []*neo4j.Record
	reads   int
}

func (s *chunkPageSession) Run(context.Context, string, map[string]any, ...func(*neo4j.TransactionConfig)) (neo4j.Result, error) {
	s.reads++
	if s.reads > 1 {
		return nil, errors.New("scan restarted after malformed page")
	}
	return chunkPageResult{records: s.records}, nil
}

func (*chunkPageSession) Close(context.Context) error { return nil }

type chunkPageResult struct {
	neo4j.Result
	records []*neo4j.Record
}

func (r chunkPageResult) Collect(context.Context) ([]*neo4j.Record, error) { return r.records, nil }
