package graph

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestProductLinkCacheCatalogueLifecycle(t *testing.T) {
	f := newPushFixtureWithFeature(t, "catalogue_code")
	ctx := t.Context()
	count := countProductReadStatements(t, f.store)
	const text = "AA12345, BB12345, CC12345, AM12345"
	read := func(label string, statements []int, keys ...string) *ProductMentionIndex {
		t.Helper()
		before := len(count.statements)
		index, err := f.store.LoadProductMentionIndex(ctx)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got := count.statements[before:]; !slices.Equal(got, statements) {
			t.Fatalf("%s: statements %v, want %v", label, got, statements)
		}
		var actual []string
		for _, match := range index.Match(text) {
			actual = append(actual, match.Key)
		}
		if !slices.Equal(actual, keys) {
			t.Fatalf("%s: keys %v, want %v", label, actual, keys)
		}
		return index
	}
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	warm, rebuilt := []int{1}, []int{1, 3}
	f.snapshot(pushA, 1, "imported",
		pushRecord(pushOne, "Original", "null", `{"scalar":"AA12345", "list":"CC-12345/AM12345/AA-12345"}`),
		pushRecord(pushTwo, "Ambiguous", "null", `{"scalar":"AM-12345"}`))
	read("initial import", rebuilt, "AA12345", "CC12345")
	cached := read("unchanged token", warm, "AA12345", "CC12345")
	fresh, err := f.other.loadProductMentionIndex(ctx, maxProductMentionValues)
	require(err)
	if !slices.Equal(slices.Sorted(maps.Keys(cached.keys)), slices.Sorted(maps.Keys(fresh.keys))) ||
		!reflect.DeepEqual(cached.Match(text), fresh.Match(text)) {
		t.Fatalf("cached and fresh indexes differ: cached matches %+v, fresh matches %+v", cached.Match(text), fresh.Match(text))
	}
	registry := strings.Replace(f.registry, `"name":"Code"`, `"name":"Updated code label"`, 1)
	imported, err := f.store.ImportFeatureRegistry(ctx, []byte(registry))
	require(err)
	if imported.Rev == f.rev {
		t.Fatalf("registry fixture did not change revision: %q", imported.Rev)
	}
	read("registry-only import", warm, "AA12345", "CC12345")

	f.put(2, pushRecord(pushOne, "Pushed", "null", `{"scalar":"BB12345"}`), "updated")
	read("push", rebuilt, "BB12345", "AM12345")
	f.remove(3, pushOne, "deleted")
	read("delete", rebuilt, "AM12345")
	f.snapshot(pushA, 4, "imported", pushRecord(pushOne, "Imported", "null", `{"scalar":"CC12345"}`))
	read("replacement import", rebuilt, "CC12345")
	f.activate(pushB)
	f.activate(pushA)
	read("activation sequence changed with the same active release", rebuilt, "CC12345")
	f.activate(pushB)
	read("different release activated without import", rebuilt)
	require(f.store.DeleteProductStore(ctx, pushStore))
	read("wipe", rebuilt)
	require(f.store.UnbindProductStore(ctx, pushStore))
	read("unbind", []int{1, 1})
	require(f.store.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		return txBindProductStore(ctx, tx, pushStore, "push-rebound")
	}))
	read("rebind", rebuilt)
}

