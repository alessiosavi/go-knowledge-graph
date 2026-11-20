package productobs

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

const testRegistry = `{
  "schema":"gkg-features/v1",
  "features":[
    {"id":"length","name":"Length","definition":"Size","kind":"measure","cardinality":"single","checkability":"page_checkable","measure":{"canonical_unit":"mm","aliases":{"mm":"1","cm":"10"}}},
    {"id":"color","name":"Color","definition":"Tone","kind":"enum","cardinality":"multi","checkability":"page_checkable","measure":null},
    {"id":"code","name":"Code","definition":"ID","kind":"code","cardinality":"single","checkability":"page_checkable","measure":null},
    {"id":"note","name":"Note","definition":"Note","kind":"text","cardinality":"single","checkability":"unverified_text","measure":null}
  ],
  "stores":{"example.test":{"keys":{
    "size":{"feature":"length","separator":null,"source_labels":["Size"],"dev_evidence":5,"numeric_search":true},
    "length":{"feature":"length","separator":null,"source_labels":["Length"],"dev_evidence":5,"numeric_search":false},
    "colors":{"feature":"color","separator":",","source_labels":[],"dev_evidence":5,"numeric_search":false},
    "shades":{"feature":"color","separator":";","source_labels":[],"dev_evidence":5,"numeric_search":false},
    "palette":{"feature":"color","separator":"|","source_labels":[],"dev_evidence":5,"numeric_search":false},
    "tones":{"feature":"color","separator":null,"source_labels":[],"dev_evidence":5,"numeric_search":false},
    "code":{"feature":"code","separator":null,"source_labels":[],"dev_evidence":5,"numeric_search":false},
    "ref":{"feature":"code","separator":null,"source_labels":[],"dev_evidence":5,"numeric_search":false}
  },"ignore":{"sku":{"reason":"identifier_internal","dev_evidence":1}},"deferred":["extra"]}}
}`

const validRecord = `{"store":"example.test","product_id":"example.test:test","title":"Test","brand":"","category":"","subcategory":"","product_type":null,"link":"","attributes":{},"page_specs":null}`

func TestFrozenRegistryDigest(t *testing.T) {
	const path = "/private/tmp/gkg-record-features/frozen/R.json"
	const want = "8c872af595eb51abaaf1a9e3154f88337fa3e840099cb8efe57decc8a2359bf7"
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("frozen registry fixture is unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	registry, err := ParseRegistry(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Digest(); got != want {
		t.Errorf("Digest() = %s, want %s", got, want)
	}
}

func TestRegistryLookup(t *testing.T) {
	registry, err := ParseRegistry([]byte(testRegistry))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  string
		want Mapping
	}{
		{"size", Mapping{FeatureID: "length", Kind: "measure", Cardinality: "single", CanonicalUnit: "mm", NumericSearch: true}},
		{"length", Mapping{FeatureID: "length", Kind: "measure", Cardinality: "single", CanonicalUnit: "mm"}},
		{"colors", Mapping{FeatureID: "color", Separator: ",", Kind: "enum", Cardinality: "multi"}},
		{"tones", Mapping{FeatureID: "color", Kind: "enum", Cardinality: "multi"}},
		{"code", Mapping{FeatureID: "code", Kind: "code", Cardinality: "single"}},
	} {
		if got, ok := registry.Lookup("example.test", tc.key); !ok || got != tc.want {
			t.Errorf("Lookup(%q) = (%+v, %t), want (%+v, true)", tc.key, got, ok, tc.want)
		}
	}
	for _, key := range []string{"Size", "sku", "extra", "missing"} {
		if _, ok := registry.Lookup("example.test", key); ok {
			t.Errorf("Lookup(%q) reported an unmapped key", key)
		}
	}
	if _, ok := registry.Lookup("unknown.test", "size"); ok {
		t.Error("Lookup reported a key from another store")
	}
}

