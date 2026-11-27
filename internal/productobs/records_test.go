package productobs

import (
	"fmt"
	"strings"
	"testing"
)

func TestImportIdentityPrefixMatchesImporter(t *testing.T) {
	for _, tc := range []struct {
		id, store string
		valid     bool
	}{
		{"shop.example:123", "shop.example", true},
		{"shop.example:", "shop.example", true},
		{"shop.example:prefix:tail", "shop.example", true},
		{"shop.example", "shop.example", false},
		{":123", "", false},
		{"SHOP.example:123", "SHOP.example", false},
		{"shop.example:" + strings.Repeat("a", 1024), "shop.example", true},
	} {
		prefix, err := ImportIdentityPrefix(tc.id)
		if (err == nil) != tc.valid || (tc.valid && prefix != tc.store) {
			t.Errorf("prefix for %q = %q, %v", tc.id, prefix, err)
		}
		raw := fmt.Sprintf(`{"store":%q,"product_id":%q,"title":"a","brand":"b","category":"c","subcategory":"d","link":"l","product_type":null,"attributes":{},"page_specs":null}`, tc.store, tc.id)
		records, err := ReadRecords(strings.NewReader(raw))
		if (err == nil) != tc.valid || (tc.valid && (len(records) != 1 || records[0].ProductID != tc.id)) {
			t.Errorf("ReadRecords for %q: records = %+v, err = %v", tc.id, records, err)
		}
		_, err = ParseImportRecord([]byte(raw))
		cappedValid := tc.valid && len(tc.id) <= MaxImportIdentityBytes
		if (err == nil) != cappedValid {
			t.Errorf("ParseImportRecord for %q: err = %v, want valid %v", tc.id, err, cappedValid)
		}
	}
}
