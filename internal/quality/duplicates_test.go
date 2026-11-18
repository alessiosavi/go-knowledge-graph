package quality

import "testing"

func TestDuplicateTierGuards(t *testing.T) {
	for _, test := range []struct {
		name, left, right, rightType, reason string
	}{
		{"same type", "Kathmandù", "Kathmandu", "place", ""},
		{"different types", "Kathmandù", "Kathmandu", "organization", "type_mismatch"},
		{"four Unicode letters", "東京大阪", "東京-大阪", "place", ""},
		{"three letters despite four characters", "123 A.B.C", "123 ABC", "place", "fold_too_short"},
		{"C++ versus C#", "C++", "C#", "place", "fold_too_short"},
		{"C# versus C", "C#", "C", "place", "fold_too_short"},
		{"C++ versus C", "C++", "C", "place", "fold_too_short"},
		{"symbol multiplicity matches", "Alpha++#@/&", "Alpha&/@#++", "place", ""},
		{"symbol multiplicity differs", "Alpha++#@/&", "Alpha&/@#+", "place", "symbol_mismatch"},
		{"symbols differ", "Alpha+", "Alpha#", "place", "symbol_mismatch"},
		{"compatibility symbol matches", "Alpha＋", "Alpha+", "place", ""},
		{"compatibility symbol is counted", "Alpha＋", "Alpha", "place", "symbol_mismatch"},
		{"digit runs match", "12 ACME Pro", "12 ACME-Pro", "place", ""},
		{"compatibility digit runs match", "１２ ACME Pro", "12 ACME-Pro", "place", ""},
		{"compatibility digit runs still reach model guard", "Acme １２", "Acme 12", "place", "version_or_model"},
		{"different digit runs without a model", "12 ACME Pro", "1-2 ACME Pro", "place", "digit_run_mismatch"},
		{"digit runs differ", "ACME 12 Pro", "ACME 1-2 Pro", "place", "digit_run_mismatch"},
		{"dotted version", "1.10 Alpha", "110 Alpha", "place", "digit_run_mismatch"},
		{"dotted versions split digits differently", "1.10", "11.0", "place", "fold_too_short"},
		{"dotted version with matching digit runs", "1.10 Alpha", "1-10 Alpha", "place", "version_or_model"},
		{"v version", "Alpha V2", "Alpha V.2", "place", "version_or_model"},
		{"model with a space", "Alpha X 100", "Alpha X-100", "place", "version_or_model"},
		{"compact model", "Alpha X100", "Alpha X-100", "place", "version_or_model"},
		{"no version or model", "24 Hours", "24-Hours", "place", ""},
		{"accented company word", "Società", "Societa", "place", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			fold := duplicateFold(test.left)
			if fold != duplicateFold(test.right) {
				t.Fatalf("fixture does not group: %q and %q", test.left, test.right)
			}
			members := []duplicateMember{
				{Key: "@test:left", Name: test.left, Type: "place"},
				{Key: "@test:right", Name: test.right, Type: test.rightType},
			}
			if got := duplicateTierReason(fold, members); got != test.reason {
				t.Errorf("reason = %q, want %q", got, test.reason)
			}
		})
	}
}

func TestDuplicateTierRequiresTypeAndKeyDomain(t *testing.T) {
	members := []duplicateMember{
		{Key: "@north:kathmandù", Name: "Kathmandù"},
		{Key: "@north:kathmandu", Name: "Kathmandu"},
	}
	if got := duplicateTierReason("kathmandu", members); got != "type_missing" {
		t.Errorf("empty type reason = %q, want type_missing", got)
	}
	members[0].Type, members[1].Type = "place", "place"
	members[1].Key = "@south:kathmandu"
	if got := duplicateTierReason("kathmandu", members); got != "key_domain_mismatch" {
		t.Errorf("mixed key domain reason = %q, want key_domain_mismatch", got)
	}
}

func TestDuplicateCensusGrouping(t *testing.T) {
	entities := map[string]entity{
		"@north:milano": {name: "Milano"}, "@north:milan": {name: "Milan"},
		"@north:x100": {name: "X 100"}, "@north:x200": {name: "X 200"},
		"@north:accent": {name: "Società", typ: "entity"}, "@north:plain": {name: "Societa", typ: "entity"},
		"@south:accent": {name: "Societá", typ: "entity"}, "@south:plain": {name: "Societa", typ: "entity"},
		"#one:scoped": {name: "Societá"}, "@north:#one:scoped": {name: "Societá"},
		"legacy-accent": {name: "Societá", typ: "entity"}, "legacy-plain": {name: "Societa", typ: "entity"},
	}
	support := make(map[string]stringSet)
	for key := range entities {
		add(support, key, "one")
	}
	support["@south:plain"] = stringSet{"two": true}
	add(support, "@north:accent", "two")
	groups := duplicateCensusGroups(entities, support)
	if len(groups) != 3 || len(groups["north"]) != 1 || len(groups["south"]) != 1 || len(groups[""]) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	for _, domain := range []string{"north", "south"} {
		group := groups[domain][0]
		if group.Domain != domain || group.Fold != "societa" || group.Tier != "A" || len(group.Members) != 2 || group.Members[0].Key != "@"+domain+":accent" {
			t.Fatalf("%s group = %+v", domain, group)
		}
	}
	if groups["north"][0].Members[0].DocumentCount != 2 || groups["south"][0].Members[1].DocumentCount != 1 {
		t.Fatalf("expected supporting document counts: %+v", groups)
	}
	if legacy := groups[""][0]; legacy.Tier != "other" || legacy.Reason != "key_domain_mismatch" || len(legacy.Members) != 2 {
		t.Fatalf("legacy group = %+v", legacy)
	}
}

func TestDuplicateEdgePercentile(t *testing.T) {
	if max, p95 := duplicateEdgePercentile(nil); max != 0 || p95 != 0 {
		t.Fatalf("empty max/p95 = %d/%d", max, p95)
	}
	counts := []int{500, 1, 20, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	if max, p95 := duplicateEdgePercentile(counts); max != 500 || p95 != 20 {
		t.Fatalf("max/p95 = %d/%d, want 500/20", max, p95)
	}
}
