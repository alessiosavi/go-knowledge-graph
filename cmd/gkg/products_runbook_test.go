package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	jsonv2 "encoding/json/v2"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	runbookStore       = "dentalica.com"
	runbookDomain      = "dentalica"
	runbookOtherDomain = "runbook-other"

	runbookRegistrySHA     = "8c872af595eb51abaaf1a9e3154f88337fa3e840099cb8efe57decc8a2359bf7"
	runbookObservationsSHA = "8b135c124f9f7f0896bbc741ea5c58dbe61778b28c911f3b8907ffa9968ff421"
	runbookOracleSHA       = "ce003491f81bc79a747328d1c97e11c1ff8bce76612b7596122e2dd0397d5d2f"
	runbookProductCount    = 3423
	runbookEdgeCount       = 13979

	runbookMiB           = 1024 * 1024
	runbookCLILimit      = 512 * runbookMiB
	runbookMemgraphLimit = 832 * runbookMiB
	runbookImportLimit   = 10 * time.Minute
	runbookSamplePeriod  = 200 * time.Millisecond
)

type runbookCandidates struct {
	RegistrySHA string                     `json:"registry_sha256"`
	Keys        []productobs.ReleaseTriple `json:"keys"`
}

type runbookReleaseFiles struct {
	registry, candidates string
	pins                 map[string]string
}

type productsRunbook struct {
	binary, dir, uri string
	env              []string
	driver           neo4j.Driver
	measurements     runbookMeasurements
}

