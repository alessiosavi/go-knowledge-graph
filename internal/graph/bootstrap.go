package graph

import (
	"context"
	"fmt"
	"strings"
)

// uniqueConstraints is the schema gkg bootstraps on every serve start and
// via `gkg migrate`.
var uniqueConstraints = []struct {
	Label string
	Prop  string
}{
	{"Document", "id"},
	{"Chunk", "id"},
	{"Entity", "key"},
	{"GkgMeta", "id"},
}

// secondaryIndexes are non-unique label indexes bootstrapped for query
// performance (document identity, chunk exact-key lookups, entity id
// lookups from the embedding write path).
var secondaryIndexes = []struct {
	Label string
	Props []string
}{
	{"Document", []string{"source_kind", "source"}},
	{"Chunk", []string{"exact_key"}},
	{"Entity", []string{"id"}},
}

// textIndexes are the Tantivy text indexes backing search.Query.
var textIndexes = []struct {
	Name  string
	Label string
	Props []string
}{
	{"entity_text", "Entity", []string{"name", "aliases"}},
	{"chunk_text", "Chunk", []string{"text"}},
}

// Bootstrap creates the schema idempotently. Memgraph has no documented
// IF NOT EXISTS for constraints, so existence is checked first and an
// already-exists error from a concurrent creator is tolerated.
func (s *Store) Bootstrap(ctx context.Context) error {
	// Pre-constraint cleanup: concurrent writers could have created
	// duplicate :GkgMeta marker nodes before the unique constraint existed;
	// the constraint DDL would then fail on the dirty data.
	if err := s.dedupeGkgMeta(ctx); err != nil {
		return err
	}
	for _, c := range uniqueConstraints {
		ok, err := s.constraintExists(ctx, c.Label, c.Prop)
		if err != nil {
			return fmt.Errorf("graph: inspect constraints: %w", err)
		}
		if ok {
			continue
		}
		ddl := fmt.Sprintf("CREATE CONSTRAINT ON (n:%s) ASSERT n.%s IS UNIQUE", c.Label, c.Prop)
		if err := s.runAuto(ctx, ddl, nil); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				continue
			}
			return fmt.Errorf("graph: create constraint (%s).%s: %w", c.Label, c.Prop, err)
		}
	}
	if err := s.ensureIndexes(ctx); err != nil {
		return err
	}
	if err := s.EnsureTextIndexes(ctx); err != nil {
		return err
	}
	return s.migrateExtractionSchema(ctx)
}

// dedupeGkgMeta removes duplicate :GkgMeta nodes (same id) so the unique
// constraint can be created on databases written before it existed. The
// surviving node is arbitrary; the marker is a single-key value.
func (s *Store) dedupeGkgMeta(ctx context.Context) error {
	err := s.runAuto(ctx, `
MATCH (m:GkgMeta)
WHERE m.id IS NOT NULL
WITH m.id AS id, collect(m) AS nodes
WHERE size(nodes) > 1
UNWIND tail(nodes) AS dup
DELETE dup`, nil)
	if err != nil {
		return fmt.Errorf("graph: dedupe gkg meta: %w", err)
	}
	return nil
}

// EnsureTextIndexes creates the Tantivy text indexes idempotently (also
// invoked by Bootstrap). Text index DDL is schema manipulation, so it runs
// in autocommit transactions like the constraints.
func (s *Store) EnsureTextIndexes(ctx context.Context) error {
	for _, ix := range textIndexes {
		ok, err := s.textIndexExists(ctx, ix.Name, ix.Label, ix.Props)
		if err != nil {
			return fmt.Errorf("graph: inspect text indexes: %w", err)
		}
		if ok {
			continue
		}
		ddl := fmt.Sprintf("CREATE TEXT INDEX %s ON :%s(%s)", ix.Name, ix.Label, strings.Join(ix.Props, ", "))
		if err := s.runAuto(ctx, ddl, nil); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				continue
			}
			return fmt.Errorf("graph: create text index %s: %w", ix.Name, err)
		}
	}
	return nil
}

