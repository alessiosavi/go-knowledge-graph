package productobs

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestFrozenEmitter(t *testing.T) {
	var fixture struct {
		Registry json.RawMessage
		Records  []json.RawMessage
	}
	data, err := os.ReadFile("testdata/emit-input.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	registry, err := ParseRegistry(fixture.Registry)
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	for _, record := range fixture.Records {
		if err := json.Compact(&input, record); err != nil {
			t.Fatal(err)
		}
		input.WriteByte('\n')
	}
	records, err := ReadRecords(&input)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := registry.Emit(&output, records, nil); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/emit-expected.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// A6 drops the two Part A audit fields from the frozen product rows.
	var transformed bytes.Buffer
	encoder := jsontext.NewEncoder(&transformed)
	for line := range bytes.SplitSeq(bytes.TrimSuffix(want, []byte("\n")), []byte("\n")) {
		var fields map[string]jsontext.Value
		if err := jsonv2.Unmarshal(line, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["type"]) == `"product"` {
			delete(fields, "partition")
			delete(fields, "page_present")
		}
		if err := jsonv2.MarshalEncode(encoder, fields, jsonv2.Deterministic(true)); err != nil {
			t.Fatal(err)
		}
	}
	want = transformed.Bytes()
	if !bytes.Equal(output.Bytes(), want) {
		wantLines := bytes.Split(want, []byte("\n"))
		gotLines := bytes.Split(output.Bytes(), []byte("\n"))
		for i := range min(len(wantLines), len(gotLines)) {
			if !bytes.Equal(wantLines[i], gotLines[i]) {
				t.Fatalf("first differing fixture line %d\ngot  %s\nwant %s", i+1, gotLines[i], wantLines[i])
			}
		}
		t.Fatalf("fixture line count = %d, want %d", len(gotLines), len(wantLines))
	}
}

func TestFold(t *testing.T) {
	for _, tc := range []struct{ kind, raw, want string }{
		{"enum", " .ＳＴＲＡẞＥ  Blue：; ", "strasse blue"},
		{"text", " ; É\u0085\x1c ǅ . ", "é dž"},
		{"code", " ００ß - a. / ９ ", "00SS-A./9"},
		{"measure", " ７\u00a0 K . ", "7 K ."},
	} {
		if got := Fold(tc.kind, tc.raw); got != tc.want {
			t.Errorf("Fold(%q, %q) = %q, want %q", tc.kind, tc.raw, got, tc.want)
		}
	}
}

func TestObservationFolds(t *testing.T) {
	registry, err := ParseRegistry([]byte(testRegistry))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key, raw, valueFold string
		itemsFolds          []string
	}{
		{"code", " ００ß - a. ", "00SS-A.", nil},
		{"size", " ７\u00a0 MM ", "7 MM", nil},
		{"colors", " .Blue.:, ＲＥＤ, Red ", "blue.:, red, red", []string{"blue", "red", "red"}},
		{"colors", " .BLUE,Red;Green. ", "blue,red;green", []string{"blue,red;green"}},
		{"shades", " ; ; ", "", []string{}},
	} {
		row := readTestRecord(t, map[string]string{tc.key: tc.raw})
		observations := registry.Observations(row, nil)
		if len(observations) != 1 {
			t.Fatalf("%q: got %d observations, want one", tc.raw, len(observations))
		}
		observation := observations[0]
		if got := observation.ValueFold(); got != tc.valueFold {
			t.Errorf("%q: ValueFold() = %q, want %q", tc.raw, got, tc.valueFold)
		}
		if got := observation.ItemsFolds(); !reflect.DeepEqual(got, tc.itemsFolds) {
			t.Errorf("%q: ItemsFolds() = %#v, want %#v", tc.raw, got, tc.itemsFolds)
		}
	}
}