func TestProductLinkCacheNewerTokenDuringBuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cache productLinkCache
		readCount, builds := 0, 0
		token := productLinkToken{release: pushA, stores: []ProductMentionStore{{Store: pushStore, SeenSeq: 1}}}
		readToken := func(context.Context) (productLinkToken, error) {
			readCount++
			return token, nil
		}
		firstIndex, secondIndex := &ProductLinkIndex{}, &ProductLinkIndex{}
		resume := make(chan struct{})
		finish := sync.OnceFunc(func() { close(resume) })
		defer finish()
		build := func(context.Context) (productLinkSnapshot, error) {
			builds++
			captured := token
			if builds == 1 {
				<-resume
				return productLinkSnapshot{token: captured, index: firstIndex}, nil
			}
			return productLinkSnapshot{token: captured, index: secondIndex}, nil
		}
		var waiters sync.WaitGroup
		load := func(want *ProductLinkIndex) {
			waiters.Go(func() {
				if got, err := cache.load(t.Context(), readToken, build); err != nil || got != want {
					t.Errorf("index = %p, err = %v, want %p", got, err, want)
				}
			})
		}
		load(firstIndex)
		synctest.Wait()
		token.stores = []ProductMentionStore{{Store: pushStore, SeenSeq: 2}}
		load(secondIndex)
		synctest.Wait()
		if builds != 1 || readCount != 2 {
			t.Errorf("before releasing first build: builds = %d, token reads = %d, want 1 and 2", builds, readCount)
		}
		finish()
		waiters.Wait()
		if builds != 2 || readCount != 2 {
			t.Errorf("after both loads: builds = %d, token reads = %d, want 2 and 2", builds, readCount)
		}
	})
}

func TestProductLinkCachePushBetweenTokenAndBuild(t *testing.T) {
	f := newPushFixtureWithFeature(t, "catalogue_code")
	ctx := t.Context()
	f.snapshot(pushA, 1, "imported", pushRecord(pushOne, "Original", "null", `{"scalar":"AA12345"}`))
	old, err := f.store.readProductLinkToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.put(2, pushRecord(pushTwo, "Added", "null", `{"scalar":"BB12345"}`), "created")

	reads, builds := 0, 0
	readToken := func(context.Context) (productLinkToken, error) {
		reads++
		return old, nil
	}
	build := func(ctx context.Context) (productLinkSnapshot, error) {
		builds++
		return f.store.buildProductLinkSnapshot(ctx, maxProductMentionValues)
	}
	index, err := f.store.productLinks.load(ctx, readToken, build)
	if err != nil {
		t.Fatal(err)
	}
	if matches := index.code.Match("AA12345, BB12345"); len(matches) != 2 || matches[1].Product.Title != "Added" {
		t.Fatalf("matches = %+v, want both products", matches)
	}
	if warm, err := f.store.productLinks.load(ctx, readToken, build); err != nil || warm != index {
		t.Fatalf("warm load for the overtaken token: index = %p, err = %v, want %p", warm, err, index)
	}
	if reads != 2 || builds != 1 {
		t.Errorf("builds = %d, token reads = %d, want 1 and 2 across both loads", builds, reads)
	}
}

