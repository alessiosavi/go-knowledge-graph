package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// denyWords are the write clauses and data-loading forms rejected anywhere
// in a query, outside string literals and quoted identifiers.
var denyWords = []string{"CREATE", "MERGE", "SET", "DELETE", "DETACH", "REMOVE", "DROP", "ALTER", "LOAD"}

// callPrefixes are the only procedure namespaces the read-only surface
// allows: the text and vector search procedures over the served indexes.
var callPrefixes = []string{"vector_search.", "text_search."}

type guardRule struct {
	word string
	re   *regexp.Regexp
}

var denyRules = func() []guardRule {
	rules := make([]guardRule, 0, len(denyWords))
	for _, w := range denyWords {
		rules = append(rules, guardRule{word: w, re: regexp.MustCompile(`(?i)\b` + w + `\b`)})
	}
	return rules
}()

var (
	callRe  = regexp.MustCompile(`(?i)\bCALL\b`)
	limitRe = regexp.MustCompile(`(?i)\bLIMIT\b`)
	unionRe = regexp.MustCompile(`(?i)\bUNION\b`)
)

// CheckReadOnly validates a caller-supplied Cypher query for the read-only
// agent surface and rewrites it with the row cap when it carries no LIMIT.
//
// Rules: exactly one statement; CREATE, MERGE, SET, DELETE, DETACH, REMOVE,
// DROP, ALTER and LOAD are rejected anywhere in code; CALL is only allowed
// for the vector_search and text_search procedure namespaces; UNION is
// rejected (a per-branch LIMIT cannot enforce a post-union row cap) and
// more than one clause-like LIMIT is rejected; keywords inside string
// literals, quoted identifiers and comments are ignored (comments count as
// whitespace, so they cannot glue tokens together). A query without a
// LIMIT gains "LIMIT $cypher_rows"; a LIMIT that is not a plain integer or
// $cypher_rows is rejected because it could exceed the cap. SHOW statements
// take no LIMIT and are passed through. The caller passes the row cap as
// the $cypher_rows parameter and rejects an explicit numeric LIMIT above
// the configured cap via LimitClause.
func CheckReadOnly(q string) (string, error) {
	if strings.TrimSpace(q) == "" {
		return "", fmt.Errorf("cypher: empty query")
	}
	code := stripNonCode(q)
	if strings.Contains(code, ";") {
		return "", fmt.Errorf("cypher: multiple statements are not allowed")
	}
	for _, rule := range denyRules {
		if rule.re.MatchString(code) {
			return "", fmt.Errorf("cypher: %s is not allowed in read-only queries", rule.word)
		}
	}
	if unionRe.MatchString(code) {
		// A Cypher UNION applies LIMIT per branch, so a post-union row
		// cap cannot be enforced on a single statement; ban it in v0.
		return "", fmt.Errorf("cypher: UNION is not supported on the read-only surface (a post-union row cap cannot be enforced)")
	}
	for _, loc := range callRe.FindAllStringIndex(code, -1) {
		name := procedureName(code[loc[1]:])
		allowed := false
		for _, prefix := range callPrefixes {
			if strings.HasPrefix(name, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			if name == "" {
				name = "(unnamed)"
			}
			return "", fmt.Errorf("cypher: CALL %s is not an allowed procedure", name)
		}
	}
	fields := strings.Fields(code)
	if len(fields) == 0 {
		return "", fmt.Errorf("cypher: empty statement")
	}
	if strings.EqualFold(fields[0], "SHOW") {
		return q, nil
	}
	args := clauseLimitArgs(code)
	switch {
	case len(args) == 0:
		return q + "\nLIMIT $cypher_rows", nil
	case len(args) > 1:
		// Which of several LIMIT clauses bounds the result cannot be
		// proven (UNION is already banned, so any second clause is
		// invalid Cypher); reject instead of guessing.
		return "", fmt.Errorf("cypher: multiple LIMIT clauses are not supported")
	case isDigits(args[0]) || args[0] == "$cypher_rows":
		return q, nil
	default:
		return "", fmt.Errorf("cypher: LIMIT must be a plain integer or $cypher_rows, got %q", args[0])
	}
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// stripNonCode blanks out string literals ('...' and "..."), backtick-
// quoted identifiers and comments (// line, /* block */) so keyword scans
// only see executable code. Backslash escapes inside strings are honored.
func stripNonCode(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			b.WriteByte(' ')
			i++
			for i < len(q) {
				if q[i] == '\\' && c != '`' && i+1 < len(q) {
					i += 2
					continue
				}
				if q[i] == c {
					i++
					break
				}
				i++
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '/':
			// Comments are whitespace for the engine, so they must be
			// whitespace for the scan too: deleting them would glue
			// tokens ("DELETE/*c*/n" must stay "DELETE n", not "DELETEn").
			b.WriteByte(' ')
			for i < len(q) && q[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			b.WriteByte(' ')
			i += 2
			for i+1 < len(q) && !(q[i] == '*' && q[i+1] == '/') {
				i++
			}
			if i+1 < len(q) {
				i += 2
			} else {
				i = len(q)
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// LimitClause returns the numeric LIMIT of a query as validated by
// CheckReadOnly: a plain integer is returned; the guard's injected
// "LIMIT $cypher_rows" is not numeric and reports false. Callers reject an
// explicit limit that exceeds their row cap — the injected cap only covers
// queries that carry no LIMIT of their own.
func LimitClause(q string) (int, bool) {
	arg, ok := limitArg(stripNonCode(q))
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(arg)
	if err != nil {
		return 0, false
	}
	return n, true
}

// limitArg locates the query's trailing LIMIT clause argument in code
// (string literals and comments removed); ok is false when no clause-like
// LIMIT exists. See clauseLimitArgs for what counts as clause-like.
func limitArg(code string) (string, bool) {
	args := clauseLimitArgs(code)
	if len(args) == 0 {
		return "", false
	}
	return args[len(args)-1], true
}

// clauseLimitArgs returns the arguments of every clause-like LIMIT token
// in code, in order: a LIMIT token not preceded by '.' or '$' (property
// and parameter names) whose continuation can start an expression.
// Anything else is an identifier named "limit" (variable, alias, pattern),
// which is not a clause.
func clauseLimitArgs(code string) []string {
	locs := limitRe.FindAllStringIndex(code, -1)
	var args []string
	for _, loc := range locs {
		if loc[0] > 0 {
			switch code[loc[0]-1] {
			case '.', '$':
				continue
			}
		}
		arg := strings.TrimSpace(code[loc[1]:])
		if arg == "" || !expressionStart(arg[0]) {
			continue
		}
		args = append(args, arg)
	}
	return args
}

// expressionStart reports whether c can begin a LIMIT expression; a
// continuation that cannot (a pattern colon, closing parenthesis, comma,
// comparison operator, ...) belongs to an identifier named limit instead.
func expressionStart(c byte) bool {
	switch {
	case c == '$' || c == '(' || c == '-' || c == '+':
		return true
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		return true
	}
	return false
}

// procedureName reads the dotted procedure name at the start of s (the text
// following a CALL keyword), skipping whitespace.
func procedureName(s string) string {
	s = strings.TrimLeft(s, " \t\r\n")
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			end++
			continue
		}
		break
	}
	return s[:end]
}
