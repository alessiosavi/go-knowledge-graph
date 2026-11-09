package api

import (
	"strings"
	"testing"
)

func TestCypherGuardAllowsReads(t *testing.T) {
	ok := []string{
		"MATCH (n) RETURN n",
		"MATCH (n:Entity) RETURN n LIMIT 5",
		"MATCH (n)-[r]->(m) RETURN n, r, m",
		"CALL vector_search.search('entity_vec', 10, $vec) YIELD node RETURN node",
		"CALL text_search.search_all('entity_text', $q) YIELD node, score RETURN node, score",
		"SHOW INDEX INFO",
	}
	for _, q := range ok {
		if _, err := CheckReadOnly(q); err != nil {
			t.Errorf("CheckReadOnly(%q) = %v, want allowed", q, err)
		}
	}
}

func TestCypherGuardDeniesWrites(t *testing.T) {
	denied := []string{
		"CREATE (n:Entity)",
		"MERGE (n:Entity {key: 'x'})",
		"MATCH (n) SET n.x = 1",
		"MATCH (n) DELETE n",
		"MATCH (n) DETACH DELETE n",
		"MATCH (n) REMOVE n.x",
		"DROP CONSTRAINT ON (n:Entity) ASSERT n.key IS UNIQUE",
		"ALTER something",
		"LOAD CSV FROM 'http://evil' AS row RETURN row",
		"LOAD PARQUET FROM 'file' AS row RETURN row",
		"MATCH (n) RETURN n; DELETE n",
		"MATCH (n) RETURN n;",
	}
	for _, q := range denied {
		if _, err := CheckReadOnly(q); err == nil {
			t.Errorf("CheckReadOnly(%q) = nil, want denied", q)
		}
	}
}

func TestCypherGuardDeniesNonAllowlistedCalls(t *testing.T) {
	denied := []string{
		"CALL embeddings.node_sentence('x')",
		"CALL mg.procedures() YIELD name RETURN name",
		"CALL custom.proc()",
	}
	for _, q := range denied {
		if _, err := CheckReadOnly(q); err == nil {
			t.Errorf("CheckReadOnly(%q) = nil, want denied", q)
		}
	}
}

func TestCypherGuardIgnoresKeywordsInStrings(t *testing.T) {
	q := `MATCH (n:Entity) WHERE n.name = 'create the widget' RETURN n`
	if _, err := CheckReadOnly(q); err != nil {
		t.Errorf("keywords inside string literals must not deny: %v", err)
	}
}

func TestCypherGuardIgnoresKeywordsInComments(t *testing.T) {
	queries := []string{
		"MATCH (n) // CREATE the widget\nRETURN n",
		"MATCH (n) /* DELETE n */ RETURN n",
	}
	for _, q := range queries {
		if _, err := CheckReadOnly(q); err != nil {
			t.Errorf("keywords inside comments must not deny: %v", err)
		}
	}
}

// TestCypherGuardDeniesCommentGluedWrites pins the comment-as-whitespace
// rule: a comment between a keyword and its operand must not glue tokens
// into identifiers the deny scan misses ("DELETE/*c*/n" is "DELETE n" for
// Memgraph, so it must be denied here).
func TestCypherGuardDeniesCommentGluedWrites(t *testing.T) {
	denied := []string{
		"MATCH (n:Entity) DELETE/*c*/n",
		"MATCH (n:Entity) SET/*c*/n.x = 1",
		"MATCH (n:Entity) DETACH/*c*/DELETE n",
		"CREATE/*c*/(n:Entity)",
		"LOAD/*c*/CSV FROM 'http://evil' AS row RETURN row",
		"CALL/*c*/mg.procedures() YIELD name RETURN name",
		"CALL/*c*/example_c.write_procedure()",
	}
	for _, q := range denied {
		if _, err := CheckReadOnly(q); err == nil {
			t.Errorf("CheckReadOnly(%q) = nil, want denied", q)
		}
	}
	// A comment between CALL and an allowlisted procedure is whitespace:
	// the procedure stays allowed.
	if _, err := CheckReadOnly("CALL/*c*/vector_search.search('entity_vec', 10, $vec) YIELD node RETURN node"); err != nil {
		t.Errorf("comment between CALL and an allowed procedure must pass: %v", err)
	}
}

// TestCypherGuardDeniesUnion pins the v0 ban: a Cypher UNION applies its
// LIMIT per branch, so the post-union row cap cannot be enforced; both the
// uncapped and the oversized-early-LIMIT shapes must be rejected.
func TestCypherGuardDeniesUnion(t *testing.T) {
	denied := []string{
		"MATCH (d:Document) RETURN d UNION MATCH (e:Entity) RETURN e",
		"MATCH (d:Document) RETURN d LIMIT 1000000000 UNION MATCH (e:Entity) RETURN e LIMIT 5",
		"MATCH (d:Document) RETURN d UNION ALL MATCH (e:Entity) RETURN e",
	}
	for _, q := range denied {
		if _, err := CheckReadOnly(q); err == nil {
			t.Errorf("CheckReadOnly(%q) = nil, want denied (UNION cannot be capped)", q)
		}
	}
	// "union" inside literals and quoted identifiers is not a UNION clause.
	allowed := []string{
		"MATCH (n) WHERE n.name = 'union all' RETURN n",
		"MATCH (n) RETURN n AS `union`",
	}
	for _, q := range allowed {
		if _, err := CheckReadOnly(q); err != nil {
			t.Errorf("CheckReadOnly(%q) = %v, want allowed", q, err)
		}
	}
}

