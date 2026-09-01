package snapshot

import (
	"bytes"
	"regexp"

	"github.com/cockroachdb/errors"
)

// SearchMode selects how the query is interpreted.
const (
	ModeLiteral = "literal"
	ModeRegex   = "regex"
)

// Matcher reports every byte offset range where the query matches a line.
// Offsets always refer to the original line bytes, even for case-insensitive
// matching.
type Matcher interface {
	FindAllIndex(line []byte) [][]int
}

// Compile validates and compiles a query into a Matcher. Case-sensitive
// literal queries use plain byte searches; every other combination uses the
// RE2 engine so that case folding and offsets stay stable.
func Compile(mode, query string, caseSensitive bool) (Matcher, error) {
	if query == "" {
		return nil, errors.New("the search query is required")
	}
	switch mode {
	case ModeLiteral:
		if caseSensitive {
			return literalMatcher(query), nil
		}
		return compileRegex("(?i)" + regexp.QuoteMeta(query))
	case ModeRegex:
		if caseSensitive {
			return compileRegex(query)
		}
		return compileRegex("(?i)" + query)
	default:
		return nil, errors.Newf("unsupported search mode %q", mode)
	}
}

// regexpMatcher adapts *regexp.Regexp, whose FindAllIndex takes a match cap,
// to the single-argument Matcher interface.
type regexpMatcher struct {
	expression *regexp.Regexp
}

func compileRegex(expression string) (Matcher, error) {
	compiled, err := regexp.Compile(expression)
	if err != nil {
		return nil, errors.Wrap(err, "compile search pattern")
	}
	return regexpMatcher{expression: compiled}, nil
}

func (m regexpMatcher) FindAllIndex(line []byte) [][]int {
	return m.expression.FindAllIndex(line, -1)
}

type literalMatcher string

func (pattern literalMatcher) FindAllIndex(line []byte) [][]int {
	needle := []byte(pattern)
	var ranges [][]int
	for offset := 0; offset <= len(line)-len(needle); {
		index := bytes.Index(line[offset:], needle)
		if index < 0 {
			break
		}
		start := offset + index
		ranges = append(ranges, []int{start, start + len(needle)})
		offset = start + len(needle)
	}
	return ranges
}
