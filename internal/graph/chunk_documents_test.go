package graph

import (
	"reflect"
	"testing"
)

func TestChunkDocumentsScoped(t *testing.T) {
	st := scopedReadTestStore(t)
	const (
		caller = "2024-03-01T00:00:00.000000000Z"
		ingest = "2025-03-01T00:00:00.000000000Z"
	)
	foldTestWrite(t, st, `
CREATE (caller:Document:ScopedReadFixture {id:'scoped-caller', domain:$domain, title:'Caller dated', source:'manual',
           document_date:$caller, version_at:$ingest}),
       (ingest:Document:ScopedReadFixture {id:'scoped-ingest', domain:$domain, title:'Ingest dated', source:'guide',
           created_at:$ingest}),
       (moved:Document:ScopedReadFixture {id:'scoped-moved', domain:'scoped-other', title:'Moved', source:'report'}),
       (caller)-[:HAS_CHUNK]->(:Chunk:ScopedReadFixture {id:'scoped-c1', doc_id:caller.id, domain:$domain}),
       (ingest)-[:HAS_CHUNK]->(:Chunk:ScopedReadFixture {id:'scoped-c2', doc_id:ingest.id, domain:$domain}),
       (moved)-[:HAS_CHUNK]->(:Chunk:ScopedReadFixture {id:'scoped-c3', doc_id:moved.id, domain:$domain}),
       (:Chunk:ScopedReadFixture {id:'scoped-c4', doc_id:'scoped-deleted', domain:$domain})`,
		map[string]any{"domain": cardTestDomain, "caller": caller, "ingest": ingest})

	got, err := st.ChunkDocuments(t.Context(), cardTestDomain, []string{
		"scoped-c1", "scoped-c2", "scoped-c3", "scoped-c4", "scoped-c1", "scoped-missing",
	})
	want := map[string]ChunkDocument{
		"scoped-c1": {DocumentID: "scoped-caller", Title: "Caller dated", Source: "manual",
			DocumentDate: new(parseTime(caller).UTC()), DocumentDateSource: new("caller")},
		"scoped-c2": {DocumentID: "scoped-ingest", Title: "Ingest dated", Source: "guide",
			DocumentDate: new(parseTime(ingest).UTC()), DocumentDateSource: new("ingest")},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ChunkDocuments = %+v, %v, want %+v", got, err, want)
	}
}
