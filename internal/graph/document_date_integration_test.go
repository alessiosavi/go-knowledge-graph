package graph_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

const (
	callerDate = "2024-03-01T00:00:00.000000000Z"
	oldVersion = "2025-03-01T00:00:00.000000000Z"
)

func dateUpsert(t *testing.T, st *graph.Store, in graph.DocumentInput) graph.UpsertResult {
	t.Helper()
	res, err := st.UpsertDocument(t.Context(), in)
	if err != nil || res.Noop {
		t.Fatalf("UpsertDocument = %+v, %v, want a new admission", res, err)
	}
	return res
}

func checkDocumentDate(t *testing.T, d graph.Document, want, source string) {
	t.Helper()
	const layout = "2006-01-02T15:04:05.000000000Z"
	if d.DocumentDate == nil || d.DocumentDate.Format(layout) != want ||
		d.DocumentDateSource == nil || *d.DocumentDateSource != source {
		t.Fatalf("document date = %v (source %v), want %s (%s)", d.DocumentDate, d.DocumentDateSource, want, source)
	}
}

func storedDocumentDate(t *testing.T, st *graph.Store, id string) (raw, version string) {
	t.Helper()
	q, err := st.RunReadQuery(t.Context(), `
MATCH (d:Document {id:$id}) RETURN d.document_date AS raw, d.version_at AS version`,
		map[string]any{"id": id}, 1)
	if err != nil || len(q.Rows) != 1 {
		t.Fatalf("read stored dates for %s: %+v, %v", id, q, err)
	}
	if value := q.Rows[0]["raw"]; value != nil {
		var ok bool
		raw, ok = value.(string)
		if !ok || raw == "" {
			t.Fatalf("raw document date = %#v, want null or a non-empty date", value)
		}
	}
	version, _ = q.Rows[0]["version"].(string)
	return raw, version
}

func TestDocumentDateStoredAndListed(t *testing.T) {
	st := graphtest.Store(t)
	in := graph.DocumentInput{Kind: "text", Source: uniqueSource(t), Hash: "dated", DocumentDate: callerDate}
	res := dateUpsert(t, st, in)
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), res.ID) })
	raw, version := storedDocumentDate(t, st, res.ID)
	if raw != callerDate || version == callerDate || version == "" {
		t.Fatalf("stored raw=%q, version=%q, want caller date and separate version clock", raw, version)
	}
	d, err := st.Document(t.Context(), res.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkDocumentDate(t, *d, callerDate, "caller")
	docs, err := st.DocumentsIn(t.Context(), graph.StatusReceived, "", 1)
	if err != nil || len(docs) != 1 || docs[0].ID != res.ID {
		t.Fatalf("DocumentsIn = %+v, %v, want %s", docs, err, res.ID)
	}
	checkDocumentDate(t, docs[0], callerDate, "caller")
}

func TestDocumentDateFallbackOnNewVersion(t *testing.T) {
	st := graphtest.Store(t)
	in := graph.DocumentInput{Kind: "text", Source: uniqueSource(t), Hash: "dated", DocumentDate: callerDate}
	first := dateUpsert(t, st, in)
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), first.ID) })
	if err := st.SetStatus(t.Context(), first.ID, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	in.Hash, in.DocumentDate = "undated", ""
	next := dateUpsert(t, st, in)
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), next.ID) })
	if next.PreviousID != first.ID {
		t.Fatalf("PreviousID = %q, want %q", next.PreviousID, first.ID)
	}
	// Distinguish the version clock from the creation clock.
	uri := os.Getenv(graphtest.URIEnv)
	rawRun(t, uri, fmt.Sprintf("MATCH (d:Document {id:'%s'}) SET d.version_at = '%s'", next.ID, oldVersion))
	raw, version := storedDocumentDate(t, st, next.ID)
	d, err := st.Document(t.Context(), next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw != "" || version != oldVersion {
		t.Fatalf("stored raw=%q, version=%q, want an unset caller date and %s", raw, version, oldVersion)
	}
	checkDocumentDate(t, *d, oldVersion, "ingest")
	// Legacy documents with no version clock fall back to their creation time.
	rawRun(t, uri, fmt.Sprintf("MATCH (d:Document {id:'%s'}) REMOVE d.version_at", next.ID))
	d, err = st.Document(t.Context(), next.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkDocumentDate(t, *d, d.CreatedAt.Format("2006-01-02T15:04:05.000000000Z"), "ingest")
}

func TestDocumentDateLiveForceKeepsDateAndVersion(t *testing.T) {
	st := graphtest.Store(t)
	in := graph.DocumentInput{Kind: "text", Source: uniqueSource(t), Hash: "dated", DocumentDate: callerDate}
	first := dateUpsert(t, st, in)
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), first.ID) })
	if err := st.SetStatus(t.Context(), first.ID, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	_, before := storedDocumentDate(t, st, first.ID)
	in.DocumentDate, in.Force = "", true
	next := dateUpsert(t, st, in)
	raw, after := storedDocumentDate(t, st, next.ID)
	if next.ID != first.ID || raw != callerDate || after != before {
		t.Fatalf("forced document = %s, raw=%q version=%q, want %s, %q, %q", next.ID, raw, after, first.ID, callerDate, before)
	}
	in.DocumentDate = oldVersion
	dateUpsert(t, st, in)
	raw, after = storedDocumentDate(t, st, next.ID)
	if raw != oldVersion || after != before {
		t.Fatalf("forced replacement raw=%q version=%q, want %q, %q", raw, after, oldVersion, before)
	}
}

func TestDocumentDateFailedRetryAndRevival(t *testing.T) {
	st := graphtest.Store(t)
	for _, status := range []graph.Status{graph.StatusFailed, graph.StatusSuperseded} {
		for name, date := range map[string]string{"dated": callerDate, "undated": ""} {
			t.Run(string(status)+"/"+name, func(t *testing.T) {
				in := graph.DocumentInput{Kind: "text", Source: uniqueSource(t), Hash: "original", DocumentDate: date}
				first := dateUpsert(t, st, in)
				t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), first.ID) })
				if err := st.SetStatus(t.Context(), first.ID, status, ""); err != nil {
					t.Fatal(err)
				}
				uri := os.Getenv(graphtest.URIEnv)
				rawRun(t, uri, fmt.Sprintf("MATCH (d:Document {id:'%s'}) SET d.version_at = '%s'", first.ID, oldVersion))
				in.DocumentDate = ""
				next := dateUpsert(t, st, in)
				raw, refreshed := storedDocumentDate(t, st, next.ID)
				if next.ID != first.ID || raw != date || refreshed <= oldVersion {
					t.Fatalf("retry = %s raw=%q version=%q, want %s, raw=%q and refreshed version", next.ID, raw, refreshed, first.ID, date)
				}
				d, err := st.Document(t.Context(), next.ID)
				if err != nil {
					t.Fatal(err)
				}
				if raw == "" {
					checkDocumentDate(t, *d, refreshed, "ingest")
				} else {
					checkDocumentDate(t, *d, raw, "caller")
				}
			})
		}
	}
}