func TestRegistryValidation(t *testing.T) {
	if _, err := ParseRegistry([]byte(testRegistry)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, old, replacement, diagnostic string }{
		{"schema", "gkg-features/v1", "gkg-features/v2", "schema"},
		{"unexpected field", `"schema":`, `"extra":0,"schema":`, "fields"},
		{"duplicate target", `"feature":"length"`, `"feature":"code","feature":"length"`, "duplicate"},
		{"duplicate mapping", `"keys":{`, `"keys":{"size":null,`, "duplicate"},
		{"duplicate id", `"id":"color"`, `"id":"length"`, "duplicate"},
		{"store grammar", `"example.test":{`, `"Example.test":{`, "store"},
		{"label across keys", `["Length"]`, `["Size"]`, "source label"},
		{"label within key", `["Size"]`, `["Size","Size"]`, "source label"},
		{"null label list", `["Size"]`, `null`, "source label"},
		{"numeric factor", `"mm":"1"`, `"mm":1`, "factor"},
		{"zero factor", `"mm":"1"`, `"mm":"0.00"`, "factor"},
		{"exponent factor", `"mm":"1"`, `"mm":"1e1"`, "factor"},
		{"duplicate folded alias", `"mm":"1"`, `"mm":"1","ＭＭ":"1"`, "alias"},
		{"temperature into length", `"mm":"1"`, `"mm":"1","degF":"1"`, "temperature"},
		{"length into temperature", `"canonical_unit":"mm"`, `"canonical_unit":"°C"`, "temperature"},
		{"scale mismatch", `"mm","aliases":{"mm":"1","cm":"10"}`, `"C","aliases":{"K":"1"}`, "temperature"},
		{"separator fold", `"separator":","`, `"separator":"，"`, "separator"},
		{"separator whitespace", `"separator":","`, `"separator":"a  b"`, "separator"},
		{"separator parentheses", `"separator":","`, `"separator":"("`, "separator"},
		{"compatibility parentheses", `"separator":","`, `"separator":"（"`, "separator"},
		{"single separator", `"separator":null`, `"separator":","`, "separator"},
		{"kind", `"kind":"enum"`, `"kind":"word"`, "kind"},
		{"cardinality", `"cardinality":"multi"`, `"cardinality":"multiple"`, "cardinality"},
		{"text checkability", `"checkability":"unverified_text"`, `"checkability":"page_checkable"`, "checkability"},
		{"nontext checkability", `"checkability":"page_checkable"`, `"checkability":"unverified_text"`, "checkability"},
		{"null measure", `"measure":{"canonical_unit":"mm","aliases":{"mm":"1","cm":"10"}}`, `"measure":null`, "measure"},
		{"nonmeasure payload", `"measure":null`, `"measure":{}`, "measure"},
		{"unknown feature", `"feature":"length"`, `"feature":"missing"`, "feature"},
		{"overlap dispositions", `["extra"]`, `["size"]`, "overlap"},
		{"ignore reason", `"identifier_internal"`, `"other"`, "reason"},
		{"map evidence", `"dev_evidence":5`, `"dev_evidence":4`, "DEV"},
		{"ignore evidence", `"dev_evidence":1`, `"dev_evidence":0`, "DEV"},
		{"fractional evidence", `"dev_evidence":5`, `"dev_evidence":5.0`, "DEV"},
		{"null flag", `"numeric_search":true`, `"numeric_search":null`, "numeric_search"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(testRegistry, tc.old, tc.replacement, 1)
			if input == testRegistry {
				t.Fatal("test did not mutate registry")
			}
			_, err := ParseRegistry([]byte(input))
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("expected diagnostic containing %q, got %v", tc.diagnostic, err)
			}
		})
	}

	// Schema mode does not run DEV evidence admission. A label may serve a
	// key at each of two stores and evidence remains an unbounded integer.
	input := strings.Replace(testRegistry, `"stores":{`, `"stores":{"another.test":{"keys":{"one":{"feature":"note","separator":null,"source_labels":["Size"],"dev_evidence":9999999999999999999999,"numeric_search":false}},"ignore":{},"deferred":[]},`, 1)
	if _, err := ParseRegistry([]byte(input)); err != nil {
		t.Fatal(err)
	}
}