// TestCypherGuardLimitClause pins the row-cap interaction: only a real
// trailing LIMIT clause suppresses the injected cap; LIMIT-looking
// identifiers/properties do not, LIMIT arithmetic is rejected because it
// would defeat the cap, and more than one clause-like LIMIT cannot be
// reasoned about (rejected).
func TestCypherGuardLimitClause(t *testing.T) {
	// A property named "limit" must not suppress the injected row cap.
	rewritten, err := CheckReadOnly("MATCH (n:Entity) WHERE n.limit IS NULL RETURN n")
	if err != nil {
		t.Fatalf("property named limit: %v", err)
	}
	if !strings.Contains(strings.ToUpper(rewritten), "LIMIT $CYPHER_ROWS") {
		t.Errorf("rewritten = %q, want the injected cap despite n.limit", rewritten)
	}
	if n, ok := LimitClause(rewritten); ok {
		t.Errorf("LimitClause = %d, %v; the injected parameter is not numeric", n, ok)
	}

	// LIMIT expressions cannot be capped; they are rejected.
	for _, q := range []string{
		"MATCH (n) RETURN n LIMIT 2 * 1000",
		"MATCH (n) RETURN n LIMIT $cypher_rows * 1000",
		"MATCH (n) RETURN n LIMIT n.count + 10",
	} {
		if _, err := CheckReadOnly(q); err == nil {
			t.Errorf("CheckReadOnly(%q) = nil, want the LIMIT expression rejected", q)
		}
	}

	// A plain integer and the cap parameter itself pass unchanged.
	for _, q := range []string{
		"MATCH (n) RETURN n LIMIT 3",
		"MATCH (n) RETURN n LIMIT $cypher_rows",
	} {
		rewritten, err := CheckReadOnly(q)
		if err != nil {
			t.Fatalf("CheckReadOnly(%q): %v", q, err)
		}
		if strings.Count(strings.ToUpper(rewritten), "LIMIT") != 1 {
			t.Errorf("rewritten = %q, must not gain a second LIMIT", rewritten)
		}
	}

	// More than one clause-like LIMIT cannot be reasoned about (UNION is
	// banned, so any second clause is invalid Cypher): reject rather than
	// guess which one bounds the result.
	if _, err := CheckReadOnly("MATCH (n) RETURN n LIMIT 5 LIMIT 1000"); err == nil {
		t.Error("two clause-like LIMITs: want a rejection")
	}
	if n, ok := LimitClause("MATCH (n) RETURN n LIMIT 3"); !ok || n != 3 {
		t.Errorf("LimitClause = %d, %v; want 3, true", n, ok)
	}

	// An identifier named limit (bare return, alias) is not a clause; the
	// cap is injected.
	for _, q := range []string{
		"MATCH (limit:Entity) RETURN limit",
		"MATCH (n) RETURN 1 AS limit",
	} {
		rewritten, err := CheckReadOnly(q)
		if err != nil {
			t.Fatalf("identifier named limit %q: %v", q, err)
		}
		if !strings.Contains(strings.ToUpper(rewritten), "LIMIT $CYPHER_ROWS") {
			t.Errorf("rewritten = %q, want the injected cap", rewritten)
		}
	}
}

func TestCypherGuardFindsExplicitLimitOutsideComments(t *testing.T) {
	if n, ok := LimitClause("MATCH (n) RETURN n LIMIT 42"); !ok || n != 42 {
		t.Errorf("LimitClause = %d, %v; want 42, true", n, ok)
	}
	if n, ok := LimitClause("MATCH (n) RETURN n LIMIT 42 // LIMIT 7"); !ok || n != 42 {
		t.Errorf("LimitClause = %d, %v; want the code-level 42", n, ok)
	}
	if _, ok := LimitClause("MATCH (n) RETURN n LIMIT $cypher_rows"); ok {
		t.Error("a parameter limit is not an explicit numeric limit")
	}
	if _, ok := LimitClause("MATCH (n) RETURN n"); ok {
		t.Error("no LIMIT must report false")
	}
}

func TestCypherGuardInjectsLimit(t *testing.T) {
	rewritten, err := CheckReadOnly("MATCH (n) RETURN n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(rewritten), "LIMIT $CYPHER_ROWS") {
		t.Errorf("rewritten = %q, want an injected LIMIT $cypher_rows", rewritten)
	}
	withLimit, err := CheckReadOnly("MATCH (n) RETURN n LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.ToUpper(withLimit), "LIMIT") != 1 {
		t.Errorf("rewritten = %q, must keep the user's LIMIT", withLimit)
	}
}

func TestCypherGuardRejectsEmpty(t *testing.T) {
	if _, err := CheckReadOnly("   "); err == nil {
		t.Fatal("empty query: want error")
	}
}