// textIndexExists reports whether a text index *with the expected name*
// over exactly props exists on the label, per SHOW INDEX INFO (the name
// rides inside the index-type value: "label_text (name: entity_text)").
func (s *Store) textIndexExists(ctx context.Context, name, label string, props []string) (bool, error) {
	recs, err := s.runRead(ctx, "SHOW INDEX INFO", nil)
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		typ, _ := r.Get("index type")
		typStr := fmt.Sprint(typ)
		if !strings.Contains(strings.ToLower(typStr), "text") {
			continue
		}
		if !strings.Contains(typStr, "name: "+name) {
			continue
		}
		l, _ := r.Get("label")
		if fmt.Sprint(l) != label {
			continue
		}
		p, ok := r.Get("property")
		if !ok {
			p, _ = r.Get("properties")
		}
		if propsMatch(p, props) {
			return true, nil
		}
	}
	return false, nil
}

// ensureIndexes creates the secondary indexes idempotently.
func (s *Store) ensureIndexes(ctx context.Context) error {
	for _, ix := range secondaryIndexes {
		ok, err := s.indexExists(ctx, ix.Label, ix.Props)
		if err != nil {
			return fmt.Errorf("graph: inspect indexes: %w", err)
		}
		if ok {
			continue
		}
		ddl := fmt.Sprintf("CREATE INDEX ON :%s(%s)", ix.Label, strings.Join(ix.Props, ", "))
		if err := s.runAuto(ctx, ddl, nil); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				continue
			}
			return fmt.Errorf("graph: create index on (%s) %v: %w", ix.Label, ix.Props, err)
		}
	}
	return nil
}

// indexExists reports whether a label index on exactly props exists, using
// SHOW INDEX INFO.
func (s *Store) indexExists(ctx context.Context, label string, props []string) (bool, error) {
	recs, err := s.runRead(ctx, "SHOW INDEX INFO", nil)
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		l, _ := r.Get("label")
		if fmt.Sprint(l) != label {
			continue
		}
		// SHOW INDEX INFO names the column "property" (singular);
		// tolerate "properties" for version drift.
		p, ok := r.Get("property")
		if !ok {
			p, _ = r.Get("properties")
		}
		if propsMatch(p, props) {
			return true, nil
		}
	}
	return false, nil
}

// propsMatch reports whether the SHOW INDEX INFO properties column names
// exactly the props, in any order; the column shape varies by version.
func propsMatch(v any, props []string) bool {
	var list []string
	switch t := v.(type) {
	case string:
		list = []string{t}
	case []any:
		for _, e := range t {
			list = append(list, fmt.Sprint(e))
		}
	case []string:
		list = t
	}
	if len(list) != len(props) {
		return false
	}
	set := make(map[string]bool, len(list))
	for _, e := range list {
		set[e] = true
	}
	for _, p := range props {
		if !set[p] {
			return false
		}
	}
	return true
}

// constraintExists reports whether a label+property *unique* constraint
// already exists, using SHOW CONSTRAINT INFO. Rows of other constraint
// types (exists, data-type) match label and property too and must not be
// mistaken for the unique one.
func (s *Store) constraintExists(ctx context.Context, label, prop string) (bool, error) {
	recs, err := s.runRead(ctx, "SHOW CONSTRAINT INFO", nil)
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		typ, _ := r.Get("constraint type")
		if !strings.Contains(fmt.Sprint(typ), "unique") {
			continue
		}
		l, _ := r.Get("label")
		if fmt.Sprint(l) != label {
			continue
		}
		p, _ := r.Get("properties")
		if propsContain(p, prop) {
			return true, nil
		}
	}
	return false, nil
}

// propsContain reports whether the SHOW CONSTRAINT INFO properties column,
// which varies by Memgraph version (string, []any or []string), names prop.
func propsContain(v any, prop string) bool {
	switch t := v.(type) {
	case string:
		return t == prop
	case []any:
		for _, e := range t {
			if fmt.Sprint(e) == prop {
				return true
			}
		}
	case []string:
		for _, e := range t {
			if e == prop {
				return true
			}
		}
	}
	return false
}
