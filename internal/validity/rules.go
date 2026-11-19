package validity

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

// Input holds the stored entity names, edge type, document title and evidence
// sentence. Relation uses the stored edge type, such as has_feature.
type Input struct {
	Head     string `json:"head"`
	Relation string `json:"relation"`
	Tail     string `json:"tail"`
	Title    string `json:"title"`
	Evidence string `json:"evidence"`
}

// Action is the recommendation made by the validity rules.
type Action string

const (
	Keep     Action = "keep"
	Drop     Action = "drop"
	Reattach Action = "reattach"
)

// Decision names the action and the first rule that selected it.
type Decision struct {
	Action Action `json:"action"`
	Rule   string `json:"rule"`
}

// Rules is a compiled catalog-v3 lexicon.
type Rules struct {
	productTypes  map[string]string
	stopTokens    map[string]bool
	qualityNouns  map[string]bool
	classNouns    map[string]bool
	evaluative    map[string]bool
	sensory       map[string]bool
	functionWords map[string]bool
}

// NewRules validates and compiles a lexicon. It does not retain mutable inputs.
func NewRules(lexicon Lexicon) (*Rules, error) {
	if err := lexicon.Validate(); err != nil {
		return nil, err
	}
	return &Rules{
		productTypes:  maps.Clone(lexicon.ProductTypes),
		stopTokens:    tokenSet(lexicon.StopTokens),
		qualityNouns:  tokenSet(lexicon.QualityNouns),
		classNouns:    tokenSet(lexicon.ClassNouns),
		evaluative:    adjectives(lexicon.EvaluativeWords, lexicon.EvaluativeStems),
		sensory:       adjectives(lexicon.SensoryWords, lexicon.SensoryStems),
		functionWords: tokenSet(lexicon.FunctionWords),
	}, nil
}

func tokenSet(tokens []string) map[string]bool {
	set := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		set[token] = true
	}
	return set
}

func adjectives(words, stems []string) map[string]bool {
	set := tokenSet(words)
	for _, stem := range stems {
		for _, suffix := range []string{"", "a", "e", "i", "o", "issim", "issima", "issime", "issimi", "issimo"} {
			set[stem+suffix] = true
		}
	}
	return set
}

// Decide evaluates catalog-v3 rules in precedence order.
// The Go empty-title rule intentionally extends the reference's "not title"
// check to titles made entirely of characters in the Python whitespace set.
func (r *Rules) Decide(in Input) Decision {
	head := r.fold(in.Head)
	if r.bulletLine(head, in.Evidence) {
		return Decision{Drop, "bullet-line"}
	}
	if rule := r.tailRule(in.Relation, in.Tail); rule != "" {
		return Decision{Drop, rule}
	}
	if !lowerish(in.Head) {
		return Decision{Keep, "head-not-lowerish"}
	}
	if strings.TrimFunc(in.Title, pythonSpace) == "" {
		return Decision{Keep, "empty-title"}
	}

	title := r.fold(in.Title)
	titleSet := tokenSet(title)
	matched := 0
	for _, token := range head {
		if titleSet[token] {
			matched++
		}
	}
	if len(head) == 0 || 2*matched < len(head) {
		return Decision{Drop, "r1-containment"}
	}
	if in.Relation == "is_a" {
		return Decision{Keep, "is-a-keep"}
	}
	for _, token := range title {
		if r.stopTokens[token] {
			break
		}
		if slices.Contains(head, token) {
			return Decision{Reattach, "ra-title-product"}
		}
	}
	if in.Relation == "made_of" && matched == len(head) {
		return Decision{Reattach, "made-of-reattach"}
	}
	return Decision{Keep, "default-keep"}
}