func TestProductLinkCachePushDuringBuild(t *testing.T) {
	f := newPushFixtureWithFeature(t, "catalogue_code")
	f.snapshot(pushA, 1, "imported", pushRecord(pushOne, "Original", "null", `{"scalar":"AA12345"}`))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	captured, resume := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(resume) })
	var readers sync.WaitGroup
	defer func() {
		finish()
		readers.Wait()
	}()
	var builds atomic.Int32
	build := func(ctx context.Context) (productLinkSnapshot, error) {
		n := builds.Add(1)
		snapshot, err := f.store.buildProductLinkSnapshot(ctx, maxProductMentionValues)
		if n == 1 {
			close(captured)
			<-resume
		}
		return snapshot, err
	}
	check := func(readToken func(context.Context) (productLinkToken, error), want string) {
		t.Helper()
		index, err := f.store.productLinks.load(ctx, readToken, build)
		if err != nil {
			t.Errorf("load for %s: %v", want, err)
			return
		}
		if matches := index.code.Match("AA12345 BB12345"); len(matches) != 1 || matches[0].Key != want {
			t.Errorf("matches = %+v, want %s", matches, want)
		}
	}
	readers.Go(func() { check(f.store.readProductLinkToken, "AA12345") })
	select {
	case <-captured:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	f.put(2, pushRecord(pushOne, "Updated", "null", `{"scalar":"BB12345"}`), "updated")
	tokenRead := make(chan struct{})
	notifyRead := sync.OnceFunc(func() { close(tokenRead) })
	readers.Go(func() {
		check(func(ctx context.Context) (productLinkToken, error) {
			token, err := f.store.readProductLinkToken(ctx)
			notifyRead()
			return token, err
		}, "BB12345")
	})
	select {
	case <-tokenRead:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	finish()
	readers.Wait()
	if got := builds.Load(); got != 2 {
		t.Errorf("builds = %d, want one build for each sequence", got)
	}
}

func TestProductLinkCacheRebindDuringBuild(t *testing.T) {
	f := newPushFixtureWithFeature(t, "catalogue_code")
	f.snapshot(pushA, 1, "imported", pushRecord(pushOne, "Original", "null", `{"scalar":"AA12345"}`))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started, resume := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(resume) })
	defer finish()

	reads, builds := 0, 0
	readToken := func(ctx context.Context) (productLinkToken, error) {
		reads++
		return f.store.readProductLinkToken(ctx)
	}
	build := func(ctx context.Context) (productLinkSnapshot, error) {
		builds++
		if builds == 1 {
			close(started)
			<-resume
		}
		return f.store.buildProductLinkSnapshot(ctx, maxProductMentionValues)
	}
	type loadResult struct {
		index *ProductLinkIndex
		err   error
	}
	done := make(chan loadResult, 1)
	go func() {
		index, err := f.store.productLinks.load(ctx, readToken, build)
		done <- loadResult{index, err}
	}()
	select {
	case <-started:
	case result := <-done:
		t.Fatalf("load returned before build: %v", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	if err := f.other.DeleteProductStore(ctx, pushStore); err != nil {
		t.Fatal(err)
	}
	if err := f.other.UnbindProductStore(ctx, pushStore); err != nil {
		t.Fatal(err)
	}
	if err := f.other.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		return txBindProductStore(ctx, tx, pushStore, "push-rebound")
	}); err != nil {
		t.Fatal(err)
	}
	finish()

	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if matches := result.index.code.Match("AA12345"); len(matches) != 0 {
		t.Fatalf("matches = %+v, want an empty index after wipe and rebind", matches)
	}
	if reads != 2 || builds != 1 {
		t.Errorf("builds = %d, token reads = %d, want 1 and 2", builds, reads)
	}
}

