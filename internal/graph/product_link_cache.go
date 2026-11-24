package graph

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	productLinkBuildTimeout   = 30 * time.Second
	productLinkFailureBackoff = 5 * time.Second
)

type productLinkToken struct {
	release       string
	activationSeq int64
	stores        []ProductMentionStore // ordered by store
}

func txProductLinkToken(ctx context.Context, tx neo4j.ManagedTransaction) (productLinkToken, error) {
	rows, err := txRows(ctx, tx, productStoresMatch+`
RETURN m.value AS release, coalesce(m.activation_seq, 0) AS activation_seq, `+mentionStoreColumns+`
ORDER BY s.store`, nil)
	if err != nil || len(rows) == 0 {
		return productLinkToken{}, err
	}
	token := productLinkToken{
		release:       getString(rows[0], "release"),
		activationSeq: productProp[int64](rows[0].AsMap(), "activation_seq"),
	}
	for _, row := range rows {
		if domain, _ := row.Get("domain"); domain != nil {
			token.stores = append(token.stores, readMentionStore(row))
		}
	}
	return token, nil
}

func (t productLinkToken) notOlderThan(other productLinkToken) bool {
	// A push can change the product count. The watermarks order those changes.
	return t.release == other.release && t.activationSeq >= other.activationSeq &&
		slices.EqualFunc(t.stores, other.stores, func(a, b ProductMentionStore) bool {
			return a.Store == b.Store && a.Domain == b.Domain && a.SeenSeq >= b.SeenSeq &&
				a.SnapshotSeq >= b.SnapshotSeq && a.WipedSeq >= b.WipedSeq &&
				sameOptional(a.ReleaseID, b.ReleaseID) && sameOptional(a.MaterializedActivationSeq, b.MaterializedActivationSeq)
		})
}

func sameOptional[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

type productLinkSnapshot struct {
	token productLinkToken
	index *ProductLinkIndex
}

type productLinkBuild struct {
	done   chan struct{}
	cancel context.CancelFunc

	// Published by closing done.
	snapshot productLinkSnapshot
	err      error
}

// productLinkCache keeps the last published snapshot and coordinates builds
// without holding the store write lock or a caller's read transaction.
type productLinkCache struct {
	mu       sync.Mutex
	snapshot productLinkSnapshot
	current  *productLinkBuild
	closed   bool
	retryAt  time.Time
	lastErr  error
}

var errProductLinkTokenChanged = errors.New("product link index version changed during load")

func (c *productLinkCache) load(ctx context.Context,
	readToken func(context.Context) (productLinkToken, error),
	build func(context.Context) (productLinkSnapshot, error),
) (*ProductLinkIndex, error) {
	for range 2 {
		token, err := readToken(ctx)
		if err != nil {
			return nil, err
		}
		index, err := c.loadAtLeast(ctx, token, build)
		if err != errProductLinkTokenChanged {
			return index, err
		}
	}
	return nil, errProductLinkTokenChanged
}

func (c *productLinkCache) loadAtLeast(ctx context.Context, token productLinkToken,
	build func(context.Context) (productLinkSnapshot, error),
) (*ProductLinkIndex, error) {
	for range 3 {
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		if c.closed {
			c.mu.Unlock()
			return nil, context.Canceled
		}
		if c.snapshot.index != nil && c.snapshot.token.notOlderThan(token) {
			index := c.snapshot.index
			c.mu.Unlock()
			return index, nil
		}

		flight := c.current
		if flight == nil {
			if time.Now().Before(c.retryAt) {
				err := c.lastErr
				c.mu.Unlock()
				return nil, err
			}
			buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), productLinkBuildTimeout)
			flight = &productLinkBuild{done: make(chan struct{}), cancel: cancel}
			c.current = flight
			go c.runBuild(buildCtx, flight, build)
		}
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if flight.err != nil {
			return nil, flight.err
		}
		if flight.snapshot.token.notOlderThan(token) {
			return flight.snapshot.index, nil
		}
		if !token.notOlderThan(flight.snapshot.token) {
			return nil, errProductLinkTokenChanged
		}
		// This flight read an older snapshot. Keep the caller's token as
		// the lower bound for the next flight.
	}
	return nil, errProductLinkTokenChanged
}