func TestMultiWithNullSeparator(t *testing.T) {
	registry, err := ParseRegistry([]byte(testRegistry))
	if err != nil {
		t.Fatal(err)
	}
	row := readTestRecord(t, map[string]string{"tones": " A, B; C "})
	got := registry.Observations(row, nil)
	if len(got) != 1 || got[0].SplitStatus != "split" || !slices.Equal(got[0].Items, []string{"A, B; C"}) {
		t.Fatalf("expected one split item with a null separator, got %s", testJSON(t, got))
	}
}

func TestSplitMulti(t *testing.T) {
	for _, tc := range []struct {
		name, raw, separator, status string
		items                        []string
	}{
		{"nested", " ; A (x (y;z),q) ; B;; ", ";", "split", []string{"A (x (y;z),q)", "B"}},
		{"decimal comma", " １，２, ٣,٤ , ｃ ", ",", "split", []string{"1,2", "٣,٤", "c"}},
		{"other protected delimiter", "A (x,y); 1,2; C", ";", "split", []string{"A (x,y)", "1,2", "C"}},
		{"mixed comma", " A, B; C ", ",", "unsplit", []string{" A, B; C "}},
		{"mixed semicolon", " A, B; C ", ";", "unsplit", []string{" A, B; C "}},
		{"mixed elsewhere", " A, B; C / D ", "/", "split", []string{"A, B; C", "D"}},
		{"unmatched open", " Ａ (B; C ", ";", "unsplit", []string{" Ａ (B; C "}},
		{"unmatched close", " A ); C ", ";", "unsplit", []string{" A ); C "}},
		{"no separator", " A, B; C ", "", "split", []string{"A, B; C"}},
		{"all empty", " ; ; ; ", ";", "split", []string{}},
		{"multicharacter", " A｜｜B｜｜C ", "||", "split", []string{"A", "B", "C"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, items := splitMulti(tc.raw, tc.separator)
			if status != tc.status || !reflect.DeepEqual(items, tc.items) {
				t.Fatalf("splitMulti = (%s, %#v), want (%s, %#v)", status, items, tc.status, tc.items)
			}
		})
	}
}

