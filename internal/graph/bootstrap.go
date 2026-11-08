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
}

// secondaryIndexes are non-unique label indexes bootstrapped for query
// performance (the upsert identity lookup filters on both properties).
var secondaryIndexes = []struct {
	Label string
	Props []string
}{
	{"Document", []string{"source_kind", "source"}},
}

// Bootstrap creates the schema idempotently. Memgraph has no documented
// IF NOT EXISTS for constraints, so existence is checked first and an
// already-exists error from a concurrent creator is tolerated.
func (s *Store) Bootstrap(ctx context.Context) error {
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
	return s.ensureIndexes(ctx)
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