func TestRecordValidation(t *testing.T) {
	if _, err := ReadRecords(strings.NewReader(validRecord)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, input, diagnostic string }{
		{"missing store", strings.Replace(validRecord, `"store":"example.test",`, "", 1), "fields"},
		{"extra field", strings.Replace(validRecord, `"brand":`, `"page_text":"","brand":`, 1), "fields"},
		{"uppercase field", strings.Replace(validRecord, `"brand":`, `"Brand":`, 1), "fields"},
		{"store mismatch", strings.Replace(validRecord, `"store":"example.test"`, `"store":"other.test"`, 1), "store"},
		{"opaque id without prefix", strings.Replace(validRecord, `example.test:test`, `test`, 1), "store"},
		{"null title", strings.Replace(validRecord, `"title":"Test"`, `"title":null`, 1), "title"},
		{"non-string attr", strings.Replace(validRecord, `"attributes":{}`, `"attributes":{"bad":7}`, 1), "attribute"},
		{"null attr", strings.Replace(validRecord, `"attributes":{}`, `"attributes":{"bad":null}`, 1), "attribute"},
		{"null attributes", strings.Replace(validRecord, `"attributes":{}`, `"attributes":null`, 1), "attributes"},
		{"top-level duplicate", strings.Replace(validRecord, `"title":`, `"title":"Secret","title":`, 1), "duplicate"},
		{"nested duplicate", strings.Replace(validRecord, `"attributes":{}`, `"attributes":{"secret":"a","\u0073ecret":"b"}`, 1), "duplicate"},
		{"duplicate in page specs", strings.Replace(validRecord, `"page_specs":null`, `"page_specs":[{"label":"a","label":"b","value":"c"}]`, 1), "duplicate"},
		{"invalid page specs", strings.Replace(validRecord, `"page_specs":null`, `"page_specs":[{"label":"a","value":false}]`, 1), "page_specs"},
		{"duplicate id", validRecord + "\n" + validRecord, "duplicate product_id"},
		{"invalid UTF-8", strings.Replace(validRecord, "Test", "\xff", 1), "JSON"},
		{"invalid constant", strings.Replace(validRecord, `"attributes":{}`, `"attributes":{"bad":NaN}`, 1), "JSON"},
		{"blank row", validRecord + "\n\n", "JSON"},
		{"trailing data", validRecord + "{}", "JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadRecords(strings.NewReader(tc.input))
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("expected diagnostic containing %q, got %v", tc.diagnostic, err)
			}
			if strings.Contains(strings.ToLower(err.Error()), "secret") {
				t.Fatalf("diagnostic disclosed input: %v", err)
			}
		})
	}
}

func TestReleaseProjection(t *testing.T) {
	registry, err := ParseRegistry([]byte(testRegistry))
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(testRegistry)))
	releaseJSON := fmt.Sprintf(`{"registry_sha256":%q,"keys":[{"store":"example.test","feature":"length","source_key":"size"}]}`, digest)
	release, err := registry.ParseRelease([]byte(releaseJSON))
	if err != nil {
		t.Fatal(err)
	}
	record := readTestRecord(t, map[string]string{"size": "10 mm", "length": "12 mm"})
	zero := strings.Replace(validRecord, "example.test:test", "unmapped.test:zero", 1)
	zero = strings.Replace(zero, `"store":"example.test"`, `"store":"unmapped.test"`, 1)
	zeroRecord, err := ReadRecords(strings.NewReader(zero))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := registry.Emit(&out, []Record{zeroRecord[0], record}, release); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"conflict":true`) || !strings.Contains(lines[0], `"source_key":"size"`) ||
		!strings.Contains(lines[1], `"product_id":"example.test:test"`) || !strings.Contains(lines[1], `"type":"product"`) ||
		!strings.Contains(lines[2], `"product_id":"unmapped.test:zero"`) || !strings.Contains(lines[2], `"type":"product"`) {
		t.Fatalf("unexpected release projection: %s", out.String())
	}
	out.Reset()
	if err := registry.Emit(&out, []Record{record, record}, release); err == nil || !strings.Contains(err.Error(), "duplicate product_id") || out.Len() != 0 {
		t.Fatalf("expected duplicate product rejection before writing, got %v", err)
	}
	for _, input := range []string{
		strings.Replace(releaseJSON, digest, strings.Repeat("0", 64), 1),
		strings.Replace(releaseJSON, `"size"`, `"sku"`, 1),
		strings.Replace(releaseJSON, `"feature":"length"`, `"feature":"color"`, 1),
		strings.Replace(releaseJSON, `"store":"example.test"`, `"store":"unmapped.test"`, 1),
		strings.Replace(releaseJSON, `}`+`]`, `},{"store":"example.test","feature":"length","source_key":"size"}]`, 1),
	} {
		if _, err := registry.ParseRelease([]byte(input)); err == nil {
			t.Errorf("accepted invalid release fixture: %s", input)
		}
	}
	emptyRelease, err := registry.ParseRelease([]byte(fmt.Sprintf(`{"registry_sha256":%q,"keys":[]}`, digest)))
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Observations(record, emptyRelease); len(got) != 0 {
		t.Errorf("empty release admitted observations: %s", testJSON(t, got))
	}
}