func (r *Rules) tailRule(relation, tail string) string {
	switch relation {
	case "has_feature", "is_a", "made_of":
	default:
		return ""
	}
	if strings.ContainsFunc(tail, func(c rune) bool {
		return pythonUpper(c) || c == '®' || c == '™' || ('0' <= c && c <= '9')
	}) {
		return ""
	}
	var core []string
	for _, token := range tokens(tail) {
		if r.functionWords[token] ||
			(len(token) > 6 && strings.HasSuffix(token, "mente")) ||
			(len(token) > 4 && strings.HasSuffix(token, "ly")) {
			continue
		}
		core = append(core, token)
	}
	if len(core) == 0 {
		return ""
	}
	allAdj, anyQuality, allQualityOrAdj := true, false, true
	for _, token := range core {
		adj := r.evaluative[token] || r.sensory[token]
		quality := r.qualityNouns[token]
		allAdj = allAdj && adj
		anyQuality = anyQuality || quality
		allQualityOrAdj = allQualityOrAdj && (quality || adj)
	}
	if allAdj {
		return "tail-adj"
	}
	if anyQuality && allQualityOrAdj {
		return "tail-quality"
	}
	if relation != "has_feature" && len(core) >= 2 && r.classNouns[core[0]] {
		for _, token := range core[1:] {
			if !r.evaluative[token] {
				return ""
			}
		}
		return "tail-class"
	}
	return ""
}

// Python regex \s includes the ASCII information separators, and \d matches
// every Unicode decimal digit.
const pythonSpaceClass = `[\x09-\x0d\x1c-\x20\x{85}\p{Z}]`

var bullet = regexp.MustCompile(`^` + pythonSpaceClass + `*(?:[-*•]|\p{Nd}+[.)])` + pythonSpaceClass + `+(.*)$`)

func (r *Rules) bulletLine(head []string, evidence string) bool {
	if len(head) < 2 {
		return false
	}
	for _, line := range strings.FieldsFunc(evidence, pythonLineBreak) {
		match := bullet.FindStringSubmatch(line)
		if match != nil && slices.Equal(head, r.fold(match[1])) {
			return true
		}
	}
	return false
}

func pythonLineBreak(c rune) bool {
	switch c {
	case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
		return true
	default:
		return false
	}
}

func pythonSpace(c rune) bool {
	return unicode.IsSpace(c) || ('\x1c' <= c && c <= '\x1f')
}

func pythonUpper(c rune) bool {
	return unicode.IsUpper(c) || unicode.Is(unicode.Other_Uppercase, c)
}

func lowerish(head string) bool {
	firstUpper := false
	for i, c := range head {
		if pythonUpper(c) {
			if i != 0 {
				return false
			}
			firstUpper = true
		}
	}
	if !firstUpper {
		return true
	}
	for _, word := range strings.FieldsFunc(head, pythonSpace) {
		first, _ := utf8.DecodeRuneInString(word)
		if unicode.IsLetter(first) && !pythonUpper(first) {
			return true
		}
	}
	return false
}

func (r *Rules) fold(s string) []string {
	out := tokens(s)
	for i, token := range out {
		if product, ok := r.productTypes[token]; ok {
			out[i] = product
		}
	}
	return out
}

func tokens(s string) []string {
	lower := cases.Lower(language.Und).String(s)
	var ascii strings.Builder
	ascii.Grow(len(lower))
	for _, c := range lower {
		if c < utf8.RuneSelf {
			writeTokenRune(&ascii, c)
			continue
		}
		// Combining marks with nonzero canonical class are discarded. Their
		// reordering across runes therefore cannot affect the tokens. Working
		// rune by rune avoids NFKD inserting stream-safe joiners in long runs
		// of combining marks.
		decomposed := norm.NFKD.String(string(c))
		for i, part := range decomposed {
			if norm.NFD.PropertiesString(decomposed[i:]).CCC() == 0 {
				writeTokenRune(&ascii, part)
			}
		}
	}
	return strings.Fields(ascii.String())
}

func writeTokenRune(b *strings.Builder, c rune) {
	if ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') {
		b.WriteByte(byte(c))
	} else {
		b.WriteByte(' ')
	}
}