func TestProductsRunbook(t *testing.T) {
	root, uri := os.Getenv("GKG_PRODUCTS_RUNBOOK_DIR"), os.Getenv(graphtest.URIEnv)
	if root == "" || uri == "" {
		t.Skip("set GKG_PRODUCTS_RUNBOOK_DIR and GKG_TEST_MEMGRAPH_URI to run the frozen catalogue proof")
	}
	bolt, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(bolt.Hostname()); bolt.Scheme != "bolt" || !ip.IsLoopback() || bolt.Port() == "" || bolt.Port() == "7687" || bolt.User != nil {
		t.Fatal("runbook requires a scratch Bolt URI on a non-default loopback port")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Fatal("runbook supports CLI Maxrss units on darwin and linux")
	}

	dir := t.TempDir()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	r := &productsRunbook{
		dir: dir, uri: uri, binary: filepath.Join(dir, "gkg"),
		measurements: runbookMeasurements{
			CLILimitBytes: runbookCLILimit, MemgraphLimitBytes: runbookMemgraphLimit,
			ImportLimitSeconds: runbookImportLimit.Seconds(),
		},
	}
	// The child commands use only the local store and repo presets. In particular,
	// they must not inherit runtime credentials or provider endpoints.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "PATH" || key == "SYSTEMROOT" {
			r.env = append(r.env, entry)
		}
	}
	r.env = append(r.env, "GKG_MEMGRAPH_URI="+uri,
		"GKG_LABELS_PRESETS_DIR="+filepath.Join(repo, "labels"),
		"GKG_INGEST_SPOOL_DIR="+filepath.Join(dir, "spool"))
	build := exec.CommandContext(t.Context(), "go", "build", "-o", r.binary, "./cmd/gkg")
	build.Dir = repo
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gkg: %v\n%s", err, output)
	}

	frozen := runbookReleaseFiles{
		registry:   filepath.Join(root, "frozen/R.json"),
		candidates: filepath.Join(root, "run/release-candidates.json"),
		pins: map[string]string{
			"R.json":           filepath.Join(root, "frozen/R.json"),
			"rf_kit.py":        filepath.Join(root, "frozen/rf_kit.py"),
			"freeze.json":      filepath.Join(root, "frozen/freeze.json"),
			"E2":               filepath.Join(root, "raw/e2-raw.jsonl"),
			"score.json":       filepath.Join(root, "run/score.json"),
			"report.json":      filepath.Join(root, "run/report.json"),
			"record-obs.jsonl": filepath.Join(root, "run/record-obs.jsonl"),
		},
	}
	var released runbookCandidates
	runbookDecode(t, runbookRead(t, frozen.candidates), &released)
	registry := runbookRead(t, frozen.registry)
	observations := runbookRead(t, frozen.pins["record-obs.jsonl"])
	recordsFile := filepath.Join(root, "a1/records.jsonl")
	records := runbookRead(t, recordsFile)

	var full []byte
	if !t.Run("a_frozen_emit", func(t *testing.T) {
		if got := productFixtureDigest(string(registry)); got != runbookRegistrySHA {
			t.Fatalf("frozen registry SHA-256 = %s, want %s", got, runbookRegistrySHA)
		}
		if got := productFixtureDigest(string(observations)); got != runbookObservationsSHA {
			t.Fatalf("frozen observations SHA-256 = %s, want %s", got, runbookObservationsSHA)
		}
		oracle := runbookOracle(t, observations, released.Keys)
		if got := productFixtureDigest(string(oracle)); got != runbookOracleSHA {
			t.Fatalf("released oracle SHA-256 = %s, want %s", got, runbookOracleSHA)
		}
		full = r.emit(t, frozen, recordsFile)
		runbookEqualJSONL(t, full, oracle)
		t.Logf("released oracle byte-equal, SHA-256 %s", runbookOracleSHA)
	}) {
		return
	}

	out := os.Getenv("GKG_PRODUCTS_RUNBOOK_OUT")
	persist := out != ""
	if out == "" {
		out = t.TempDir()
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	defer func() {
		path := filepath.Join(out, "measurements.json")
		encoded, err := json.MarshalIndent(r.measurements, "", "  ")
		if err == nil {
			err = os.WriteFile(path, append(encoded, '\n'), 0600)
		}
		if err != nil {
			t.Errorf("write measurements: %v", err)
		} else if persist {
			t.Logf("measurements written to %s", path)
		} else {
			t.Logf("measurements:\n%s", encoded)
		}
	}()

	r.driver, err = neo4j.NewDriver(r.uri, neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.driver.Close(context.Background()) })
	if rows := r.query(t, "MATCH (n) RETURN count(n)", nil); rows[0].Values[0] != int64(0) {
		t.Fatal("runbook requires an empty scratch database")
	}
	r.cli(t, "migrate", "-schema-only")
	st := graphtest.Store(t)
	docIDs := r.seedMixedGraph(t, st)
	r.cli(t, "fold", "rebuild", "-clear-exceptions") // Initialize the record required by the read-only merge plan.
	baseGraph := graphtest.Snapshot(t)
	withoutProducts := r.maintenance(t, st)
	r.domainConflicts(t, st, docIDs, false)
	baseGraph.AssertUnchanged(t, "G0 maintenance")

	var wideID, wideRev string
	if !t.Run("b1_frozen_import", func(t *testing.T) {
		wideID, wideRev = r.activate(t, frozen, runbookStore)
		r.importSnapshot(t, wideID, runbookStore, 1, recordsFile, "imported", runbookProductCount, runbookEdgeCount, 1)
		runbookEqualJSONL(t, r.readProducts(t, wideID, wideRev, runbookStore).projected, full)
		t.Logf("read back %d products and %d observations byte-equal to (a)", runbookProductCount, runbookEdgeCount)
	}) {
		return
	}
	t.Run("x_mixed_graph", func(t *testing.T) {
		mixedGraph := graphtest.Snapshot(t)
		withProducts := r.maintenance(t, st)
		r.compareMaintenance(t, withoutProducts, withProducts)
		r.domainConflicts(t, st, docIDs, true)
		mixedGraph.AssertUnchanged(t, "G1 maintenance and rejected moves/merges")

		// Binding is the only expected change on a non-product node. Comparison
		// retains every edge touching a non-product node, including cross-edges.
		guards := r.query(t, `MATCH (d:Domain) RETURN d.id,d.product_guard ORDER BY d.id`, nil)
		if len(guards) != 2 || !reflect.DeepEqual(guards[0].Values, []any{runbookDomain, int64(1)}) ||
			!reflect.DeepEqual(guards[1].Values, []any{runbookOtherDomain, nil}) {
			t.Fatalf("domain binding guards: %v", guards)
		}
		project := func(labels []any, props map[string]any) bool {
			for _, label := range []string{"Product", "ProductStore", "ProductRelease", "FeatureRev", "FeatureRegistry"} {
				if slices.Contains(labels, any(label)) {
					return false
				}
			}
			if slices.Contains(labels, any("GkgMeta")) && (props["id"] == "product_release" || props["id"] == "feature_registry_import") {
				return false
			}
			if slices.Contains(labels, any("Domain")) && props["id"] == runbookDomain {
				delete(props, "product_guard")
			}
			return true
		}
		baseGraph.ProjectNodes(project).AssertEqual(t, mixedGraph.ProjectNodes(project), "G0 vs G1 non-product graph")
		t.Log("only the bound domain's product count/binding flag and Cypher product counts changed; move and merge returned 409; fence unchanged")
	})
	t.Run("b2_replay", func(t *testing.T) {
		before := graphtest.Snapshot(t)
		r.importSnapshot(t, wideID, runbookStore, 2, recordsFile, "unchanged", runbookProductCount, runbookEdgeCount, 0)
		// A2 requires the highest admitted sequence to advance on replay.
		// A4 excludes import from the judge lease, so its fence delta is zero.
		expected := before.ProjectNodes(func(labels []any, props map[string]any) bool {
			if slices.Contains(labels, any("ProductStore")) && props["store"] == runbookStore {
				if props["seen_seq"] != int64(1) || props["snapshot_seq"] != int64(1) {
					t.Fatalf("pre-replay watermark = %v", props)
				}
				props["seen_seq"] = int64(2)
			}
			return true
		})
		expected.AssertJudgeLeaseFenceAdvanced(t, "unchanged snapshot replay", 0)
		t.Log("unchanged; full graph snapshot differs only in seen_seq (1 → 2)")
	})

	if !t.Run("b3_remove_ten", func(t *testing.T) {
		shrunkRecords, removed := runbookRemoveProducts(t, records)
		shrunkPath := writeProductFixture(t, dir, "shrunk.jsonl", string(shrunkRecords))
		expected, products, edges := runbookFilterProjection(t, full, func(row map[string]any) bool {
			return !removed[row["product_id"].(string)]
		})
		if products != runbookProductCount-10 {
			t.Fatalf("fixture removed %d products, want 10", runbookProductCount-products)
		}
		before := graphtest.Snapshot(t)
		beforeShrink := r.readProducts(t, wideID, wideRev, runbookStore)
		started := time.Now()
		r.importSnapshot(t, wideID, runbookStore, 3, shrunkPath, "imported", products, edges, 0)
		finished := time.Now()
		after := graphtest.Snapshot(t)
		actual := r.readProducts(t, wideID, wideRev, runbookStore)
		runbookEqualJSONL(t, actual.projected, expected)
		maps.DeleteFunc(beforeShrink.properties, func(key runbookRowKey, _ map[string]any) bool { return removed[key.id] })
		for key, props := range beforeShrink.properties {
			if !reflect.DeepEqual(props, actual.properties[key]) {
				t.Fatalf("surviving product or edge properties changed: %+v", key)
			}
		}

		// Replacement allocates new Memgraph IDs. The read-back above checks the
		// labels, properties and relationships of all products by logical key.
		// Compare the rest of the full snapshot by physical ID, including schema.
		keep := func(labels []any, props map[string]any) bool {
			return !slices.Contains(labels, any("Product")) || props["store"] != runbookStore
		}
		isStore := func(labels []any, props map[string]any) bool {
			return slices.Contains(labels, any("ProductStore")) && props["store"] == runbookStore
		}
		var importedAt string
		after = after.WithoutRelationshipType("HAS_FEATURE").ProjectNodes(func(labels []any, props map[string]any) bool {
			if isStore(labels, props) {
				importedAt, _ = props["imported_at"].(string)
			}
			return keep(labels, props)
		})
		stamp, err := time.Parse("2006-01-02T15:04:05.000000000Z", importedAt)
		if err != nil || stamp.Before(started) || stamp.After(finished) {
			t.Fatalf("ProductStore imported_at = %q, want UTC import timestamp between %s and %s", importedAt, started.UTC(), finished.UTC())
		}
		digest := productFixtureDigest(string(shrunkRecords))
		before.WithoutRelationshipType("HAS_FEATURE").ProjectNodes(func(labels []any, props map[string]any) bool {
			if isStore(labels, props) {
				props["product_count"], props["edge_count"] = int64(products), int64(edges)
				props["seen_seq"], props["snapshot_seq"] = int64(3), int64(3)
				props["snapshot_digest"], props["imported_at"] = digest, importedAt
			}
			return keep(labels, props)
		}).AssertEqual(t, after, "shrink snapshot outside rebuilt product records and ProductStore watermark")
		t.Logf("removed exactly 10 products and their %d observations; ProductStore counts %d/%d, sequences 3/3, SHA-256 %s",
			runbookEdgeCount-edges, products, edges, digest)
	}) {
		return
	}

	var remappedRegistry []byte
	var remappedKeys []productobs.ReleaseTriple
	var remappedOutput []byte
	var remappedID, remappedRev string
	if !t.Run("b4_remap", func(t *testing.T) {
		remappedRegistry, remappedKeys = runbookRemapRegistry(t, registry, released.Keys)
		remapped := runbookSyntheticRelease(t, remappedRegistry, remappedKeys)
		remappedOutput = r.emit(t, remapped, recordsFile)
		remappedID, remappedRev = r.activate(t, remapped, runbookStore)
		if remappedRev == wideRev || remappedID == wideID || r.edgeCount(t) != 0 {
			t.Fatal("new registry activation must change release and revision and prune every old edge")
		}
		r.importSnapshot(t, remappedID, runbookStore, 4, recordsFile, "imported", runbookProductCount, runbookEdgeCount, 0)
		runbookEqualJSONL(t, r.readProducts(t, remappedID, remappedRev, runbookStore).projected, remappedOutput)
		t.Logf("full remap on revision %s, all %d edges carry release %s", remappedRev, runbookEdgeCount, remappedID)
	}) {
		return
	}

	t.Run("b5_narrow", func(t *testing.T) {
		drop := remappedKeys[0]
		kept := slices.DeleteFunc(slices.Clone(remappedKeys), func(key productobs.ReleaseTriple) bool { return key == drop })
		narrow := runbookSyntheticRelease(t, remappedRegistry, kept)
		expected, products, edges := runbookFilterProjection(t, remappedOutput, func(row map[string]any) bool {
			return row["feature"] != drop.Feature || row["source_key"] != drop.SourceKey
		})
		if products != runbookProductCount || edges >= runbookEdgeCount {
			t.Fatalf("narrow fixture did not drop an observed triple: %+v", drop)
		}
		runbookEqualJSONL(t, r.emit(t, narrow, recordsFile), expected)
		id, rev := r.activate(t, narrow, runbookStore)
		if id == remappedID || rev != remappedRev || r.edgeCount(t) != int64(edges) {
			t.Fatal("narrow activation did not immediately prune exactly the dropped edges")
		}
		r.importSnapshot(t, id, runbookStore, 5, recordsFile, "imported", products, edges, 0)
		runbookEqualJSONL(t, r.readProducts(t, id, rev, runbookStore).projected, expected)
		t.Logf("dropped %s/%s (%d observations) at activation; still absent after import", drop.Feature, drop.SourceKey, runbookEdgeCount-edges)
	})

	t.Run("m_three_store_import", func(t *testing.T) {
		r.cli(t, "products", "delete-store", runbookStore)
		r.cli(t, "products", "unbind", runbookStore)
		stores := []string{"runbook-1.test", "runbook-2.test", "runbook-3.test"}
		clonedRegistry, keys := runbookCloneRegistry(t, registry, released.Keys, stores)
		files := runbookSyntheticRelease(t, clonedRegistry, keys)
		id, rev := r.activate(t, files, stores...)
		var projected []byte
		for _, store := range stores {
			data := runbookCloneRecords(t, records, store)
			path := writeProductFixture(t, dir, store+".jsonl", string(data))
			r.importSnapshot(t, id, store, 1, path, "imported", runbookProductCount, runbookEdgeCount, 3)
			projected = append(projected, r.emit(t, files, path)...)
		}
		runbookEqualJSONL(t, r.readProducts(t, id, rev, stores...).projected, projected)
		t.Logf("three stores in %s hold %d products and %d observations", runbookDomain, len(stores)*runbookProductCount, len(stores)*runbookEdgeCount)
	})
}

func runbookRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func runbookDecode(t *testing.T, data []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatal(err)
	}
}

func runbookJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := jsonv2.Marshal(value, jsonv2.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func runbookObject(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var row map[string]any
	runbookDecode(t, line, &row)
	return row
}

func runbookOracle(t *testing.T, frozen []byte, keys []productobs.ReleaseTriple) []byte {
	t.Helper()
	selected := make(map[productobs.ReleaseTriple]bool, len(keys))
	for _, key := range keys {
		selected[key] = true
	}
	var out bytes.Buffer
	for line := range bytes.SplitSeq(bytes.TrimSuffix(frozen, []byte("\n")), []byte("\n")) {
		row := runbookObject(t, line)
		switch row["type"] {
		case "product":
			delete(row, "partition")
			delete(row, "page_present")
		case "obs":
			store, _, _ := strings.Cut(row["product_id"].(string), ":")
			if !selected[productobs.ReleaseTriple{Store: store, Feature: row["feature"].(string), SourceKey: row["source_key"].(string)}] {
				continue
			}
		default:
			t.Fatalf("unknown frozen observation row type %v", row["type"])
		}
		out.Write(runbookJSON(t, row))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func runbookEqualJSONL(t *testing.T, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	i := 1
	for len(got) > 0 && len(want) > 0 {
		gotLine, rest, _ := bytes.Cut(got, []byte("\n"))
		wantLine, remaining, _ := bytes.Cut(want, []byte("\n"))
		if !bytes.Equal(gotLine, wantLine) {
			t.Fatalf("line %d differs\n got: %.512s\nwant: %.512s", i, gotLine, wantLine)
		}
		got, want = rest, remaining
		i++
	}
	t.Fatalf("JSONL differs after %d matching lines (%d vs %d remaining bytes)", i-1, len(got), len(want))
}

func runbookFilterProjection(t *testing.T, projection []byte, keep func(map[string]any) bool) ([]byte, int, int) {
	t.Helper()
	var out bytes.Buffer
	var products, edges int
	for line := range bytes.SplitSeq(bytes.TrimSuffix(projection, []byte("\n")), []byte("\n")) {
		row := runbookObject(t, line)
		if !keep(row) {
			continue
		}
		if row["type"] == "product" {
			products++
		} else {
			edges++
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), products, edges
}

func runbookRemoveProducts(t *testing.T, records []byte) ([]byte, map[string]bool) {
	t.Helper()
	rows := bytes.SplitN(records, []byte("\n"), 11)
	if len(rows) != 11 || len(rows[10]) == 0 {
		t.Fatal("A1 input must contain more than ten products")
	}
	removed := make(map[string]bool, 10)
	for _, line := range rows[:10] {
		row := runbookObject(t, line)
		removed[row["product_id"].(string)] = true
	}
	return rows[10], removed
}

func runbookRemapRegistry(t *testing.T, data []byte, keys []productobs.ReleaseTriple) ([]byte, []productobs.ReleaseTriple) {
	t.Helper()
	registry := runbookObject(t, data)
	ids := make(map[string]string)
	for _, raw := range registry["features"].([]any) {
		feature := raw.(map[string]any)
		id := feature["id"].(string)
		ids[id] = "runbook_" + id
		feature["id"] = ids[id]
	}
	for _, raw := range registry["stores"].(map[string]any) {
		store := raw.(map[string]any)
		for _, rawMapping := range store["keys"].(map[string]any) {
			mapping := rawMapping.(map[string]any)
			mapping["feature"] = ids[mapping["feature"].(string)]
		}
	}
	keys = slices.Clone(keys)
	for i := range keys {
		keys[i].Feature = ids[keys[i].Feature]
	}
	return runbookJSON(t, registry), keys
}

func runbookCloneRegistry(t *testing.T, data []byte, keys []productobs.ReleaseTriple, stores []string) ([]byte, []productobs.ReleaseTriple) {
	t.Helper()
	registry := runbookObject(t, data)
	original := registry["stores"].(map[string]any)[runbookStore]
	clones := make(map[string]any, len(stores))
	var cloned []productobs.ReleaseTriple
	for _, store := range stores {
		clones[store] = original
		for _, key := range keys {
			if key.Store != runbookStore {
				t.Fatalf("unexpected store in frozen release: %s", key.Store)
			}
			key.Store = store
			cloned = append(cloned, key)
		}
	}
	registry["stores"] = clones
	return runbookJSON(t, registry), cloned
}

func runbookCloneRecords(t *testing.T, records []byte, store string) []byte {
	t.Helper()
	var out bytes.Buffer
	for line := range bytes.SplitSeq(bytes.TrimSuffix(records, []byte("\n")), []byte("\n")) {
		row := runbookObject(t, line)
		id, ok := strings.CutPrefix(row["product_id"].(string), runbookStore+":")
		if !ok || row["store"] != runbookStore {
			t.Fatal("A1 input has a product outside the frozen store")
		}
		row["product_id"], row["store"] = store+":"+id, store
		out.Write(runbookJSON(t, row))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func runbookSyntheticRelease(t *testing.T, registry []byte, keys []productobs.ReleaseTriple) runbookReleaseFiles {
	t.Helper()
	dir := t.TempDir()
	candidates := string(runbookJSON(t, runbookCandidates{RegistrySHA: productFixtureDigest(string(registry)), Keys: keys}))
	pins := make(map[string]string)
	for name, body := range productFixturePins(string(registry), candidates) {
		pins[name] = writeProductFixture(t, dir, name, body)
	}
	return runbookReleaseFiles{
		registry: pins["R.json"], candidates: writeProductFixture(t, dir, "candidates.json", candidates), pins: pins,
	}
}

type runbookProcess struct {
	stdout, stderr bytes.Buffer
	elapsed        time.Duration
	state          *os.ProcessState
	err            error
}

func (r *productsRunbook) command(ctx context.Context, args ...string) *runbookProcess {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Dir, cmd.Env = r.dir, r.env
	result := new(runbookProcess)
	cmd.Stdout, cmd.Stderr = &result.stdout, &result.stderr
	start := time.Now()
	result.err = cmd.Run()
	result.elapsed = time.Since(start)
	result.state = cmd.ProcessState
	return result
}

func (p *runbookProcess) assertOK(t *testing.T, args []string) {
	t.Helper()
	if p.err != nil {
		t.Fatalf("gkg %v: %v\nstdout: %.2000s\nstderr: %.2000s", args, p.err, p.stdout.Bytes(), p.stderr.Bytes())
	}
}

func (r *productsRunbook) cli(t *testing.T, args ...string) *runbookProcess {
	t.Helper()
	p := r.command(t.Context(), args...)
	p.assertOK(t, args)
	return p
}

func (r *productsRunbook) emit(t *testing.T, files runbookReleaseFiles, input string) []byte {
	t.Helper()
	return r.cli(t, "products", "emit", "-registry", files.registry, "-release", files.candidates, input).stdout.Bytes()
}

func runbookField(t *testing.T, output, key string) string {
	t.Helper()
	for _, field := range strings.Fields(output) {
		if value, ok := strings.CutPrefix(field, key+"="); ok && value != "" {
			return value
		}
	}
	t.Fatalf("missing %s in %q", key, output)
	return ""
}

func (r *productsRunbook) activate(t *testing.T, files runbookReleaseFiles, stores ...string) (id, rev string) {
	t.Helper()
	reg := r.cli(t, "features", "import", files.registry).stdout.String()
	rev = runbookField(t, reg, "rev")
	args := []string{"products", "release", "create", "-registry", rev, "-candidates", files.candidates, "-approved-by", "p8-runbook"}
	for _, store := range stores {
		args = append(args, "-store-domain", store+"="+runbookDomain)
	}
	for _, name := range slices.Sorted(maps.Keys(files.pins)) {
		args = append(args, "-pin", name+"="+files.pins[name])
	}
	id = runbookField(t, r.cli(t, args...).stdout.String(), "id")
	r.cli(t, "products", "release", "activate", id)
	return id, rev
}

func (r *productsRunbook) queryRows(ctx context.Context, query string, params map[string]any) ([]*neo4j.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	session := r.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	result, err := session.Run(ctx, query, params)
	if err != nil {
		return nil, err
	}
	return result.Collect(ctx)
}

func (r *productsRunbook) query(t *testing.T, query string, params map[string]any) []*neo4j.Record {
	t.Helper()
	rows, err := r.queryRows(t.Context(), query, params)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return rows
}

func (r *productsRunbook) edgeCount(t *testing.T) int64 {
	t.Helper()
	return r.query(t, "MATCH ()-[e:HAS_FEATURE]->() RETURN count(e)", nil)[0].Values[0].(int64)
}

type runbookRowKey struct{ id, feature, source string }

type runbookProductGraph struct {
	projected  []byte
	properties map[runbookRowKey]map[string]any
}

func (r *productsRunbook) readProducts(t *testing.T, release, rev string, stores ...string) runbookProductGraph {
	t.Helper()
	type projection struct {
		key runbookRowKey
		row map[string]any
	}
	graph := runbookProductGraph{properties: make(map[runbookRowKey]map[string]any)}
	var records []projection
	add := func(key runbookRowKey, row, props map[string]any) {
		t.Helper()
		if _, ok := graph.properties[key]; ok {
			t.Fatalf("duplicate projection key: %+v", key)
		}
		graph.properties[key] = props
		records = append(records, projection{key, row})
	}
	for _, row := range r.query(t, `MATCH (p:Product)
		RETURN properties(p), labels(p), EXISTS { MATCH (p)-[r]-() WHERE type(r) <> 'HAS_FEATURE' }`, nil) {
		props := row.Values[0].(map[string]any)
		id, store := props["product_id"].(string), props["store"].(string)
		if !slices.Contains(stores, store) || !strings.HasPrefix(id, store+":") || props["domain"] != runbookDomain {
			t.Fatalf("unexpected store/domain for product %s: %s/%v", id, store, props["domain"])
		}
		if !reflect.DeepEqual(row.Values[1], []any{"Product"}) || row.Values[2] != false {
			t.Fatalf("unexpected labels or non-feature relationships for product %s: %v", id, row.Values[1:])
		}
		record := map[string]any{"type": "product"}
		for _, key := range strings.Fields("brand category link product_id product_type store subcategory title") {
			record[key] = props[key]
		}
		add(runbookRowKey{id: id}, record, props)
	}
	// Match the relation globally so an edge attached to the wrong node label or
	// a stale feature revision cannot disappear from the read-back.
	for _, row := range r.query(t, `MATCH (p)-[e:HAS_FEATURE]->(f)
		RETURN p.product_id, p:Product, f:FeatureRev, f.id, f.rev, f.key, properties(e)`, nil) {
		props := row.Values[6].(map[string]any)
		if row.Values[1] != true || row.Values[2] != true || row.Values[4] != rev || props["release_id"] != release {
			t.Fatalf("invalid HAS_FEATURE endpoints or release: %v", row.Values)
		}
		id, feature := row.Values[0].(string), row.Values[3].(string)
		if row.Values[5] != rev+"/"+feature {
			t.Fatalf("feature %s has unexpected revision key %v", feature, row.Values[5])
		}
		if _, ok := graph.properties[runbookRowKey{id: id}]; !ok {
			t.Fatalf("edge has no projected product %s", id)
		}
		record := map[string]any{"type": "obs", "product_id": id, "feature": feature, "items": nil, "typed": nil}
		for _, key := range strings.Fields("conflict raw source_key split_status") {
			record[key] = props[key]
		}
		if items, ok := props["items_json"].(string); ok {
			var parsed any
			runbookDecode(t, []byte(items), &parsed)
			record["items"] = parsed
		}
		if props["status"] != nil {
			typed := make(map[string]any)
			for _, key := range strings.Fields("canonical_unit hi hi_canonical inclusive lo lo_canonical qualifier status unit_in") {
				typed[key] = props[key]
			}
			record["typed"] = typed
		}
		add(runbookRowKey{id: id, feature: feature, source: props["source_key"].(string)}, record, props)
	}
	slices.SortFunc(records, func(a, b projection) int {
		return cmp.Or(cmp.Compare(a.key.id, b.key.id),
			cmp.Compare(a.row["type"].(string), b.row["type"].(string)),
			cmp.Compare(a.key.feature, b.key.feature), cmp.Compare(a.key.source, b.key.source))
	})
	var output bytes.Buffer
	for _, item := range records {
		output.Write(runbookJSON(t, item.row))
		output.WriteByte('\n')
	}
	graph.projected = output.Bytes()
	return graph
}

type runbookMeasurements struct {
	MemgraphSource     string                     `json:"memgraph_source"`
	MemgraphPeakScope  string                     `json:"memgraph_peak_scope"`
	SampleIntervalMS   int64                      `json:"memgraph_sample_interval_ms"`
	CLILimitBytes      int64                      `json:"cli_rss_limit_bytes"`
	MemgraphLimitBytes int64                      `json:"memgraph_limit_bytes"`
	ImportLimitSeconds float64                    `json:"import_limit_seconds_per_store"`
	Imports            []runbookImportMeasurement `json:"imports"`
}

type runbookImportMeasurement struct {
	Scale                    int     `json:"stores_in_phase"`
	Store                    string  `json:"store"`
	CLIBytes                 int64   `json:"cli_peak_rss_bytes"`
	Seconds                  float64 `json:"import_seconds"`
	MemgraphBytes            int64   `json:"memgraph_peak_bytes"`
	MemgraphPreBytes         int64   `json:"memgraph_pre_bytes"`
	MemgraphSampledPeakBytes int64   `json:"memgraph_sampled_peak_bytes"`
	MemgraphPeakDeltaBytes   int64   `json:"memgraph_peak_delta_bytes"`
	MemgraphSamples          int     `json:"memgraph_samples"`
	ExitCode                 int     `json:"exit_code"`
}

type runbookMemorySample struct {
	resident, lifetimePeak int64
}

func (r *productsRunbook) importSnapshot(t *testing.T, release, store string, seq int64, input, status string, products, edges, scale int) {
	t.Helper()
	reportPath := filepath.Join(t.TempDir(), "import.json")
	args := []string{"products", "import", "-store", store, "-release", release, "-snapshot-seq",
		strconv.FormatInt(seq, 10), "-report", reportPath, input}
	ctx, cancel := context.WithTimeout(t.Context(), runbookImportLimit)
	defer cancel()
	var p *runbookProcess
	if scale == 0 {
		p = r.command(ctx, args...)
	} else {
		p = r.measureImport(t, ctx, scale, store, args)
	}
	p.assertOK(t, args)
	var report graph.ProductImportReport
	runbookDecode(t, runbookRead(t, reportPath), &report)
	if report.Store != store || report.Release != release || report.Status != status ||
		!strings.HasPrefix(p.stdout.String(), status+" ") || report.SnapshotSeq != seq ||
		report.ProductCount != products || report.EdgeCount != edges || report.RejectionCount != 0 ||
		report.SnapshotDigest != productFixtureDigest(string(runbookRead(t, input))) {
		t.Fatalf("unexpected snapshot import report: %+v; CLI stdout %s", report, p.stdout.Bytes())
	}
}

func (r *productsRunbook) measureImport(t *testing.T, ctx context.Context, scale int, store string, args []string) *runbookProcess {
	t.Helper()
	pre, err := r.memory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source, scope := "memory_res", "import_sampled"
	if pre.lifetimePeak > 0 {
		source, scope = "peak_memory_res", "process_lifetime"
	}
	if old := r.measurements.MemgraphSource; old != "" && old != source {
		t.Fatalf("Memgraph memory statistic changed from %s to %s", old, source)
	}
	r.measurements.MemgraphSource = source
	r.measurements.MemgraphPeakScope = scope
	r.measurements.SampleIntervalMS = runbookSamplePeriod.Milliseconds()
	type samples struct {
		runbookMemorySample
		count int
		err   error
	}
	stop := make(chan struct{})
	done := make(chan samples, 1)
	ticker := time.NewTicker(runbookSamplePeriod)
	defer ticker.Stop()
	go func() {
		var measured samples
		defer func() { done <- measured }()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				value, err := r.memory(t.Context())
				if err != nil {
					measured.err = err
					return
				}
				measured.resident = max(measured.resident, value.resident)
				measured.lifetimePeak = max(measured.lifetimePeak, value.lifetimePeak)
				measured.count++
			}
		}
	}()
	p := r.command(ctx, args...)
	close(stop)
	// Include the immediate post-import boundary in the sampled peak. Count
	// the in-flight samples on their own so the polling rate is observable.
	last, err := r.memory(t.Context())
	memory := <-done
	if err != nil {
		t.Error(err)
	} else if (pre.lifetimePeak > 0) != (last.lifetimePeak > 0) {
		t.Error("Memgraph peak_memory_res availability changed during import")
	}
	peak := max(memory.resident, last.resident)
	absolutePeak := max(pre.resident, peak, pre.lifetimePeak, memory.lifetimePeak, last.lifetimePeak)
	rss, exit := int64(0), -1
	if p.state != nil {
		exit = p.state.ExitCode()
		if usage, ok := p.state.SysUsage().(*syscall.Rusage); ok {
			rss = usage.Maxrss
			if runtime.GOOS == "linux" {
				rss *= 1024
			}
		}
	}
	measured := runbookImportMeasurement{Scale: scale, Store: store, CLIBytes: rss, Seconds: p.elapsed.Seconds(),
		MemgraphBytes: absolutePeak, MemgraphPreBytes: pre.resident, MemgraphSampledPeakBytes: peak,
		MemgraphPeakDeltaBytes: peak - pre.resident, MemgraphSamples: memory.count, ExitCode: exit}
	r.measurements.Imports = append(r.measurements.Imports, measured)
	t.Logf("%dx %s: CLI %.2f MiB, wall %.3fs, Memgraph pre %.2f MiB, sampled peak %.2f MiB, delta %+.2f MiB, absolute peak %.2f MiB (%s, %d samples), exit %d",
		scale, store, float64(rss)/runbookMiB, measured.Seconds, float64(pre.resident)/runbookMiB, float64(peak)/runbookMiB,
		float64(measured.MemgraphPeakDeltaBytes)/runbookMiB, float64(absolutePeak)/runbookMiB, scope, memory.count, exit)
	if memory.err != nil {
		t.Error(memory.err)
	}
	if minSamples := int(p.elapsed / (250 * time.Millisecond)); memory.count < minSamples {
		t.Errorf("recorded %d Memgraph samples during %.3fs, want at least %d at 4 Hz", memory.count, measured.Seconds, minSamples)
	}
	if rss <= 0 || rss > runbookCLILimit || p.elapsed > runbookImportLimit || absolutePeak > runbookMemgraphLimit {
		t.Errorf("import measurement missing or over limit: %+v", measured)
	}
	return p
}

func (r *productsRunbook) memory(ctx context.Context) (runbookMemorySample, error) {
	rows, err := r.queryRows(ctx, "SHOW STORAGE INFO", nil)
	if err != nil {
		return runbookMemorySample{}, fmt.Errorf("Memgraph storage info: %w", err)
	}
	stats := make(map[string]any, len(rows))
	for _, row := range rows {
		if len(row.Values) == 2 {
			stats[fmt.Sprint(row.Values[0])] = row.Values[1]
		}
	}
	var sample runbookMemorySample
	sample.resident, err = runbookMemoryBytes(stats["memory_res"])
	if err != nil {
		return sample, fmt.Errorf("SHOW STORAGE INFO memory_res = %v: %w", stats["memory_res"], err)
	}
	if value, exists := stats["peak_memory_res"]; exists {
		sample.lifetimePeak, err = runbookMemoryBytes(value)
		if err != nil {
			return sample, fmt.Errorf("SHOW STORAGE INFO peak_memory_res = %v: %w", value, err)
		}
	}
	return sample, nil
}

func runbookMemoryBytes(value any) (int64, error) {
	raw := strings.TrimSpace(fmt.Sprint(value))
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		return n, nil
	}
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}} {
		if digits, ok := strings.CutSuffix(raw, unit.suffix); ok {
			n, err := strconv.ParseFloat(strings.TrimSpace(digits), 64)
			if err == nil && n > 0 && n <= float64(1<<50) {
				return int64(math.Ceil(n * float64(unit.bytes))), nil
			}
			break
		}
	}
	return 0, fmt.Errorf("expected positive bytes or IEC size")
}

func (r *productsRunbook) seedMixedGraph(t *testing.T, st *graph.Store) map[string]string {
	t.Helper()
	ctx := t.Context()
	const text = "Quillon uses Atlas."
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	docIDs := make(map[string]string)
	for _, slug := range []string{runbookDomain, runbookOtherDomain} {
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: slug + "-fixture.md", Hash: slug, Labels: "general"})
		must(err)
		docIDs[slug] = doc.ID
		must(st.AssignDomain(ctx, graph.Assignment{DocID: doc.ID, Domain: slug, Profile: domain.Profile{"quillon": 1}, CreatedBy: "admin",
			Decision: graph.DecisionRecord{OpID: "runbook-seed-" + slug, At: time.Unix(0, 0).UTC()}}))
		must(st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text}, {Seq: 1, Text: text}}))
		head, tail := "quillon@"+slug, "atlas@"+slug
		entities := []graph.EntityUpsert{{Key: head, Name: "Quillon", Type: "person"}, {Key: tail, Name: "Atlas", Type: "organization"}}
		chunk := doc.ID + "-c0"
		fact := graph.FactUpsert{HeadKey: head, TailKey: tail, Rel: "uses", Confidence: .9,
			HeadMention: graph.Span{Start: 0, End: 7}, TailMention: graph.Span{Start: 13, End: 18}, JudgeInput: []byte("{}")}
		pending := fact
		pending.Rel = "depends_on"
		fact.JudgeGen, pending.JudgeGen = "runbook-confirmed", "runbook-pending"
		must(st.UpsertExtractionOpts(ctx, doc.ID, chunk, "runbook:1", entities, []graph.FactUpsert{fact, pending}, graph.WriteOptions{Domain: slug, StoreJudgeInput: true}))
		candidate := fact
		candidate.Rel = "works_at"
		must(st.UpsertExtractionOpts(ctx, doc.ID, doc.ID+"-c1", "runbook:1", entities, []graph.FactUpsert{candidate}, graph.WriteOptions{Domain: slug, FactsAsCandidates: true}))
		migrationQuery(t, `MATCH (:Entity)-[f:uses {doc_id:$doc}]->(:Entity) SET f.judge='confirmed'`, map[string]any{"doc": doc.ID})
		must(st.SetStatus(ctx, doc.ID, graph.StatusDone, ""))
	}
	migrationQuery(t, `
CREATE (:Document {id:'runbook-inflight',source:'recovery.md',source_kind:'text',domain:$domain,status:'received',
                  created_at:'2026-01-01T00:00:00Z',updated_at:'2026-01-01T00:00:00Z'})
CREATE (:Entity {key:'orphan@runbook-other',name:'Orphan',type:'organization',domain:$domain})`, map[string]any{"domain": runbookOtherDomain})
	return docIDs
}