func TestProductLinkCacheRereadsIncomparableTokenOnce(t *testing.T) {
	var cache productLinkCache
	reads, builds := 0, 0
	readToken := func(context.Context) (productLinkToken, error) {
		reads++
		return productLinkToken{release: pushA}, nil
	}
	build := func(context.Context) (productLinkSnapshot, error) {
		builds++
		return productLinkSnapshot{token: productLinkToken{release: pushB}, index: &ProductLinkIndex{}}, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	index, err := cache.load(ctx, readToken, build)
	if index != nil || !errors.Is(err, errProductLinkTokenChanged) || reads != 2 || builds != 2 {
		t.Errorf("index = %p, err = %v, reads = %d, builds = %d, want version error after two attempts", index, err, reads, builds)
	}
}

func TestProductLinkCacheLoadAtLeastCapsStaleBuilds(t *testing.T) {
	var cache productLinkCache
	token := productLinkToken{release: pushA, activationSeq: 1}
	stale := productLinkToken{release: pushA}
	index := &ProductLinkIndex{}
	builds := 0
	build := func(context.Context) (productLinkSnapshot, error) {
		builds++
		if builds == 4 {
			return productLinkSnapshot{token: token, index: index}, nil
		}
		return productLinkSnapshot{token: stale, index: index}, nil
	}
	got, err := cache.loadAtLeast(t.Context(), token, build)
	if got != nil || !errors.Is(err, errProductLinkTokenChanged) || builds != 3 {
		t.Errorf("index = %p, err = %v, builds = %d, want version error after three rounds", got, err, builds)
	}
}

func TestProductLinkCacheContinuousPushes(t *testing.T) {
	f := newPushFixtureWithFeature(t, "catalogue_code")
	original := pushRecord(pushOne, "Original", "null", `{"list":"AA12345/AC12345"}`)
	updated := pushRecord(pushOne, "Updated", "null", `{"list":"BB12345/BC12345"}`)
	f.snapshot(pushA, 1, "imported", original)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	const pushes = 24
	check := func() {
		t.Helper()
		index, err := f.store.LoadProductMentionIndex(ctx)
		if err != nil {
			t.Errorf("load while pushing: %v", err)
			return
		}
		matches := index.Match("AA12345 AC12345 BB12345 BC12345")
		if len(matches) == 2 {
			switch matches[0].Product.Title {
			case "Original":
				if matches[1].Product.Title == "Original" && matches[0].Key == "AA12345" && matches[1].Key == "AC12345" {
					return
				}
			case "Updated":
				if matches[1].Product.Title == "Updated" && matches[0].Key == "BB12345" && matches[1].Key == "BC12345" {
					return
				}
			}
		}
		t.Errorf("incomplete or mixed index: %+v", matches)
	}
	var readers sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		readers.Go(func() {
			<-start
			for range pushes {
				check()
			}
		})
	}
	close(start)
	var pushErr error
	for seq := int64(2); seq < 2+pushes; seq++ {
		record := original
		if seq%2 == 0 {
			record = updated
		}
		if _, pushErr = f.other.PushProduct(ctx, seq, []byte(record)); pushErr != nil {
			break
		}
	}
	readers.Wait()
	if pushErr != nil {
		t.Fatal(pushErr)
	}
	index, err := f.store.LoadProductMentionIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if matches := index.Match("AA12345 AC12345 BB12345 BC12345"); len(matches) != 2 || matches[0].Key != "AA12345" || matches[1].Key != "AC12345" {
		t.Errorf("final matches = %+v, want the last committed pair", matches)
	}
}

func TestProductLinkCacheDetachesBuildAndCoalescesWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cache productLinkCache
		token := productLinkToken{release: pushA}
		ready := make(chan struct{})
		release := sync.OnceFunc(func() { close(ready) })
		defer release()
		index := &ProductLinkIndex{}
		builds := 0
		build := func(ctx context.Context) (productLinkSnapshot, error) {
			builds++
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) != 30*time.Second {
				t.Errorf("detached build time limit = %v, want 30s", time.Until(deadline))
			}
			<-ready
			return productLinkSnapshot{token: token, index: index}, ctx.Err()
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		canceled := make(chan error, 1)
		go func() {
			_, err := cache.loadAtLeast(ctx, token, build)
			canceled <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-canceled; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter returned %v", err)
		}
		deadline, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		if _, err := cache.loadAtLeast(deadline, token, build); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired waiter returned %v", err)
		}
		var waiters sync.WaitGroup
		for range 16 {
			waiters.Go(func() {
				if got, err := cache.loadAtLeast(t.Context(), token, build); err != nil || got != index {
					t.Errorf("concurrent wait failed: index = %p, err = %v", got, err)
				}
			})
		}
		synctest.Wait()
		release()
		waiters.Wait()
		if builds != 1 {
			t.Fatalf("build count = %d, want 1", builds)
		}
	})
}

func TestProductLinkCacheFailureBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cache productLinkCache
		token := productLinkToken{release: pushA, activationSeq: 1}
		index := &ProductLinkIndex{}
		retryStarted, retryReady := make(chan struct{}), make(chan struct{})
		resume := sync.OnceFunc(func() { close(retryReady) })
		defer resume()
		var builds int
		build := func(context.Context) (productLinkSnapshot, error) {
			builds++
			switch builds {
			case 1:
				return productLinkSnapshot{}, ErrProductMentionIndexTooLarge
			case 2:
				close(retryStarted)
				<-retryReady
			}
			return productLinkSnapshot{token: token, index: index}, nil
		}
		fails := func() {
			t.Helper()
			if got, err := cache.loadAtLeast(t.Context(), token, build); got != nil || !errors.Is(err, ErrProductMentionIndexTooLarge) {
				t.Fatalf("index = %p, err = %v, want the raw limit error", got, err)
			}
		}
		fails()
		fails()
		time.Sleep(5*time.Second - time.Nanosecond)
		fails()
		if builds != 1 {
			t.Fatalf("started %d builds during the backoff", builds)
		}
		time.Sleep(time.Nanosecond)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			if got, err := cache.loadAtLeast(t.Context(), token, build); got != index || err != nil {
				t.Errorf("load after backoff: index = %p, err = %v", got, err)
			}
		}()
		<-retryStarted
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			older := productLinkToken{release: pushA}
			if got, err := cache.loadAtLeast(t.Context(), older, build); got != index || err != nil {
				t.Errorf("joined retry with older token: index = %p, err = %v", got, err)
			}
		}()
		synctest.Wait()
		resume()
		<-finished
		<-joined
		if got, err := cache.loadAtLeast(t.Context(), token, build); got != index || err != nil {
			t.Fatalf("recovered index did not stay cached: index = %p, err = %v", got, err)
		}
		if builds != 2 {
			t.Fatalf("build count = %d, want one failure and one recovery", builds)
		}
	})
}

func TestProductLinkCacheStopDrainsBuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cache productLinkCache
		token := productLinkToken{release: pushA}
		canceled, release := make(chan struct{}), make(chan struct{})
		build := func(ctx context.Context) (productLinkSnapshot, error) {
			<-ctx.Done()
			close(canceled)
			<-release
			return productLinkSnapshot{}, ctx.Err()
		}
		ctx, cancel := context.WithCancel(t.Context())
		waiter := make(chan error, 1)
		go func() {
			_, err := cache.loadAtLeast(ctx, token, build)
			waiter <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-waiter; !errors.Is(err, context.Canceled) {
			t.Errorf("waiter: %v", err)
		}
		closed := make(chan struct{})
		go func() {
			cache.stop(t.Context())
			close(closed)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-closed:
			t.Error("cache stopped before the build was drained")
		default:
		}
		close(release)
		<-closed
	})
}

type productLinkCloseDriver struct {
	neo4j.Driver
	closedWith error
}

func (d *productLinkCloseDriver) Close(ctx context.Context) error {
	d.closedWith = ctx.Err()
	return nil
}

func TestStoreCloseStopsWaitingForProductLinkBuildAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		driver := &productLinkCloseDriver{}
		store := &Store{driver: driver}
		build := func(ctx context.Context) (productLinkSnapshot, error) {
			<-ctx.Done()
			time.Sleep(2 * time.Second) // Cleanup outlasts the Close deadline.
			return productLinkSnapshot{}, ctx.Err()
		}
		loaded := make(chan error, 1)
		go func() {
			_, err := store.productLinks.loadAtLeast(t.Context(), productLinkToken{release: pushA}, build)
			loaded <- err
		}()
		synctest.Wait()

		const budget = time.Second
		ctx, cancel := context.WithTimeout(t.Context(), budget)
		defer cancel()
		start := time.Now()
		if err := store.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed != budget {
			t.Errorf("Close returned after %v, want %v", elapsed, budget)
		}
		if !errors.Is(driver.closedWith, context.DeadlineExceeded) {
			t.Errorf("driver Close context error = %v, want deadline exceeded", driver.closedWith)
		}
		if err := <-loaded; !errors.Is(err, context.Canceled) {
			t.Errorf("build error = %v, want canceled", err)
		}
	})
}
