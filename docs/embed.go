// Package docs embeds the canonical OpenAPI specification in the binary.
package docs

import _ "embed"

// OpenAPI is the same specification maintained in docs/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
