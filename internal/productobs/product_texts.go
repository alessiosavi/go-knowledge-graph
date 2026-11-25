package productobs

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	maxProductTextKeyBytes = 256
	maxProductTextBytes    = 2048
)

//go:embed measure_units.txt
var productMeasureUnits string

var bareProductMeasure = productMeasurePattern()

func productMeasurePattern() *regexp.Regexp {
	units := strings.Fields(productMeasureUnits)
	for i, unit := range units {
		// The Python reference's case-insensitive matching includes dotted and dotless I.
		units[i] = strings.ReplaceAll(regexp.QuoteMeta(unit), "i", "[iİı]")
	}
	return regexp.MustCompile(`(?i)^[<>≤≥~]? *` + number +
		`(?: *[-–x×/] *` + number + `)? *(?:` + strings.Join(units, "|") + `)?\.?$`)
}

// ProductText is the text of one attribute and its raw source key.
type ProductText struct {
	Key       string
	Text      string
	Truncated bool
}

// ProductTextStats counts attributes skipped for their value or key length,
// and how many emitted texts were truncated. A skipped key counts once.
type ProductTextStats struct {
	SkippedValues int
	SkippedKeys   int
	Truncated     int
}

// ProductTexts projects eligible attributes in raw-key order. Each attribute
// retains its own entry even when its text is identical to another attribute's.
func ProductTexts(record Record) ([]ProductText, ProductTextStats) {
	texts := make([]ProductText, 0, min(len(record.Attributes), MaxImportAttributes))
	var stats ProductTextStats
	for _, key := range slices.Sorted(maps.Keys(record.Attributes)) {
		if len(key) > maxProductTextKeyBytes {
			stats.SkippedKeys++
			continue
		}
		value := record.Attributes[key]
		// Collapse whitespace only for classification. Search text keeps the
		// original value, including its original whitespace.
		measure := strings.Join(strings.FieldsFunc(value, isSpace), " ")
		if measure == "" || bareProductMeasure.MatchString(measure) {
			stats.SkippedValues++
			continue
		}

		prefix := strings.ReplaceAll(key, "_", " ") + ": "
		limit := maxProductTextBytes - len(prefix)
		truncated := len(value) > limit
		if truncated {
			stats.Truncated++
			for limit > 0 && !utf8.RuneStart(value[limit]) {
				limit--
			}
			value = value[:limit]
		}
		texts = append(texts, ProductText{Key: key, Text: prefix + value, Truncated: truncated})
		if len(texts) == MaxImportAttributes {
			break
		}
	}
	return texts, stats
}

// ProductTextID returns the first 32 hex digits of the SHA-256 of the
// store and text separated by a zero byte.
func ProductTextID(store, text string) string {
	digest := sha256.Sum256([]byte(store + "\x00" + text))
	return hex.EncodeToString(digest[:16])
}

// ProductTextVecID returns the store-independent vec_id, the first 32 hex
// digits of the SHA-256 of the text.
func ProductTextVecID(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:16])
}
