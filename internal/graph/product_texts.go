package graph

import (
	"context"
	"errors"
	"fmt"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	productTextVersion       = 1
	maxProductTextEdges      = 200_000
	maxProductTextStoreBytes = 64 << 20
	productTextBatchSize     = 512
)

type productTextCounts struct {
	count, edges, bytes int
}

func (c productTextCounts) checkCap() error {
	switch {
	case c.edges > maxProductTextEdges:
		return errors.New("text_edge_count exceeds 200,000")
	case c.bytes > maxProductTextStoreBytes:
		return errors.New("text_bytes exceeds 64 MiB")
	default:
		return nil
	}
}

func (c productTextCounts) setParams(params map[string]any) {
	params["text_count"] = int64(c.count)
	params["text_edge_count"] = int64(c.edges)
	params["text_bytes"] = int64(c.bytes)
}

type productTextEdge struct {
	product, id, key string
	truncated        bool
}

type productTextProjection struct {
	idsByText map[string]string
	edges     []productTextEdge
	bytes     int
}

func (p *productTextProjection) add(record productobs.Record, productID string) productobs.ProductTextStats {
	texts, stats := productobs.ProductTexts(record)
	if len(texts) > 0 && p.idsByText == nil {
		p.idsByText = make(map[string]string, len(texts))
	}
	for _, text := range texts {
		id, exists := p.idsByText[text.Text]
		if !exists {
			id = productobs.ProductTextID(record.Store, text.Text)
			p.idsByText[text.Text] = id
			p.bytes += len(text.Text)
		}
		p.edges = append(p.edges, productTextEdge{product: productID, id: id, key: text.Key, truncated: text.Truncated})
		p.bytes += len(text.Key)
	}
	return stats
}

func (p productTextProjection) counts() productTextCounts {
	return productTextCounts{count: len(p.idsByText), edges: len(p.edges), bytes: p.bytes}
}

// The orphan checks start from the ProductText and expand its incoming edges.
// Writing the pattern from (:Product) lets Memgraph plan a scan of every node
// per text, which made imports and pushes grow with the whole graph.
const deleteOrphanedStoreTextsQuery = `
MATCH (n:ProductText {store:$store})
WHERE NOT EXISTS { MATCH (n)<-[:HAS_TEXT]-(p) WHERE p:Product }
DELETE n`

const deleteOrphanedPushedTextsQuery = `
UNWIND $ids AS id
WITH DISTINCT id
MATCH (n:ProductText {id:id})
WHERE NOT EXISTS { MATCH (n)<-[:HAS_TEXT]-(p) WHERE p:Product }
WITH n, n.text AS text
DELETE n
RETURN text`

const mergeProductTextNodes = `
UNWIND $rows AS row
MERGE (n:ProductText {id:row.id})
ON CREATE SET n.store=$store, n.text=row.text, n.vec_id=row.vec_id`

func txWriteProductTexts(ctx context.Context, tx neo4j.ManagedTransaction, store string, projection productTextProjection) error {
	nodes := make([]map[string]any, 0, min(productTextBatchSize, len(projection.idsByText)))
	for value, id := range projection.idsByText {
		nodes = append(nodes, map[string]any{"id": id, "text": value, "vec_id": productobs.ProductTextVecID(value)})
		if len(nodes) == productTextBatchSize {
			if err := txRun(ctx, tx, mergeProductTextNodes, map[string]any{"store": store, "rows": nodes}); err != nil {
				return fmt.Errorf("merge product text nodes: %w", err)
			}
			nodes = nodes[:0]
		}
	}
	if len(nodes) != 0 {
		if err := txRun(ctx, tx, mergeProductTextNodes, map[string]any{"store": store, "rows": nodes}); err != nil {
			return fmt.Errorf("merge product text nodes: %w", err)
		}
	}
	for start := 0; start < len(projection.edges); start += productTextBatchSize {
		batch := projection.edges[start:min(start+productTextBatchSize, len(projection.edges))]
		rows := make([]map[string]any, 0, len(batch))
		for _, e := range batch {
			rows = append(rows, map[string]any{"product": e.product, "id": e.id, "key": e.key, "truncated": e.truncated})
		}
		created, err := txRows(ctx, tx, `
UNWIND $rows AS row
MATCH (p:Product {id:row.product}), (n:ProductText {id:row.id})
CREATE (p)-[e:HAS_TEXT {key:row.key, truncated:row.truncated}]->(n)
RETURN count(e) AS created`, map[string]any{"rows": rows})
		if err != nil {
			return fmt.Errorf("create product text edges: %w", err)
		}
		if len(created) != 1 || getInt(created[0], "created") != len(rows) {
			return errors.New("missing product or text when creating text edges")
		}
	}
	return nil
}

