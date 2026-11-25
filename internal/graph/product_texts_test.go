package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	textTestStore = "t1b-texts.test"
	textTestOne   = textTestStore + ":1"
	textTestTwo   = textTestStore + ":2"
)

func textTestRecord(id, title, attrs string) string {
	return strings.ReplaceAll(pushRecord(id, title, "null", attrs), pushStore, textTestStore)
}

func assertTextRows(t *testing.T, got, want [][]any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

func (f pushFixture) checkTextLedger(version any, count, edges, bytes int) {
	f.t.Helper()
	assertTextRows(f.t, f.query(`MATCH (s:ProductStore {store:$store})
RETURN s.text_version, s.text_count, s.text_edge_count, s.text_bytes`, map[string]any{"store": f.storeID}),
		[][]any{{version, int64(count), int64(edges), int64(bytes)}})
}

func (f pushFixture) textWriteState() [][][]any {
	f.t.Helper()
	params := map[string]any{"store": f.storeID}
	return [][][]any{
		f.query(`MATCH (s:ProductStore {store:$store}) RETURN properties(s)`, params),
		f.query(`MATCH (m:GkgMeta {id:'product_release'}) RETURN properties(m)`, nil),
		f.query(`MATCH (p:Product {store:$store}) RETURN id(p), properties(p) ORDER BY p.id`, params),
		f.query(`MATCH (p:Product {store:$store})-[e:HAS_FEATURE]->(n) RETURN p.id, n.key, properties(e) ORDER BY p.id,n.key,e.source_key`, params),
		f.query(`MATCH (n:ProductText {store:$store}) RETURN id(n), properties(n) ORDER BY n.id`, params),
		f.query(`MATCH (p:Product {store:$store})-[e:HAS_TEXT]->(n) RETURN p.id, n.id, properties(e), id(e) ORDER BY p.id,e.key`, params),
	}
}

func TestProductTextSnapshotPushAndWipe(t *testing.T) {
	f := newPushFixtureForStore(t, textTestStore, "code")
	const firstKey, secondKey = "surface_finish", "surface finish"
	prefix := secondKey + ": "
	value := strings.Repeat("é", (2048-len(prefix)-2)/2) + "xy"
	shared := prefix + value
	if len(shared) != 2048 {
		t.Fatalf("fixture text is %d bytes", len(shared))
	}
	first := textTestRecord(textTestOne, "one", fmt.Sprintf(`{%q:%q,"scalar":"A1"}`, firstKey, value+" extra"))
	second := textTestRecord(textTestTwo, "two", fmt.Sprintf(`{%q:%q}`, secondKey, value))
	// Changed imports also collect any unreferenced text already in the store.
	params := map[string]any{"store": textTestStore}
	f.query(`CREATE (:ProductText {id:'t1b-orphan', store:$store, text:'orphan'})`, params)
	f.snapshot(pushA, 1, "imported", first, second)
	sharedBytes := len(shared) + len(firstKey) + len(secondKey)
	f.checkTextLedger(int64(1), 2, 3, sharedBytes+len("scalar: A1")+len("scalar"))
	assertTextRows(t, f.query(`MATCH (p:Product {store:$store})-[e:HAS_TEXT]->(n:ProductText {text:$text})
RETURN p.product_id, e.key, e.truncated, properties(n) ORDER BY p.product_id`, map[string]any{"store": textTestStore, "text": shared}),
		[][]any{
			{textTestOne, firstKey, true, textNodeProperties(shared)},
			{textTestTwo, secondKey, false, textNodeProperties(shared)},
		})
	assertTextRows(t, f.query(`MATCH (n:ProductText {store:$store}) RETURN count(n)`, params), [][]any{{int64(2)}})
	before := f.textWriteState()
	f.snapshot(pushA, 2, "unchanged", first, second)
	if got := f.textWriteState()[2:]; !reflect.DeepEqual(got, before[2:]) {
		t.Fatal("unchanged import replaced products, nodes, or edges")
	}

	f.remove(3, textTestOne, "deleted")
	f.checkTextLedger(int64(1), 1, 1, len(shared)+len(secondKey))
	assertTextRows(t, f.query(`MATCH (n:ProductText {store:$store}) RETURN properties(n)`, params),
		[][]any{{textNodeProperties(shared)}})
	identical := f.textWriteState()
	f.put(4, second, "updated")
	f.checkTextLedger(int64(1), 1, 1, len(shared)+len(secondKey))
	if got := f.textWriteState()[4]; !reflect.DeepEqual(got, identical[4]) {
		t.Fatal("upsert of identical text replaced its node")
	}
	f.put(5, textTestRecord(textTestTwo, "updated", `{"café":"🪥 fresh","unused":"2 pcs"}`), "updated")
	f.checkTextLedger(int64(1), 1, 1, len("café: 🪥 fresh")+len("café"))
	f.remove(6, textTestOne, "absent")
	f.checkTextLedger(int64(1), 1, 1, len("café: 🪥 fresh")+len("café"))
	assertTextRows(t, f.query(`MATCH (n:ProductText {store:$store}) RETURN properties(n)`, params),
		[][]any{{textNodeProperties("café: 🪥 fresh")}})

	if err := f.store.DeleteProductStore(t.Context(), textTestStore); err != nil {
		t.Fatal(err)
	}
	f.checkTextLedger(nil, 0, 0, 0)
	if after := f.textWriteState(); len(after[2])+len(after[3])+len(after[4])+len(after[5]) != 0 {
		t.Fatal("wipe left product or text data behind")
	}
}

func textNodeProperties(text string) map[string]any {
	return map[string]any{"id": productobs.ProductTextID(textTestStore, text), "store": textTestStore,
		"vec_id": productobs.ProductTextVecID(text), "text": text}
}

func TestProductTextLegacyPush(t *testing.T) {
	f := newPushFixtureForStore(t, textTestStore, "code")
	first := textTestRecord(textTestOne, "legacy", `{"color":"blue"}`)
	second := textTestRecord(textTestTwo, "legacy", `{"color":"green"}`)
	f.snapshot(pushA, 1, "imported", first, second)
	params := map[string]any{"store": textTestStore}
	f.query(`MATCH (s:ProductStore {store:$store})
REMOVE s.text_version, s.text_count, s.text_edge_count, s.text_bytes`, params)
	f.query(`MATCH (n:ProductText {store:$store}) DETACH DELETE n`, params)
	f.put(2, first, "updated")
	f.checkTextLedger(nil, 1, 1, len("color: blue")+len("color"))
	assertTextRows(t, f.query(`MATCH (p:Product {store:$store})-[:HAS_TEXT]->(n)
RETURN p.product_id, n.text`, params), [][]any{{textTestOne, "color: blue"}})
	f.remove(3, textTestOne, "deleted")
	f.checkTextLedger(nil, 0, 0, 0)
	f.snapshot(pushA, 4, "imported", first, second)
	f.checkTextLedger(int64(1), 2, 2, len("color: blue")+len("color: green")+2*len("color"))
}

func TestProductTextSnapshotBatches(t *testing.T) {
	f := newPushFixtureForStore(t, textTestStore, "code")
	items := func(prefix string) string {
		attrs := make(map[string]string)
		for i := range 300 {
			attrs[fmt.Sprintf("%s%03d", prefix, i)] = "cotton"
		}
		data, err := json.Marshal(attrs)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	f.snapshot(pushA, 1, "imported", textTestRecord(textTestOne, "", items("a")), textTestRecord(textTestTwo, "", items("b")))
	f.checkTextLedger(int64(1), 600, 600, 600*(len("a000: cotton")+len("a000")))
	params := map[string]any{"store": textTestStore}
	assertTextRows(t, f.query(`MATCH (n:ProductText {store:$store}) RETURN count(n)`, params), [][]any{{int64(600)}})
	assertTextRows(t, f.query(`MATCH (:Product {store:$store})-[e:HAS_TEXT]->() RETURN count(e)`, params), [][]any{{int64(600)}})
	f.snapshot(pushA, 2, "imported", textTestRecord(textTestOne, "", `{"material":"cotton"}`))
	f.checkTextLedger(int64(1), 1, 1, len("material: cotton")+len("material"))
	assertTextRows(t, f.query(`MATCH (n:ProductText {store:$store}) RETURN n.text`, params), [][]any{{"material: cotton"}})
}

func TestProductTextImportRepairsByteCount(t *testing.T) {
	f := newPushFixtureForStore(t, textTestStore, "code")
	record := textTestRecord(textTestOne, "original", `{"color":"blue"}`)
	f.snapshot(pushA, 1, "imported", record)
	f.query(`MATCH (s:ProductStore {store:$store}) SET s.text_bytes=0`, map[string]any{"store": textTestStore})
	report := f.snapshot(pushA, 2, "unchanged", record)
	if report.TextAction != "materialized" {
		t.Errorf("text action = %q, want materialized", report.TextAction)
	}
	f.checkTextLedger(int64(1), 1, 1, len("color: blue")+len("color"))
}

func TestProductTextImportCapRefusal(t *testing.T) {
	for _, tc := range []struct {
		capName        string
		products, keys int
		padding        string
	}{
		{"text_edge_count", 391, 512, "cotton"},
		// Each key's value has a unique prefix per product. 1,058 x 31 texts
		// of length 2,048 (plus the keys) exceed 64 MiB, within the row cap.
		{"text_bytes", 1058, 31, strings.Repeat("x", 2048)},
	} {
		t.Run(tc.capName, func(t *testing.T) {
			f := newPushFixtureForStore(t, textTestStore, "code")
			f.snapshot(pushA, 1, "imported", textTestRecord(textTestOne, "original", `{"scalar":"A1"}`))
			before := f.textWriteState()
			records := make([]string, 0, tc.products)
			for i := range tc.products {
				attrs := make(map[string]string, tc.keys)
				value := tc.padding
				if tc.capName == "text_bytes" {
					value = fmt.Sprintf("%d-%s", i, value)
				}
				for k := range tc.keys {
					attrs[fmt.Sprintf("key%03d", k)] = value
				}
				data, err := json.Marshal(attrs)
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, textTestRecord(fmt.Sprintf("%s:%d", textTestStore, i), "replacement", string(data)))
			}
			// The cap refusal precedes the write transaction, so nothing is written.
			report, err := f.importRows(f.store, pushA, 2, nil, records...)
			if !errors.Is(err, ErrProductData) || !strings.Contains(err.Error(), tc.capName) || report.RejectionCount != 1 {
				t.Fatalf("cap error = %v, rejections = %+v", err, report.Rejections)
			}
			if got := f.textWriteState(); !reflect.DeepEqual(got, before) {
				t.Fatal("refused import changed the store, watermarks, products, or texts")
			}
		})
	}
}

func TestProductTextPushCapRollback(t *testing.T) {
	for _, tc := range []struct {
		capName string
		limit   int
	}{
		{"text_edge_count", 200000},
		{"text_bytes", 64 << 20},
	} {
		t.Run(tc.capName, func(t *testing.T) {
			f := newPushFixtureForStore(t, textTestStore, "code")
			original := textTestRecord(textTestOne, "original", `{"scalar":"A1"}`)
			f.snapshot(pushA, 1, "imported", original)
			// Seed the ledger boundary without populating a second large fixture.
			f.query(`MATCH (s:ProductStore {store:$store}) SET s.`+tc.capName+`=$limit`,
				map[string]any{"store": textTestStore, "limit": tc.limit})
			before := f.textWriteState()
			_, err := f.store.PushProduct(t.Context(), 2, []byte(textTestRecord(textTestOne, "changed",
				`{"scalar":"B2","material":"cotton"}`)))
			assertProductMutationError(t, err, "store_cap", nil)
			if got := f.textWriteState(); !reflect.DeepEqual(got, before) {
				t.Fatal("refused push changed the store, watermarks, products, or texts")
			}
			f.put(2, original, "updated")
		})
	}
}
