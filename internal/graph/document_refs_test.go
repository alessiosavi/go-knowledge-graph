package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestValidateProductRefs(t *testing.T) {
	maxID := "a:" + strings.Repeat("x", 1022)
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf("a:%02d", i)
	}
	for _, tc := range []struct {
		name     string
		in, want []string
		invalid  bool
	}{
		{name: "omitted"},
		{name: "explicit empty", in: []string{}, want: []string{}},
		{name: "ordered", in: []string{"b:3", "a:2", "a:1"}, want: []string{"a:1", "a:2", "b:3"}},
		{name: "exact A1 suffix", in: []string{"a:y ", "a:y", "a:y:z"}, want: []string{"a:y", "a:y ", "a:y:z"}},
		{name: "maximum refs", in: many[:50], want: many[:50]},
		{name: "maximum bytes", in: []string{maxID}, want: []string{maxID}},
		{name: "too many", in: many, invalid: true},
		{name: "too long in bytes", in: []string{"a:" + strings.Repeat("é", 512)}, invalid: true},
		{name: "duplicate after sorting", in: []string{"a:1", "b:2", "a:1"}, invalid: true},
		{name: "bare prefix", in: []string{"store.test"}, invalid: true},
		{name: "empty suffix", in: []string{"store.test:"}, invalid: true},
		{name: "empty store", in: []string{":x"}, invalid: true},
		{name: "invalid store", in: []string{"A:1"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.in)
			got, err := ValidateProductRefs(tc.in)
			if tc.invalid {
				var refsErr *DocumentRefsError
				if !errors.As(err, &refsErr) || refsErr.Code != "invalid_products" {
					t.Fatalf("invalid products error = %v", err)
				}
			} else if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("refs = %#v, %v, want %#v", got, err, tc.want)
			}
			if !reflect.DeepEqual(before, tc.in) {
				t.Errorf("validation mutated input: %q", tc.in)
			}
		})
	}
}

func TestDocumentProductRefAnchorSurvivesProductWrites(t *testing.T) {
	f := newPushFixture(t)
	ctx := t.Context()
	in := DocumentInput{Kind: "text", Source: t.Name(), Hash: "page", ProductRefs: []string{pushOne}}
	res, err := f.store.UpsertDocument(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"doc": res.ID, "id": productobs.ProductID(pushStore, pushOne)}
	t.Cleanup(func() {
		for _, query := range []string{
			`MATCH (d:Document {id:$doc}) DETACH DELETE d`,
			`MATCH (a:ProductRef {id:$id}) DETACH DELETE a`,
		} {
			if err := f.store.runWrite(context.Background(), query, params); err != nil {
				t.Error(err)
			}
		}
	})
	const anchorQuery = `
MATCH (:Document {id:$doc})-[:REFS]->(a:ProductRef)
WHERE a.id = $id RETURN id(a), a.store, a.product_id`
	before := f.query(anchorQuery, params)
	if len(before) != 1 || before[0][1] != pushStore || before[0][2] != pushOne {
		t.Fatalf("anchor before product arrival = %v", before)
	}
	a := pushRecord(pushOne, "A", "null", `{"scalar":"A1"}`)
	b := pushRecord(pushOne, "B", "null", `{"scalar":"A2"}`)
	c := pushRecord(pushOne, "C", "null", `{"scalar":"A3"}`)
	f.snapshot(pushA, 1, "imported", a)
	f.snapshot(pushA, 2, "imported", b)
	f.put(3, c, "updated")
	if after := f.query(anchorQuery, params); !reflect.DeepEqual(after, before) {
		t.Errorf("anchor after re-import and push = %v, want %v", after, before)
	}
	if got := f.query("MATCH (p:Product {id:$id}) RETURN p.title", params); !reflect.DeepEqual(got, [][]any{{"C"}}) {
		t.Errorf("live same-ID product = %v", got)
	}
	if err := f.store.DeleteDocument(ctx, res.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.query(`
MATCH (a:ProductRef {id:$id}) OPTIONAL MATCH (:Document)-[r:REFS]->(a)
RETURN id(a), count(r)`, params); !reflect.DeepEqual(got, [][]any{{before[0][0], int64(0)}}) {
		t.Errorf("orphan anchor after document delete = %v", got)
	}
}

func TestFinishDocumentRefRetirementRollsBack(t *testing.T) {
	f := newPushFixture(t)
	ctx := t.Context()
	in := DocumentInput{Kind: "text", Source: t.Name(), Hash: "old", ProductRefs: []string{pushOne}}
	old, err := f.store.UpsertDocument(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetStatus(ctx, old.ID, StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	in.Hash, in.ProductRefs = "new", nil
	replacement, err := f.store.UpsertDocument(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetStatus(ctx, replacement.ID, StatusMerged, ""); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"old": old.ID, "new": replacement.ID, "anchor": productobs.ProductID(pushStore, pushOne)}
	t.Cleanup(func() {
		if err := f.store.runWrite(context.Background(), `
MATCH (d:Document) WHERE d.id IN [$old,$new] DETACH DELETE d
WITH DISTINCT 1 AS x MATCH (a:ProductRef {id:$anchor}) DETACH DELETE a`, params); err != nil {
			t.Error(err)
		}
	})
	const state = `
MATCH (o:Document {id:$old}), (n:Document {id:$new})
RETURN o.status, n.status, EXISTS { MATCH (o)-[:REFS]->(:ProductRef) }`
	before, after := [][]any{{"done", "merged", true}}, [][]any{{"superseded", "done", false}}
	injected := errors.New("finish interrupted before commit")
	err = f.store.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		retired, err := txFinishDocument(ctx, tx, replacement.ID, true, "")
		if err != nil || !slices.Equal(retired, []string{old.ID}) {
			t.Fatalf("finish tx = %v, %v", retired, err)
		}
		rows, err := txRows(ctx, tx, state, params)
		if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].Values, after[0]) {
			t.Fatalf("inside finish tx = %v, %v, want %v", rows, err, after)
		}
		if got := f.query(state, params); !reflect.DeepEqual(got, before) {
			t.Errorf("concurrent reader saw provisional retirement: %v", got)
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("transaction = %v, want injected failure", err)
	}
	if got := f.query(state, params); !reflect.DeepEqual(got, before) {
		t.Errorf("after rollback = %v, want %v", got, before)
	}
	if _, err := f.store.FinishDocument(ctx, replacement.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.query(state, params); !reflect.DeepEqual(got, after) {
		t.Errorf("after commit = %v, want %v", got, after)
	}
}