type runbookMaintenance struct {
	outputs map[string]any
	domains []graph.DomainCount
	cypher  []int
}

func (r *productsRunbook) maintenance(t *testing.T, st *graph.Store) runbookMaintenance {
	t.Helper()
	outputs := make(map[string]any)
	// Inventory of cmd/gkg's graph maintenance probes. Repair validity and
	// rejudge provide classifier/plan phases, and quality provides the read-only
	// fold check. Rebuild has no preview: eval-sample exports its document and
	// chunk inputs. Preflight and schema-only migrate exercise the current schema
	// without running extraction. Graph-free commands have no G0/G1 input.
	for _, probe := range []struct {
		name        string
		args        []string
		prefix      string
		stdoutRows  int
		files       int
		rowsPerFile int
	}{
		{"domain list", []string{"domain", "list"}, "DOMAIN\tNAME\tDOCUMENTS\tNEAREST\tSCORE\n", 3, 0, 0},
		{"domain evaluate", []string{"domain", "evaluate"}, "docs=", 1, 0, 0},
		{"repair names", []string{"repair-names", "-dry-run"}, "scanned=", 1, 0, 0},
		{"repair labels", []string{"repair-labels", "-dry-run", "-report", "out"}, "scanned_chunks=", 1, 1, 1},
		{"repair titles", []string{"repair-titles", "-out", "out"}, "total=", 1, 1, 1},
		{"repair validity", []string{"repair-validity", "-classify", "-preset", "general", "-out", "out"}, "inventory_digest=", 1, 4, 1},
		{"judge rejudge", []string{"judge", "rejudge", "-plan", "-out", "out"}, "planned=", 1, 1, 3}, // Header plus two requests.
		{"judge audit", []string{"judge", "audit", "-sample", "2", "-seed", "1", "-out", "out"}, "sampled=2 ", 1, 1, 2},
		{"quality and fold check", []string{"quality", "-json", "-fold-check", "-duplicates-out", "out"}, `{"metrics":`, 1, 1, 1},
		{"merge folds", []string{"merge-folds", "-plan", "-out", "out"}, "groups=", 1, 1, 2}, // Header and digest footer.
		{"eval-sample (bound domain)", []string{"eval-sample", "-domain", runbookDomain, "-out", "out"}, "documents=", 1, 1, 1},
		{"eval-sample (other domain)", []string{"eval-sample", "-domain", runbookOtherDomain, "-out", "out"}, "documents=", 1, 1, 1},
		{"migration preflight", []string{"preflight"}, "", 0, 0, 0},
		{"schema-only migration", []string{"migrate", "-schema-only"}, "schema ok\n", 1, 0, 0},
	} {
		cmd := *r
		cmd.dir = t.TempDir()
		p := cmd.cli(t, probe.args...)
		if probe.stdoutRows == 0 {
			// Preflight's successful current-schema path emits no output.
			current, err := st.ExtractionSchemaCurrent(t.Context())
			if err != nil || !current || p.stdout.Len() != 0 || p.stderr.Len() != 0 {
				t.Fatalf("migration preflight: want a current schema and silent success, got current=%t, error=%v, stdout=%q, stderr=%q",
					current, err, p.stdout.String(), p.stderr.String())
			}
		} else if !strings.HasPrefix(p.stdout.String(), probe.prefix) || runbookNonemptyLines(p.stdout.Bytes()) < probe.stdoutRows {
			t.Fatalf("%s: want stdout prefix %q and at least %d non-empty lines, got %.1000q",
				probe.name, probe.prefix, probe.stdoutRows, p.stdout.String())
		}
		artifact := make(map[string]string)
		err := filepath.WalkDir(cmd.dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			name, err := filepath.Rel(cmd.dir, path)
			if err != nil {
				return err
			}
			data := runbookRead(t, path)
			if got := runbookNonemptyLines(data); got < probe.rowsPerFile {
				t.Fatalf("%s artifact %s: got %d non-empty lines, want at least %d", probe.name, name, got, probe.rowsPerFile)
			}
			if probe.name == "merge folds" {
				data = runbookMergePlan(t, data)
			}
			artifact[name] = string(data)
			return nil
		})
		if err != nil || len(artifact) != probe.files {
			t.Fatalf("%s: found %d artifacts, want %d: %v", probe.name, len(artifact), probe.files, err)
		}
		outputs[probe.name] = struct {
			stdout, stderr string
			files          map[string]string
		}{p.stdout.String(), p.stderr.String(), artifact}
	}

	// Backfill (both modes), GC and recovery have no CLI dry-run. Inspect
	// the actual backfill/recovery selectors and preview GC's orphan selection.
	ctx := t.Context()
	candidates, err := st.UnjudgedCandidates(ctx, "", 0)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("expected two candidate backfill rows, got %d: %v", len(candidates), err)
	}
	outputs["judge candidate backfill selector"] = candidates
	pending, err := st.PendingStoredJudgeEdges(ctx, "", nil, 100)
	if err != nil || len(pending) != 2 {
		t.Fatalf("expected two stored-edge backfill rows, got %d: %v", len(pending), err)
	}
	outputs["judge edge backfill selector"] = pending
	inFlight, err := st.InFlight(ctx)
	if err != nil || len(inFlight) != 1 || inFlight[0].ID != "runbook-inflight" {
		t.Fatalf("recovery selector returned %v: %v", inFlight, err)
	}
	outputs["recovery selector"] = inFlight
	orphans := r.query(t, `MATCH (e:Entity) WHERE NOT EXISTS { MATCH (e)--() } RETURN e.key ORDER BY e.key`, nil)
	if len(orphans) != 1 || orphans[0].Values[0] != "orphan@runbook-other" {
		t.Fatal("GC preview must identify the one seeded orphan")
	}
	outputs["entity GC preview"] = orphans

	handler := api.NewServer(config.Config{}, st, nil).Handler()
	documents := r.query(t, "MATCH (d:Document) RETURN d.id ORDER BY d.id", nil)
	if len(documents) != 3 {
		t.Fatalf("document status: want three fixture documents, got %d", len(documents))
	}
	for _, row := range documents {
		id := row.Values[0].(string)
		path := "/v1/documents/" + url.PathEscape(id) + "/status"
		data := runbookHTTP(t, handler, http.MethodGet, path, nil, http.StatusOK)
		var status struct {
			ID     string       `json:"id"`
			Status graph.Status `json:"status"`
		}
		runbookDecode(t, data, &status)
		want := graph.StatusDone
		if id == "runbook-inflight" {
			want = graph.StatusReceived
		}
		if status.ID != id || status.Status != want {
			t.Fatalf("document status %s: want %s, got %s", id, want, data)
		}
		outputs["document status "+id] = data
	}
	var domains []graph.DomainCount
	runbookDecode(t, runbookHTTP(t, handler, http.MethodGet, "/v1/domains", nil, http.StatusOK), &domains)
	for _, d := range domains {
		// The first JSONL row is the same DomainCount shape as the API.
		cmd := r.cli(t, "domain", "show", d.ID)
		head, ledger, found := bytes.Cut(cmd.stdout.Bytes(), []byte("\n"))
		var shown graph.DomainCount
		runbookDecode(t, head, &shown)
		if !found || shown != d || runbookNonemptyLines(ledger) < 1 || cmd.stderr.Len() != 0 {
			t.Fatalf("domain show %s must match /v1/domains and include at least one ledger row", d.ID)
		}
		outputs["domain show ledger "+d.ID] = string(ledger)
	}
	query := map[string]string{"query": `MATCH (n)
		WITH count(CASE WHEN n:Product THEN 1 END) AS products, count(CASE WHEN n:Document THEN 1 END) AS documents
		OPTIONAL MATCH ()-[r:HAS_FEATURE]->() RETURN products,documents,count(r) AS features`}
	var result struct {
		Columns []string         `json:"columns"`
		Rows    []map[string]int `json:"rows"`
	}
	runbookDecode(t, runbookHTTP(t, handler, http.MethodPost, "/v1/cypher", query, http.StatusOK), &result)
	if !slices.Equal(result.Columns, []string{"products", "documents", "features"}) || len(result.Rows) != 1 || len(result.Rows[0]) != 3 {
		t.Fatalf("malformed Cypher count result: %+v", result)
	}
	return runbookMaintenance{outputs: outputs, domains: domains,
		cypher: []int{result.Rows[0]["products"], result.Rows[0]["documents"], result.Rows[0]["features"]}}
}

