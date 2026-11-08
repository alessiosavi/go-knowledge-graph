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
	return nil
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
