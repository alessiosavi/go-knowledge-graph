// Package convert provides the optional high-quality document converter: an
// HTTP client for a docling-serve sidecar. Conversion is off by default;
// when enabled, the ingest loader routes rich binary formats (PDF, Office
// documents, images) through it and the returned markdown enters the
// existing chunk/extract/embed pipeline unchanged.
package convert

import (
	"context"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// Converter turns a raw document body into markdown. Implementations are
// safe for concurrent use.
type Converter interface {
	Convert(ctx context.Context, body []byte, filename, contentType string) (markdown string, err error)
}

// New builds the converter the configuration selects. The off mode (and an
// unknown mode) returns a pure nil interface — never a typed-nil.
func New(cfg config.Config) Converter {
	if cfg.Converter.Mode == "docling" {
		return newDocling(cfg.Converter)
	}
	return nil
}