func runbookNonemptyLines(data []byte) int {
	count := 0
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) != 0 {
			count++
		}
	}
	return count
}

// Each merge plan stamps its generation time and seals the actual bytes.
// Verify the seal before comparing plan contents without the clock value.
func runbookMergePlan(t *testing.T, data []byte) []byte {
	t.Helper()
	footerOffset := bytes.LastIndexByte(bytes.TrimSuffix(data, []byte("\n")), '\n')
	if footerOffset < 0 {
		t.Fatal("merge-folds report is missing its digest footer")
	}
	payload := data[:footerOffset+1]
	var footer struct {
		SHA256 string `json:"sha256"`
	}
	runbookDecode(t, data[footerOffset+1:], &footer)
	if footer.SHA256 != productFixtureDigest(string(payload)) {
		t.Fatal("merge-folds report digest does not match the payload")
	}
	headerLine, rows, _ := bytes.Cut(payload, []byte("\n"))
	header := runbookObject(t, headerLine)
	stamp, _ := header["generated_at"].(string)
	if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
		t.Fatalf("merge-folds report generation time: %v", err)
	}
	delete(header, "generated_at")
	return append(append(runbookJSON(t, header), '\n'), rows...)
}

func (r *productsRunbook) compareMaintenance(t *testing.T, before, after runbookMaintenance) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(before.outputs)) {
		if !reflect.DeepEqual(before.outputs[name], after.outputs[name]) {
			t.Errorf("%s output changed after adding products", name)
		} else {
			t.Logf("%s output unchanged", name)
		}
	}
	if len(before.outputs) != len(after.outputs) {
		t.Error("maintenance output inventory changed")
	}
	// The CLI list (document counts) is byte-identical. API and CLI show may
	// add only the product count and binding flag of the bound domain.
	want := []graph.DomainCount{
		{ID: runbookDomain, Name: runbookDomain, Documents: 1},
		{ID: runbookOtherDomain, Name: runbookOtherDomain, Documents: 1},
	}
	if !slices.Equal(before.domains, want) {
		t.Fatalf("G0 domains = %+v; want %+v", before.domains, want)
	}
	want[0].Products, want[0].ProductsBound = runbookProductCount, true
	if !slices.Equal(after.domains, want) {
		t.Errorf("G1 domains = %+v; want %+v", after.domains, want)
	}
	if !slices.Equal(before.cypher, []int{0, 3, 0}) || !slices.Equal(after.cypher, []int{runbookProductCount, 3, runbookEdgeCount}) {
		t.Errorf("unexpected Cypher products/documents/features delta: %v → %v", before.cypher, after.cypher)
	}
}