func TestMeasureGrammar(t *testing.T) {
	m, err := newMeasure("mm", map[string]string{
		"mm": "1", "m": "1000", "cm": "10", "%": "1", "min": "2",
		"μm": ".001", "mm/s": "2.50", "mm/s2": "3", "x": ".12345678901234567890",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		raw, qualifier, unit, lo, hi, loCanonical, hiCanonical string
		inclusive                                              bool
	}{
		{" ００７,２５００ ㎜ ", "exact", "mm", "7.25", "7.25", "7.25", "7.25", true},
		{"٣,٢٥ cm", "exact", "cm", "3.25", "3.25", "32.5", "32.5", true},
		{"７％–９％", "range", "%", "7", "9", "7", "9", true},
		{"3-7 mm", "range", "mm", "3", "7", "3", "7", true},
		{"da 3 a 7 cm", "range", "cm", "3", "7", "30", "70", true},
		{"≤ 7 µm", "upper", "μm", "", "7", "", "0.007", true},
		{"max.7 mm", "upper", "mm", "", "7", "", "7", true},
		{"≥7 cm", "lower", "cm", "7", "", "70", "", true},
		{"7mm/s2", "exact", "mm/s2", "7", "7", "21", "21", true},
		{"7mm/s", "exact", "mm/s", "7", "7", "17.5", "17.5", true},
		{"0 x", "exact", "x", "0", "0", "0", "0", true},
		{"100000000000000000001 x", "exact", "x", "100000000000000000001", "100000000000000000001", "12345678901234567890.1234567890123456789", "12345678901234567890.1234567890123456789", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			want := wantTyped("mm", tc.qualifier, tc.unit, tc.lo, tc.hi, tc.loCanonical, tc.hiCanonical, tc.inclusive)
			if got := m.parse(tc.raw); !reflect.DeepEqual(got, want) {
				t.Errorf("typed = %s, want %s", testJSON(t, got), testJSON(t, want))
			}
		})
	}
	for _, tc := range []struct {
		operators []string
		lo, hi    string
		qualifier string
		inclusive bool
	}{
		{[]string{"<", "sotto", "below", "under"}, "", "7", "upper", false},
		{[]string{"<=", "≤", "max", "max.", "fino a", "up to"}, "", "7", "upper", true},
		{[]string{">", "oltre", "above", "over"}, "7", "", "lower", false},
		{[]string{">=", "≥", "min", "min.", "at least"}, "7", "", "lower", true},
	} {
		for _, op := range tc.operators {
			want := wantTyped("mm", tc.qualifier, "mm", tc.lo, tc.hi, tc.lo, tc.hi, tc.inclusive)
			if got := m.parse(op + " 7 mm"); !reflect.DeepEqual(got, want) {
				t.Errorf("operator %q: typed = %s, want %s", op, testJSON(t, got), testJSON(t, want))
			}
		}
	}
	for _, raw := range []string{
		"7 in 3/12 mm", "7", "7,123 mm", "7.123 mm", "٧.١٢٣ mm", "0-7.123 mm",
		"7 m–9 mm", "7 min 2 sec", "margin 1/2", "prefix 7 mm", "7 mm suffix", "-7 mm", "7. mm", "7 °F", "7—9 mm",
	} {
		if got := m.parse(raw); !reflect.DeepEqual(got, &TypedValue{Status: "unparsed"}) {
			t.Errorf("%q: typed = %s, want all typed fields null apart from unparsed status", raw, testJSON(t, got))
		}
	}

	// The first unit can match "m" or "m-4m", with a matching right-side
	// alias only when the longer first unit wins.
	longest, err := newMeasure("m", map[string]string{"m": "1", "m-4m": "1", "m-7m-4m": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := longest.parse("3m-4m-7m-4m"); !reflect.DeepEqual(got, wantTyped("m", "range", "m-4m", "3", "7", "3", "7", true)) {
		t.Fatalf("expected longest matching aliases: %s", testJSON(t, got))
	}
}

func TestTemperatureScales(t *testing.T) {
	m, err := newMeasure("Celsius", map[string]string{"C": "1", "degC": "1", "°c": "1", "° c": "1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"7 °c", "7 ° C", "7 degC", "7℃"} {
		if got := m.parse(raw); got.Status != "ok" {
			t.Errorf("%q: expected parsed temperature", raw)
		}
	}
	for _, raw := range []string{"7 °F", "7 deg f", "7 kelvin", "7 K"} {
		if got := m.parse(raw); got.Status != "unparsed" {
			t.Errorf("%q: expected unparsed temperature", raw)
		}
	}
	for _, tc := range [][2]string{{"C", "° F"}, {"K", "degC"}, {"mm", "F"}, {"F", "mm"}} {
		if _, err := newMeasure(tc[0], map[string]string{tc[1]: "1"}); err == nil {
			t.Errorf("accepted cross-scale alias %q for %q", tc[1], tc[0])
		}
	}
}

func TestConflictRules(t *testing.T) {
	registry, err := ParseRegistry([]byte(testRegistry))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key, other, left, right string
		conflict                bool
	}{
		{"size", "length", "10 mm", "1 cm", false},
		{"size", "length", "10 mm", "<10 mm", true},
		{"size", "length", "<=10 mm", "<10 mm", true},
		{"size", "length", "10 mm", "10-10 mm", true},
		{"size", "length", "0-10 mm", "1-10 mm", true},
		{"size", "length", "10 mm", "１０", true},
		{"size", "length", "１０", "10", false},
		{"size", "length", "unknown", "UNKNOWN", true},
		{"code", "ref", " ００ß-1 ", "00SS-1", false},
		{"code", "ref", "00SS-1", "00SS1", true},
		{"colors", "shades", "Blue, red, red", " .RED.;", false},
		{"colors", "shades", "red,blue", "red;green", true},
		{"colors", "shades", " Red,Blue;green ", "red,blue;green", false},
		{"colors", "shades", "red,blue;green", "red;green", true},
		{"colors", "shades", "red,blue", "red,blue;green", true},
		{"colors", "shades", "", "red;green", false},
	} {
		row := readTestRecord(t, map[string]string{tc.key: tc.left, tc.other: tc.right})
		got := registry.Observations(row, nil)
		if len(got) != 2 || got[0].Conflict != tc.conflict || got[1].Conflict != tc.conflict {
			t.Errorf("%q and %q: got %s, want conflict %t on both observations", tc.left, tc.right, testJSON(t, got), tc.conflict)
		}
	}
	row := readTestRecord(t, map[string]string{"colors": "A,B", "shades": "A", "palette": "B"})
	if got := registry.Observations(row, nil); len(got) != 3 || slices.ContainsFunc(got, func(o Observation) bool { return !o.Conflict }) {
		t.Errorf("every alias must conflict when any pair disagrees: %s", testJSON(t, got))
	}
}

func TestCanonicalTypedEquality(t *testing.T) {
	left := Observation{Typed: wantTyped("mm", "range", "cm", "1", "2", "10", "20", true)}
	right := Observation{Typed: wantTyped("mm", "range", "mm", "10", "20", "10", "20", true)}
	if !equalSingle(left, right) {
		t.Fatal("equal canonical quantities with different aliases must agree")
	}
	for _, change := range []func(*TypedValue){
		func(tv *TypedValue) { tv.Qualifier = new("lower") },
		func(tv *TypedValue) { tv.Inclusive = new(false) },
		func(tv *TypedValue) { tv.CanonicalUnit = new("cm") },
		func(tv *TypedValue) { tv.LoCanonical = new("1") },
		func(tv *TypedValue) { tv.HiCanonical = nil },
	} {
		changed := *right.Typed
		change(&changed)
		if equalSingle(left, Observation{Typed: &changed}) {
			t.Errorf("distinct canonical tuples agreed: %s", testJSON(t, &changed))
		}
	}
}

func TestCanonicalUTF8(t *testing.T) {
	registry, err := ParseRegistry([]byte(testRegistry))
	if err != nil {
		t.Fatal(err)
	}
	record := readTestRecord(t, map[string]string{"shades": ";;;", "code": "É<&\u2028\u2029\x01"})
	var output bytes.Buffer
	if err := registry.Emit(&output, []Record{record}, nil); err != nil {
		t.Fatal(err)
	}
	want := `{"conflict":false,"feature":"code","items":null,"product_id":"example.test:test","raw":"É<&` +
		"\u2028\u2029" + `\u0001","source_key":"code","split_status":"single","type":"obs","typed":null}` + "\n"
	if !strings.HasPrefix(output.String(), want) || !strings.Contains(output.String(), `"items":[]`) {
		t.Fatalf("unexpected canonical encoding: %q", output.String())
	}
}

func wantTyped(canonicalUnit, qualifier, unit, lo, hi, loCanonical, hiCanonical string, inclusive bool) *TypedValue {
	return &TypedValue{
		CanonicalUnit: new(canonicalUnit), Qualifier: new(qualifier), UnitIn: new(unit), Inclusive: new(inclusive),
		Lo: optionalText(lo), Hi: optionalText(hi), LoCanonical: optionalText(loCanonical), HiCanonical: optionalText(hiCanonical),
		Status: "ok",
	}
}

func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func testJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readTestRecord(t *testing.T, attributes map[string]string) Record {
	t.Helper()
	input := strings.Replace(validRecord, `"attributes":{}`, `"attributes":`+testJSON(t, attributes), 1)
	records, err := ReadRecords(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	return records[0]
}