func txWriteSnapshotTexts(ctx context.Context, tx neo4j.ManagedTransaction, store string,
	projection productTextProjection, replaceEdges bool) error {
	params := map[string]any{"store": store}
	if replaceEdges {
		if err := txRun(ctx, tx, `MATCH (:Product {store:$store})-[e:HAS_TEXT]->() DELETE e`, params); err != nil {
			return err
		}
	}
	if err := txWriteProductTexts(ctx, tx, store, projection); err != nil {
		return err
	}
	if err := txRun(ctx, tx, deleteOrphanedStoreTextsQuery, params); err != nil {
		return fmt.Errorf("delete orphaned snapshot texts: %w", err)
	}
	projection.counts().setParams(params)
	params["text_version"] = int64(productTextVersion)
	return txRun(ctx, tx, `
MATCH (s:ProductStore {store:$store})
SET s.text_version=$text_version, s.text_count=$text_count, s.text_edge_count=$text_edge_count, s.text_bytes=$text_bytes`, params)
}

type previousProductTexts struct {
	ids       []string
	edgeCount int
	keyBytes  int
}

func txPreviousProductTexts(ctx context.Context, tx neo4j.ManagedTransaction, id string) (previousProductTexts, error) {
	rows, err := txRows(ctx, tx, `
MATCH (:Product {id:$id})-[e:HAS_TEXT]->(n:ProductText)
RETURN n.id AS id, e.key AS key`, map[string]any{"id": id})
	if err != nil {
		return previousProductTexts{}, err
	}
	previous := previousProductTexts{ids: make([]string, 0, len(rows)), edgeCount: len(rows)}
	for _, row := range rows {
		previous.ids = append(previous.ids, getString(row, "id"))
		previous.keyBytes += len(getString(row, "key"))
	}
	return previous, nil
}

// Deleting the old edges and writing the new ones can retain a node already
// counted by the store. Add only new nodes and subtract only dropped orphans.
func txUpdatePushedProductTexts(ctx context.Context, tx neo4j.ManagedTransaction, store string, counts productTextCounts,
	previous previousProductTexts, projection productTextProjection) (productTextCounts, error) {
	added := projection.counts()
	counts.count += added.count
	counts.edges += added.edges - previous.edgeCount
	counts.bytes += added.bytes - previous.keyBytes

	if added.count != 0 {
		ids := make([]string, 0, added.count)
		for _, id := range projection.idsByText {
			ids = append(ids, id)
		}
		existing, err := txRows(ctx, tx, `
UNWIND $ids AS id
MATCH (n:ProductText {id:id})
RETURN n.text AS text`, map[string]any{"ids": ids})
		if err != nil {
			return productTextCounts{}, err
		}
		counts.count -= len(existing)
		for _, row := range existing {
			counts.bytes -= len(getString(row, "text"))
		}
		if err := txWriteProductTexts(ctx, tx, store, projection); err != nil {
			return productTextCounts{}, err
		}
	}

	if previous.edgeCount != 0 {
		removed, err := txRows(ctx, tx, deleteOrphanedPushedTextsQuery, map[string]any{"ids": previous.ids})
		if err != nil {
			return productTextCounts{}, err
		}
		counts.count -= len(removed)
		for _, row := range removed {
			counts.bytes -= len(getString(row, "text"))
		}
	}
	return counts, nil
}
