package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestRepairNamesCLI(t *testing.T) {
	preflightCommandStore(t)
	migrationQuery(t, `UNWIND range(1, 1000) AS n
CREATE (:Entity {id: 'fill-' + toString(n), key: 'fill-' + toString(n), name: 'Zorbo ' + toString(n)})`, nil)
	migrationQuery(t, `CREATE
 (q:Entity {id: 'first', key: '@acme:quillon', name: 'Quillon\'s:', type: 'organization'}),
 (m:Entity {id: 'last', key: '#doc:mira zorbin', name: '(Mira Zorbin)', name_key: 'obsolete', type: 'person'}),
 (:Entity {id: 'clean', key: 'zorbo', name: 'Zorbo', name_key: 'zorbo', type: 'person'}),
 (:Entity {id: 'literal', key: 'kwargs', name: '**kwargs', name_key: 'kwargs', type: 'code'}),
 (:Entity {id: 'legacy', key: 'quillon\'s', name: 'Quillon\'s\'s', name_key: 'quillon\'s'}),
 (:Entity {id: 'colon', key: '@acme:kwargs', name: '**kwargs:', name_key: 'kwargs', type: 'code'}),
 (:Entity {id: 'uneven', key: 'x', name: '*x**:', name_key: 'x', type: 'code'}),
 (:Entity {id: 'period', key: 'args', name: '*args.', name_key: 'args', type: 'code'}),
 (:Entity {id: 'geo', key: 'zorbo geo', name: 'Zorbo GEO.**', name_key: 'zorbo geo'}),
 (:Entity {id: 'agent', key: 'agent', name: '**Agent', name_key: 'agent'}),
 (:Entity {id: 'operator', key: 'call operator', name: '()', name_key: '', type: 'code'}),
 (:Chunk {id: 'zorbo-chunk'})-[:MENTIONS {start: 0, end: 13}]->(m),
 (m)-[:joined {judge: 'confirmed'}]->(q)`, nil)
	const entities = `MATCH (e:Entity)
WHERE e.id IN ['first', 'last', 'clean', 'literal', 'legacy', 'colon', 'uneven', 'period', 'geo', 'agent', 'operator']
RETURN properties(e) AS props ORDER BY e.id`
	const relationships = `MATCH (a)-[r]->(b:Entity) RETURN a.id, type(r), properties(r), b.id ORDER BY a.id`
	const possessiveInTextIndex = `CALL text_search.search('entity_text_v2', 'data.name:"s"')
YIELD node WHERE node.id = 'first' RETURN node.id`
	before := migrationQuery(t, entities, nil)
	edges := migrationQuery(t, relationships, nil)
	if len(before) != 11 || len(edges) != 2 {
		t.Fatalf("fixture: %d entities, %d relationships", len(before), len(edges))
	}
	if hits := migrationQuery(t, possessiveInTextIndex, nil); len(hits) != 1 {
		t.Fatalf("dirty name should match its possessive token in the text index: %v", hits)
	}
	check := func(want string, flags ...string) {
		t.Helper()
		var out, errb bytes.Buffer
		if code := run(append([]string{"repair-names"}, flags...), &out, &errb); code != 0 || out.String() != want || errb.Len() != 0 {
			t.Errorf("repair-names %v = %d, stdout %q, stderr %q; want stdout %q", flags, code, &out, &errb, want)
		}
	}
	check("scanned=1011 would_change=7\n", "-dry-run")
	if got := migrationQuery(t, entities, nil); !reflect.DeepEqual(got, before) {
		t.Fatal("dry run modified entities")
	}
	check("scanned=1011 changed=7\n")
	after := migrationQuery(t, entities, nil)
	if len(after) != len(before) {
		t.Fatalf("repair changed entity count from %d to %d", len(before), len(after))
	}
	for i, row := range before {
		v, _ := row.Get("props")
		props := v.(map[string]any)
		switch props["id"] {
		case "first":
			props["name"], props["name_key"] = "Quillon", "quillon"
		case "last":
			props["name"], props["name_key"] = "Mira Zorbin", "mira zorbin"
		case "colon":
			props["name"] = "**kwargs"
		case "uneven":
			props["name"] = "*x**"
		case "period":
			props["name"] = "*args"
		case "geo":
			props["name"] = "Zorbo GEO"
		case "agent":
			props["name"] = "Agent"
		}
		if got, _ := after[i].Get("props"); !reflect.DeepEqual(got, props) {
			t.Errorf("entity %s after repair = %v, want %v", props["id"], got, props)
		}
	}
	if got := migrationQuery(t, relationships, nil); !reflect.DeepEqual(got, edges) {
		t.Error("repair modified relationships")
	}
	if hits := migrationQuery(t, possessiveInTextIndex, nil); len(hits) != 0 {
		t.Errorf("text index still matches the removed possessive token: %v", hits)
	}
	check("scanned=1011 changed=0\n")
}

func TestRepairNamesCLIPartialCounts(t *testing.T) {
	preflightCommandStore(t)
	// Force the second page to fail at commit after the first page succeeds.
	migrationQuery(t, `CREATE CONSTRAINT ON (n:RepairNameConflict) ASSERT n.name IS UNIQUE`, nil)
	t.Cleanup(func() {
		migrationQuery(t, `DROP CONSTRAINT ON (n:RepairNameConflict) ASSERT n.name IS UNIQUE`, nil)
	})
	migrationQuery(t, `UNWIND range(1, 999) AS n
CREATE (:Entity {id: 'fill-' + toString(n), key: 'fill-' + toString(n), name: 'Zorbo ' + toString(n)})`, nil)
	migrationQuery(t, `CREATE
 (:Entity {id: 'early', key: 'mira zorbin', name: '(Mira Zorbin)'}),
 (:Entity:RepairNameConflict {id: 'late', key: '@acme:quillon', name: '(Quillon)'}),
 (:Entity:RepairNameConflict {id: 'peer', key: 'quillon', name: 'Quillon'})`, nil)

	var stdout, stderr bytes.Buffer
	code := run([]string{"repair-names"}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 ||
		!strings.HasPrefix(stderr.String(), "gkg repair-names: graph: repair entity names:") ||
		!strings.HasSuffix(stderr.String(), " (scanned=1002 changed=1)\n") {
		t.Errorf("repair-names = %d, stdout %q, stderr %q; want a store error with scanned=1002 changed=1", code, &stdout, &stderr)
	}
	rows := migrationQuery(t, `MATCH (e:Entity) WHERE e.id IN ['early', 'late'] RETURN e.name ORDER BY e.id`, nil)
	if len(rows) != 2 || rows[0].Values[0] != "Mira Zorbin" || rows[1].Values[0] != "(Quillon)" {
		t.Errorf("entity names after failed repair = %v, want the first page's repair only", rows)
	}
}
