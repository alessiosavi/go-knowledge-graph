package graph

import (
	"reflect"
	"testing"
)

func TestDomainStores(t *testing.T) {
	st := scopedReadTestStore(t)
	foldTestWrite(t, st, `
CREATE (:Domain:ScopedReadFixture {id:$domain}),
       (:Domain:ScopedReadFixture {id:'scoped-empty'}),
       (:ProductStore:ScopedReadFixture {store:'shop.example', domain:$domain}),
       (:ProductStore:ScopedReadFixture {store:'other.example', domain:$domain})`,
		map[string]any{"domain": cardTestDomain})
	for _, tc := range []struct {
		domain string
		exists bool
		stores []string
	}{
		{cardTestDomain, true, []string{"other.example", "shop.example"}},
		{"scoped-empty", true, []string{}},
		{"scoped-missing", false, []string{}},
	} {
		t.Run(tc.domain, func(t *testing.T) {
			exists, stores, err := st.DomainStores(t.Context(), tc.domain)
			if err != nil || exists != tc.exists || !reflect.DeepEqual(stores, tc.stores) {
				t.Fatalf("DomainStores = %t, %#v, %v, want %t, %#v", exists, stores, err, tc.exists, tc.stores)
			}
		})
	}
}
