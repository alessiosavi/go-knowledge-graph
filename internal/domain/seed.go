package domain

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Seed is a versioned set of source-prefix assignments and domain names.
type Seed struct {
	Schema int               `json:"schema"`
	Names  map[string]string `json:"names"`
	Rules  []SeedRule        `json:"rules"`
}

// SeedRule assigns sources beginning with Match to Domain.
type SeedRule struct {
	Match  string `json:"match"`
	Domain string `json:"domain"`
}

var domainSlug = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// ValidDomain reports whether s is a domain slug: 1 to 40 characters of
// lowercase ASCII letters, digits and hyphens. It is the one slug contract
// for seeds, stored domain properties and scoped search.
func ValidDomain(s string) bool { return domainSlug.MatchString(s) }

// LoadSeed loads schema 1, validates domain slugs, and rejects duplicate
// matches and empty matches (there is no default rule).
func LoadSeed(r io.Reader) (Seed, error) {
	var seed Seed
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&seed); err != nil {
		return Seed{}, fmt.Errorf("domain: decode seed: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Seed{}, fmt.Errorf("domain: seed contains multiple JSON values")
		}
		return Seed{}, fmt.Errorf("domain: decode seed trailer: %w", err)
	}
	if seed.Schema != 1 {
		return Seed{}, fmt.Errorf("domain: unsupported seed schema %d (want 1)", seed.Schema)
	}
	// Sorted, so the first reported invalid slug is the same on every run.
	for _, domain := range slices.Sorted(maps.Keys(seed.Names)) {
		if !ValidDomain(domain) {
			return Seed{}, fmt.Errorf("domain: invalid seed domain slug %q", domain)
		}
	}
	seen := make(map[string]bool)
	for _, rule := range seed.Rules {
		if !ValidDomain(rule.Domain) {
			return Seed{}, fmt.Errorf("domain: invalid seed domain slug %q", rule.Domain)
		}
		if rule.Match == "" {
			return Seed{}, fmt.Errorf("domain: seed rule match must not be empty")
		}
		if seen[rule.Match] {
			return Seed{}, fmt.Errorf("domain: duplicate seed rule match %q", rule.Match)
		}
		seen[rule.Match] = true
	}
	return seed, nil
}

// DomainFor returns the domain of the longest literal source-prefix
// match. Matching is case-sensitive and does not canonicalize the source.
func (s Seed) DomainFor(source string) (string, bool) {
	var domain string
	longest := 0
	for _, rule := range s.Rules {
		if len(rule.Match) > longest && strings.HasPrefix(source, rule.Match) {
			domain, longest = rule.Domain, len(rule.Match)
		}
	}
	return domain, longest > 0
}
