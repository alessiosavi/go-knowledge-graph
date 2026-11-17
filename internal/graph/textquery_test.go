package graph_test

import (
	"context"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestTextQueryTerms(t *testing.T) {
	for _, tt := range []struct {
		query string
		want  string
	}{
		{"l'Acme-Max", `"l" "acme" "max"`},
		{"Zorbo's", `"zorbo" "s"`},
		{"a:b", `"a" "b"`},
		{"(x", `"x"`},
		{`"x`, `"x"`},
		{"x AND", `"x" "and"`},
		{"Or not", `"or" "not"`},
		{"*", ""},
		{"  Caffè  Ünïcode ", `"caffè" "ünïcode"`},
		{"C++ / Go", `"c" "go"`},
		{"", ""},
		{" \t\n", ""},
		{"Go27_１２٣", `"go27" "１２٣"`},
		{"x and", `"x" "and"`},
		{"oR nOt", `"or" "not"`},
		{"Zorbe\u0300llo", `"zorbèllo"`},
		{"ZORBÈLLO", `"zorbèllo"`},
	} {
		t.Run(tt.query, func(t *testing.T) {
			if got := graph.TextQueryTerms(tt.query); got != tt.want {
				t.Errorf("TextQueryTerms(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestTextSearchEmptyTerms(t *testing.T) {
	// An unopened store makes a database query fail: empty terms must
	// return nil, nil before touching the driver.
	var st graph.Store
	ctx := context.Background()
	for _, query := range []string{"", " \t\n", "*", `':()+-/`, "💡"} {
		t.Run(query+"/entity", func(t *testing.T) {
			rows, err := st.EntityTextSearch(ctx, query, 10)
			if rows != nil || err != nil {
				t.Errorf("EntityTextSearch(%q) = %+v, %v; want nil, nil", query, rows, err)
			}
		})
		t.Run(query+"/chunk", func(t *testing.T) {
			rows, err := st.ChunkTextSearch(ctx, query, 10)
			if rows != nil || err != nil {
				t.Errorf("ChunkTextSearch(%q) = %+v, %v; want nil, nil", query, rows, err)
			}
		})
	}
}
