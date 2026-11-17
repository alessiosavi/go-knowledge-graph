// Package domain builds entity-name profiles and makes deterministic domain
// assignments without a store or extraction-provider dependency.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Profile maps Fold(name) to the number of distinct chunks mentioning it.
type Profile map[string]int

// DocProfile is a document's stored assignment and entity-name profile.
type DocProfile struct {
	DocID, Domain, Source string
	Profile               Profile
}

// Fold applies NFKD, discards non-ASCII characters, lowercases, and keeps
// only a-z and 0-9. In particular, this does not transliterate letters
// such as ß or ł that do not decompose into ASCII.
func Fold(name string) string {
	var out strings.Builder
	for _, r := range norm.NFKD.String(name) {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			out.WriteByte(byte(r))
		}
	}
	return out.String()
}

// BuildProfile counts each folded name at most once per chunk.
func BuildProfile(chunkNames map[string][]string) Profile {
	profile := make(Profile)
	for _, names := range chunkNames {
		seen := make(map[string]bool)
		for _, name := range names {
			key := Fold(name)
			if key != "" && !seen[key] {
				profile[key]++
				seen[key] = true
			}
		}
	}
	return profile
}

// NewDomainID derives the founding document's domain id: "d-" and the
// first 8 characters of a 32-character lowercase hexadecimal document id.
// Any other id (never written by gkg, but possibly stored by hand) is
// hashed first, so detection never panics and the result stays in the
// d-<8 hex> space.
func NewDomainID(docID string) string {
	if !isDocID(docID) {
		sum := sha256.Sum256([]byte(docID))
		docID = hex.EncodeToString(sum[:])
	}
	return "d-" + docID[:8]
}

func isDocID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// DomainToken returns one opaque, alphanumeric token for a domain slug.
func DomainToken(domain string) string {
	sum := sha256.Sum256([]byte(domain))
	return "dk" + hex.EncodeToString(sum[:8])
}

// DomainName joins the three highest TF-IDF-weighted original names.
// Equal weights are ordered by folded name. A missing original falls
// back to its folded name.
func DomainName(target DocProfile, originals map[string]string, corpus []DocProfile) string {
	corpus = withoutTarget(target, corpus)
	terms := weightedTerms(target.Profile, documentFrequencies(corpus), len(corpus))
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].weight != terms[j].weight {
			return terms[i].weight > terms[j].weight
		}
		return terms[i].name < terms[j].name
	})
	names := make([]string, 0, 3)
	for _, term := range terms[:min(3, len(terms))] {
		name := originals[term.name]
		if name == "" {
			name = term.name
		}
		names = append(names, name)
	}
	return strings.Join(names, " / ")
}

type weightedTerm struct {
	name   string
	weight float64
}

func documentFrequencies(corpus []DocProfile) map[string]int {
	df := make(map[string]int)
	for _, doc := range corpus {
		for name, tf := range doc.Profile {
			if name != "" && tf > 0 {
				df[name]++
			}
		}
	}
	return df
}

// weightedTerms returns terms in name order so sums are deterministic
// despite Go's randomized map iteration.
func weightedTerms(profile Profile, df map[string]int, n int) []weightedTerm {
	terms := make([]weightedTerm, 0, len(profile))
	for name, tf := range profile {
		if name != "" && tf > 0 {
			idf := math.Log(float64(n+1)/float64(df[name]+1)) + 1
			terms = append(terms, weightedTerm{name, (1 + math.Log(float64(tf))) * idf})
		}
	}
	sort.Slice(terms, func(i, j int) bool { return terms[i].name < terms[j].name })
	return terms
}

// withoutTarget also excludes documents that have no domain assignment.
func withoutTarget(target DocProfile, corpus []DocProfile) []DocProfile {
	out := make([]DocProfile, 0, len(corpus))
	for _, doc := range corpus {
		if doc.Domain != "" && (target.DocID == "" || doc.DocID != target.DocID) {
			out = append(out, doc)
		}
	}
	return out
}