func (r *productsRunbook) domainConflicts(t *testing.T, st *graph.Store, docs map[string]string, bound bool) {
	t.Helper()
	cfg := config.Config{}
	handler := api.NewServer(cfg, st, ingest.NewPipeline(cfg, st, nil, nil, nil)).Handler()
	for _, op := range []struct {
		action        string
		docs, domains []string
		body          map[string]any
	}{
		{"move", []string{docs[runbookOtherDomain]}, []string{runbookDomain}, map[string]any{"document_ids": []string{docs[runbookOtherDomain]}, "domain": runbookDomain}},
		{"move", []string{docs[runbookDomain]}, []string{runbookOtherDomain}, map[string]any{"document_ids": []string{docs[runbookDomain]}, "domain": runbookOtherDomain}},
		{"merge", nil, []string{runbookDomain, runbookOtherDomain}, map[string]any{"from": runbookDomain, "into": runbookOtherDomain}},
		{"merge", nil, []string{runbookOtherDomain, runbookDomain}, map[string]any{"from": runbookOtherDomain, "into": runbookDomain}},
	} {
		// In G0, compare the real read-only preflight so an allowed operation
		// cannot move or re-extract the seed. In G1 exercise the real handler.
		err := st.RequireUnboundDomains(t.Context(), op.domains, op.docs)
		if !bound {
			if err != nil {
				t.Fatalf("G0 should admit %s: %v", op.action, err)
			}
			continue
		}
		if !errors.Is(err, graph.ErrDomainHasProducts) {
			t.Fatalf("G1 %s preflight: %v", op.action, err)
		}
		var resp struct {
			Error string `json:"error"`
		}
		data := runbookHTTP(t, handler, http.MethodPost, "/v1/admin/domains/"+op.action, op.body, http.StatusConflict)
		runbookDecode(t, data, &resp)
		if resp.Error != "domain_has_products" {
			t.Fatalf("G1 %s conflict = %s", op.action, data)
		}
	}
}

func runbookHTTP(t *testing.T, handler http.Handler, method, path string, body any, status int) []byte {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(runbookJSON(t, body)))
	request.RemoteAddr = "127.0.0.1:1"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != status {
		t.Fatalf("%s %s returned %d, want %d: %s", method, path, response.Code, status, response.Body)
	}
	return response.Body.Bytes()
}