func (c *productLinkCache) runBuild(ctx context.Context, flight *productLinkBuild,
	build func(context.Context) (productLinkSnapshot, error),
) {
	snapshot, err := build(ctx)
	c.mu.Lock()
	if err == nil {
		err = ctx.Err()
	}
	flight.cancel()
	flight.err = err
	if err != nil {
		c.retryAt = time.Now().Add(productLinkFailureBackoff)
		c.lastErr = err
	} else {
		// Builds are serialized. A change of binding or release can make
		// the result incomparable with the previous snapshot.
		if c.snapshot.index == nil || !c.snapshot.token.notOlderThan(snapshot.token) {
			c.snapshot = snapshot
		}
		flight.snapshot = c.snapshot
		c.retryAt = time.Time{}
		c.lastErr = nil
	}
	c.current = nil
	close(flight.done)
	c.mu.Unlock()
}

// stop cancels the detached build and waits for it within the shutdown context.
func (c *productLinkCache) stop(ctx context.Context) {
	c.mu.Lock()
	c.closed = true
	flight := c.current
	if flight != nil {
		flight.cancel()
	}
	c.mu.Unlock()
	if flight != nil {
		select {
		case <-flight.done:
		case <-ctx.Done():
			slog.Warn("graph: stopped waiting for product link index build during shutdown", "err", ctx.Err())
		}
	}
}

// LoadProductLinkIndex checks the catalogue version before reusing the shared
// immutable snapshot. An unavailable leg does not prevent loading other legs.
func (s *Store) LoadProductLinkIndex(ctx context.Context) (*ProductLinkIndex, error) {
	return s.productLinks.load(ctx, s.readProductLinkToken, func(ctx context.Context) (productLinkSnapshot, error) {
		return s.buildProductLinkSnapshot(ctx, maxProductMentionValues)
	})
}

// WarmProductLinks loads the read-time product link index within the build deadline.
func (s *Store) WarmProductLinks(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, productLinkBuildTimeout)
	defer cancel()
	_, err := s.LoadProductLinkIndex(ctx)
	return err
}

func (s *Store) buildProductLinkSnapshot(ctx context.Context, limit int) (productLinkSnapshot, error) {
	return s.buildProductLinkSnapshotWithLimit(ctx, limit, maxProductLinkRows)
}

func (s *Store) buildProductLinkSnapshotWithLimit(ctx context.Context, codeLimit, linkLimit int) (productLinkSnapshot, error) {
	var token productLinkToken
	var observations []ProductIdentifierObservation
	index := &ProductLinkIndex{}
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		var err error
		token, err = txProductLinkToken(ctx, tx)
		if err != nil || len(token.stores) == 0 {
			return err
		}
		if token.release != "" {
			names := make([]string, len(token.stores))
			for i, store := range token.stores {
				names[i] = store.Store
			}
			observations, err = txLoadMentionIdentifiers(ctx, tx, map[string]any{
				"stores": names, "release": token.release,
			}, codeLimit)
			if errors.Is(err, ErrProductMentionIndexTooLarge) {
				index.codeErr = err
			} else if err != nil {
				return err
			}
		}
		return txLoadProductLinkOwners(ctx, tx, token.stores, index, linkLimit)
	})
	if err != nil {
		return productLinkSnapshot{}, err
	}
	if index.codeErr == nil {
		index.code, err = buildProductMentionIndex(ctx, observations, true)
		if err != nil {
			return productLinkSnapshot{}, err
		}
	}
	slog.DebugContext(ctx, "built product link index", "page_ambiguous", index.pageAmbiguous)
	if index.codeErr != nil || index.urlOff || index.pageOff || index.productsOff || index.searchOff {
		slog.WarnContext(ctx, "product link index cap reached",
			"code", index.codeErr, "url_off", index.urlOff, "page_off", index.pageOff,
			"products_off", index.productsOff, "search_off", index.searchOff)
	}
	return productLinkSnapshot{token: token, index: index}, nil
}
