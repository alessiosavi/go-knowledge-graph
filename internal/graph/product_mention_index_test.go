package graph

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadProductMentionIndexRawLimit(t *testing.T) {
	f := newPushFixture(t)
	f.query(`CREATE (p:Product {id:'mention-limit-p', store:'push.test'}),
		(f:FeatureRev {id:'catalogue_code', key:'mention-limit-code'}),
		(p)-[:HAS_FEATURE {release_id:$release, value_fold:'AB12345', items_fold:['AB12345','AB12345']}]->(f)`,
		map[string]any{"release": pushA})
	ctx := t.Context()
	index, err := f.store.loadProductMentionIndex(ctx, 2)
	if !errors.Is(err, ErrProductMentionIndexTooLarge) || index != nil {
		t.Fatalf("limit 2: index=%v err=%v", index, err)
	}
	index, err = f.store.loadProductMentionIndex(ctx, 3)
	if err != nil || index.Stats.IdentifierValues != 1 || len(index.Match("AB12345")) != 1 {
		t.Fatalf("limit 3: index=%+v err=%v", index, err)
	}

	// A second binding must share the same cap. An unbound store and values
	// from another release must contribute nothing to the count.
	f.query(`MATCH (f:FeatureRev {key:'mention-limit-code'})
		CREATE (:ProductStore {store:'mention-limit-other.test'}),
		(q:Product {id:'mention-limit-q', store:'mention-limit-other.test'}),
		(q)-[:HAS_FEATURE {release_id:$release, value_fold:'CD12345'}]->(f),
		(q)-[:HAS_FEATURE {release_id:'inactive', items_fold:['EF12345','GH12345']}]->(f)`,
		map[string]any{"release": pushA})
	index, err = f.store.loadProductMentionIndex(ctx, 3)
	if err != nil || len(index.Match("AB12345, CD12345")) != 1 {
		t.Fatalf("unbound store contributed to the limit: %+v %v", index, err)
	}
	f.query(`MATCH (s:ProductStore {store:'mention-limit-other.test'}) SET s.domain='push-rebound'`, nil)
	index, err = f.store.loadProductMentionIndex(ctx, 3)
	if !errors.Is(err, ErrProductMentionIndexTooLarge) || index != nil {
		t.Fatalf("bound store missing from limit: %+v %v", index, err)
	}
	index, err = f.store.loadProductMentionIndex(ctx, 4)
	if err != nil {
		t.Fatalf("limit 4: %v", err)
	}
	matches := index.Match("AB12345, CD12345, EF12345")
	if len(matches) != 2 || !strings.HasPrefix(matches[1].Product.ID, "mention-limit-q") {
		t.Fatalf("matches = %+v", matches)
	}
}
