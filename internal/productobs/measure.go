package productobs

import (
	"errors"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TypedValue carries the parsed decimal magnitudes and their canonical
// equivalents. Fields are ordered by their JSON keys for stream encoding.
type TypedValue struct {
	CanonicalUnit *string `json:"canonical_unit"`
	Hi            *string `json:"hi"`
	HiCanonical   *string `json:"hi_canonical"`
	Inclusive     *bool   `json:"inclusive"`
	Lo            *string `json:"lo"`
	LoCanonical   *string `json:"lo_canonical"`
	Qualifier     *string `json:"qualifier"`
	Status        string  `json:"status"`
	UnitIn        *string `json:"unit_in"`
}

type measure struct {
	canonical string
	aliases   map[string]string
	factors   map[string]decimal
	exact     *regexp.Regexp
	dashRange *regexp.Regexp
	wordRange *regexp.Regexp
}

const number = `(\p{Nd}+(?:[.,]\p{Nd}+)?)`

var (
	factorPattern     = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)$`)
	disallowedMeasure = regexp.MustCompile(`(^|[^\pL\pN_])in +\p{Nd}+ */ *\p{Nd}+|\p{Nd}[.,]\p{Nd}{3}($|[^\p{Nd}])`)
	bounds            = []struct {
		operator, qualifier string
		inclusive           bool
	}{
		{"<=", "upper", true}, {"≤", "upper", true}, {"<", "upper", false},
		{"max.", "upper", true}, {"max", "upper", true}, {"fino a", "upper", true},
		{"sotto", "upper", false}, {"below", "upper", false}, {"under", "upper", false},
		{"up to", "upper", true},
		{">=", "lower", true}, {"≥", "lower", true}, {">", "lower", false},
		{"min.", "lower", true}, {"min", "lower", true}, {"oltre", "lower", false},
		{"above", "lower", false}, {"over", "lower", false}, {"at least", "lower", true},
	}
)

func newMeasure(canonical string, aliases map[string]string) (*measure, error) {
	if !nonempty(canonical) {
		return nil, errors.New("expected a canonical unit")
	}
	if len(aliases) == 0 {
		return nil, errors.New("expected unit aliases")
	}
	m := &measure{canonical: canonical, aliases: aliases, factors: make(map[string]decimal, len(aliases))}
	scale := temperatureScale(canonical)
	units := make([]string, 0, len(aliases))
	for rawUnit, factor := range aliases {
		unit := measureFold(rawUnit)
		if _, exists := m.factors[unit]; exists || unit == "" {
			return nil, errors.New("empty or duplicate folded alias")
		}
		if !factorPattern.MatchString(factor) {
			return nil, errors.New("expected a positive decimal string factor")
		}
		d := parseDecimal(factor)
		if d.coefficient.Sign() <= 0 {
			return nil, errors.New("expected a positive decimal string factor")
		}
		if temperatureScale(unit) != scale {
			return nil, errors.New("aliases cross temperature scales")
		}
		m.factors[unit] = d
		units = append(units, unit)
	}
	slices.SortFunc(units, func(a, b string) int {
		if length := utf8.RuneCountInString(b) - utf8.RuneCountInString(a); length != 0 {
			return length
		}
		return strings.Compare(a, b)
	})
	for i, unit := range units {
		units[i] = regexp.QuoteMeta(unit)
	}
	unit := "(" + strings.Join(units, "|") + ")"
	end := number + " *" + unit + "$"
	// Compile can fail for an impractically large alternation. Keep registry
	// validation errors on the error path even for user-supplied regex sizes.
	var err error
	m.exact, err = regexp.Compile("^" + end)
	if err != nil {
		return nil, errors.New("unit aliases exceed measure grammar limits")
	}
	m.dashRange, err = regexp.Compile("^" + number + " *" + unit + "? *[-–] *" + end)
	if err != nil {
		return nil, errors.New("unit aliases exceed measure grammar limits")
	}
	m.wordRange, err = regexp.Compile("^da " + number + " a " + end)
	if err != nil {
		return nil, errors.New("unit aliases exceed measure grammar limits")
	}
	return m, nil
}

func measureFold(raw string) string {
	return caseFold.String(Fold("measure", raw))
}

func temperatureScale(unit string) string {
	token := strings.ReplaceAll(strings.ReplaceAll(measureFold(unit), " ", ""), ".", "")
	if rest, found := strings.CutPrefix(token, "deg"); found {
		token = rest
	} else {
		token = strings.TrimPrefix(token, "°")
	}
	switch token {
	case "c", "celsius", "centigrade":
		return "c"
	case "f", "fahrenheit":
		return "f"
	case "k", "kelvin":
		return "k"
	default:
		return ""
	}
}

func (m *measure) parse(raw string) *TypedValue {
	unparsed := &TypedValue{Status: "unparsed"}
	value := measureFold(raw)
	if disallowedMeasure.MatchString(value) {
		return unparsed
	}
	if fields := m.exact.FindStringSubmatch(value); fields != nil {
		return m.typed("exact", fields[2], fields[1], fields[1], true)
	}
	if fields := m.dashRange.FindStringSubmatch(value); fields != nil {
		if fields[2] != "" && fields[2] != fields[4] {
			return unparsed
		}
		return m.typed("range", fields[4], fields[1], fields[3], true)
	}
	if fields := m.wordRange.FindStringSubmatch(value); fields != nil {
		return m.typed("range", fields[3], fields[1], fields[2], true)
	}
	for _, bound := range bounds {
		if rest, found := strings.CutPrefix(value, bound.operator); found {
			fields := m.exact.FindStringSubmatch(strings.TrimLeft(rest, " "))
			if fields == nil {
				continue
			}
			lo, hi := "", fields[1]
			if bound.qualifier == "lower" {
				lo, hi = hi, lo
			}
			return m.typed(bound.qualifier, fields[2], lo, hi, bound.inclusive)
		}
	}
	return unparsed
}

func (m *measure) typed(qualifier, unit, lo, hi string, inclusive bool) *TypedValue {
	factor := m.factors[unit]
	out := &TypedValue{
		Status: "ok", Qualifier: &qualifier, UnitIn: &unit,
		Inclusive: &inclusive, CanonicalUnit: new(m.canonical),
	}
	out.Lo, out.LoCanonical = magnitudes(lo, factor)
	out.Hi, out.HiCanonical = magnitudes(hi, factor)
	return out
}

func magnitudes(number string, factor decimal) (*string, *string) {
	if number == "" {
		return nil, nil
	}
	quantity := parseDecimal(number)
	canonical := decimal{scale: quantity.scale + factor.scale}
	canonical.coefficient.Mul(&quantity.coefficient, &factor.coefficient)
	return new(quantity.String()), new(canonical.String())
}

// A decimal is an integer coefficient times 10 to the negative scale.
type decimal struct {
	coefficient big.Int
	scale       int
}

func parseDecimal(s string) decimal {
	ascii := strings.Map(func(r rune) rune {
		switch {
		case r == ',':
			return '.'
		case r <= unicode.MaxASCII:
			return r
		default:
			return asciiDigit(r)
		}
	}, s)
	whole, fraction, _ := strings.Cut(ascii, ".")
	var out decimal
	out.coefficient.SetString(whole+fraction, 10)
	out.scale = len(fraction)
	return out
}

func asciiDigit(r rune) rune {
	// Consecutive Nd blocks each contain ten digits and start at zero.
	offset := rune(0)
	for unicode.IsDigit(r - 1) {
		offset++
		r--
	}
	return '0' + offset%10
}

func (d decimal) String() string {
	digits := d.coefficient.String()
	if d.scale == 0 {
		return digits
	}
	if d.scale >= len(digits) {
		digits = strings.Repeat("0", d.scale-len(digits)+1) + digits
	}
	point := len(digits) - d.scale
	fraction := strings.TrimRight(digits[point:], "0")
	if fraction == "" {
		return digits[:point]
	}
	return digits[:point] + "." + fraction
}
