package graph

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const titlePrefixBytes = 8 * 1024

// DocumentTitleSeed is the metadata and reconstructed prefix for one document.
type DocumentTitleSeed struct {
	ID, Source, Text string
	HasTitle         bool
}

// PageDocumentTitleSeeds scans documents in ID order. Only untitled documents
// need their leading chunks loaded, and offsets remove chunk overlap.
func (s *Store) PageDocumentTitleSeeds(ctx context.Context, after, domain string, limit int) ([]DocumentTitleSeed, error) {
	rows, err := s.runRead(ctx, `
MATCH (d:Document)
WHERE d.id > $after AND ($domain = '' OR d.domain = $domain)
WITH d ORDER BY d.id LIMIT $limit
OPTIONAL MATCH (d)-[:HAS_CHUNK]->(c:Chunk)
WHERE coalesce(d.title, '') = '' AND c.start < $max_bytes
RETURN d.id AS id, d.source AS source, coalesce(d.title, '') <> '' AS titled,
       c.start AS start, substring(coalesce(c.text, ''), 0, $read_bytes) AS text
ORDER BY id, c.seq`, map[string]any{
		"after": after, "domain": domain, "limit": limit,
		"max_bytes": titlePrefixBytes, "read_bytes": titlePrefixBytes + utf8.UTFMax - 1,
	})
	if err != nil {
		return nil, fmt.Errorf("graph: scan document titles: %w", err)
	}
	var page []DocumentTitleSeed
	var prefix strings.Builder
	pos := 0
	for _, row := range rows {
		id := getString(row, "id")
		if len(page) == 0 || page[len(page)-1].ID != id {
			if len(page) != 0 {
				page[len(page)-1].Text = prefix.String()
			}
			page = append(page, DocumentTitleSeed{ID: id, Source: getString(row, "source"), HasTitle: getBool(row, "titled")})
			prefix.Reset()
			pos = 0
		}
		start, text := getInt(row, "start"), getString(row, "text")
		if text == "" || start < 0 || pos >= titlePrefixBytes {
			continue
		}
		if start > pos {
			// The chunker trims whitespace at boundaries. Preserve a line
			// boundary and leave a blank line when the gap spans two bytes.
			prefix.WriteString(strings.Repeat("\n", min(start-pos, 2)))
		}
		skip, end := max(0, pos-start), min(len(text), titlePrefixBytes-start)
		for end > skip && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		if skip < end {
			prefix.WriteString(text[skip:end])
			pos = start + end
		}
	}
	if len(page) != 0 {
		page[len(page)-1].Text = prefix.String()
	}
	return page, nil
}

// DocumentTitleUpdate is one proposed title and its derivation source.
type DocumentTitleUpdate struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	TitleSource string `json:"title_source"`
}

// SetMissingDocumentTitles applies one batch. Rechecking the title and domain
// at write time protects titles and scope changes made after the scan.
func (s *Store) SetMissingDocumentTitles(ctx context.Context, domain string, updates []DocumentTitleUpdate) (map[string]int, error) {
	rows := make([]map[string]any, 0, len(updates))
	for _, u := range updates {
		rows = append(rows, map[string]any{"id": u.ID, "title": u.Title, "origin": u.TitleSource})
	}
	var counts map[string]int
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		res, err := tx.Run(ctx, `
UNWIND $rows AS row
MATCH (d:Document {id: row.id})
WHERE coalesce(d.title, '') = '' AND ($domain = '' OR d.domain = $domain)
SET d.title = row.title, d.title_source = row.origin
RETURN row.origin AS origin, count(*) AS applied`, map[string]any{"rows": rows, "domain": domain})
		if err != nil {
			return err
		}
		records, err := res.Collect(ctx)
		if err != nil {
			return err
		}
		counts = make(map[string]int, len(records))
		for _, r := range records {
			counts[getString(r, "origin")] = getInt(r, "applied")
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("graph: set missing document titles: %w", err)
	}
	return counts, nil
}
