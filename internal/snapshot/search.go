package snapshot

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cockroachdb/errors"
)

const (
	// DefaultContextLines is the number of context lines shown before and
	// after each matched line when no explicit value is requested.
	DefaultContextLines = 2
	// MaxContextLines bounds the context width a caller may request.
	MaxContextLines = 10
	// DefaultMaxResults is the number of matches reported when no explicit
	// value is requested.
	DefaultMaxResults = 50
	// MaxResultsLimit bounds the result count a caller may request.
	MaxResultsLimit = 500
	// DefaultMaxFileBytes is the largest text file searched by default.
	DefaultMaxFileBytes = 1 << 20
	// DefaultSearchTimeout is the per-search wall clock when none is requested.
	DefaultSearchTimeout = 30 * time.Second
	// MaxSearchTimeout bounds the per-search wall clock a caller may request.
	MaxSearchTimeout = 5 * time.Minute
	// binaryProbeBytes is how much of a file is inspected for NUL bytes when
	// deciding whether the content is text.
	binaryProbeBytes = 8192
	// maxLineRunes caps the text reported for a single line; longer lines are
	// reported truncated with an ellipsis.
	maxLineRunes = 500
)

// ErrInvalidOptions reports search options that fail validation, so that the
// tool layer can answer with INVALID_ARGUMENT instead of an internal error.
var ErrInvalidOptions = errors.New("the search options are invalid")

// Options configures one bounded search over a snapshot.
type Options struct {
	// Matcher is the compiled query; see Compile.
	Matcher Matcher
	// ContextLines is the number of context lines around each match, bounded
	// by MaxContextLines.
	ContextLines int
	// MaxResults caps the number of matches. Reaching the cap sets
	// Stats.Truncated.
	MaxResults int
	// MaxFileBytes skips files larger than this and counts them in
	// Stats.SkippedOversized.
	MaxFileBytes int64
	// Include restricts the search to paths matching at least one restricted
	// glob. Empty means every path.
	Include []string
	// Exclude skips paths matching any restricted glob.
	Exclude []string
}

// Stats reports what a search skipped or bounded, so that the tool layer can
// surface observable warnings instead of silently dropping content.
type Stats struct {
	SkippedBinary    int
	SkippedOversized int
	Truncated        bool
	TimedOut         bool
}

// Match is one text hit inside a snapshot. Path always uses forward slashes
// and is relative to the snapshot root. Column is the 1-based byte offset of
// the match within its line.
type Match struct {
	Path     string
	Line     int
	Column   int
	LineText string
	// Context holds the surrounding lines, up to ContextLines on each side of
	// the matched line. The matched line itself is not repeated here.
	Context []ContextLine
}

// ContextLine is one line of context with its 1-based line number.
type ContextLine struct {
	Number int
	Text   string
}

// Search walks the snapshot content root and reports every occurrence the
// matcher finds, sorted by path, line, and column. Only regular files inside
// the .git-excluded, glob-filtered path set are searched; binary, invalid
// UTF-8, and oversized files are skipped and counted in the stats. The search
// stops early once MaxResults matches have been collected or the context
// deadline passes, and the stats say which bound applied.
func Search(ctx context.Context, root string, options Options) ([]Match, Stats, error) {
	if options.Matcher == nil {
		return nil, Stats{}, errors.Wrap(ErrInvalidOptions, "the search matcher is required")
	}
	if options.ContextLines < 0 || options.ContextLines > MaxContextLines {
		return nil, Stats{}, errors.Wrapf(ErrInvalidOptions, "context lines must be between 0 and %d", MaxContextLines)
	}
	if options.MaxResults < 1 || options.MaxResults > MaxResultsLimit {
		return nil, Stats{}, errors.Wrapf(ErrInvalidOptions, "result limit must be between 1 and %d", MaxResultsLimit)
	}
	if options.MaxFileBytes < 1 {
		return nil, Stats{}, errors.Wrap(ErrInvalidOptions, "the file size limit must be positive")
	}
	for _, pattern := range append(slices.Clone(options.Include), options.Exclude...) {
		if err := validatePathPattern(pattern); err != nil {
			return nil, Stats{}, errors.Wrap(ErrInvalidOptions, err.Error())
		}
	}

	var (
		matches []Match
		stats   Stats
	)
	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Unreadable snapshot content must not fail the whole search;
			// skipping the entry keeps scanning the rest of the snapshot.
			return fs.SkipDir
		}
		if err := ctx.Err(); err != nil {
			stats.TimedOut = true
			return fs.SkipAll
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relativePath, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		relativePath = filepath.ToSlash(relativePath)
		if pathExcluded(relativePath, options.Include, options.Exclude) {
			return nil
		}

		content, skipped := readSearchableFile(filePath, options.MaxFileBytes)
		switch skipped {
		case skippedOversized:
			stats.SkippedOversized++
			return nil
		case skippedBinary:
			stats.SkippedBinary++
			return nil
		case skippedUnreadable:
			// A file that vanished or cannot be read, for example because a
			// concurrent eviction unlinked it, is skipped like an unreadable
			// directory instead of failing the whole search.
			return nil
		}

		found := searchContent(content, relativePath, options)
		if len(found) > 0 {
			matches = append(matches, found...)
			if len(matches) >= options.MaxResults {
				stats.Truncated = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return nil, Stats{}, err
	}

	if len(matches) > options.MaxResults {
		matches = matches[:options.MaxResults]
	}
	slices.SortFunc(matches, func(a, b Match) int {
		if order := strings.Compare(a.Path, b.Path); order != 0 {
			return order
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Column - b.Column
	})
	return matches, stats, nil
}

// pathExcluded reports whether the slash path fails the include set or hits
// the exclude set. The .git directory is always excluded.
func pathExcluded(path string, include, exclude []string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == ".git" {
			return true
		}
	}
	for _, pattern := range exclude {
		if matchPathPattern(pattern, path) {
			return true
		}
	}
	if len(include) == 0 {
		return false
	}
	for _, pattern := range include {
		if matchPathPattern(pattern, path) {
			return false
		}
	}
	return true
}

const (
	skippedNone = iota
	skippedOversized
	skippedBinary
	skippedUnreadable
)

// readSearchableFile loads a file for searching. Files beyond the byte limit,
// files that look binary (a NUL byte in the probe or invalid UTF-8), and files
// that cannot be read are skipped with a reason instead of being searched.
func readSearchableFile(path string, maxFileBytes int64) ([]byte, int) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, skippedUnreadable
	}
	if info.Size() > maxFileBytes {
		return nil, skippedOversized
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, skippedUnreadable
	}
	if int64(len(content)) > maxFileBytes {
		return nil, skippedOversized
	}
	probe := content
	if len(probe) > binaryProbeBytes {
		probe = probe[:binaryProbeBytes]
	}
	if bytes.IndexByte(probe, 0) >= 0 || !utf8.Valid(content) {
		return nil, skippedBinary
	}
	return content, skippedNone
}

// searchContent reports every match of the compiled query inside one text
// file, keeping the raw bytes so that match offsets stay exact.
func searchContent(content []byte, path string, options Options) []Match {
	if len(content) > 0 && content[len(content)-1] == '\n' {
		content = content[:len(content)-1]
	}
	lines := bytes.Split(content, []byte("\n"))

	var matches []Match
	for index, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		for _, located := range options.Matcher.FindAllIndex(line) {
			matches = append(matches, Match{
				Path:     path,
				Line:     index + 1,
				Column:   located[0] + 1,
				LineText: truncateLine(string(line)),
				Context:  contextLines(lines, index, options.ContextLines),
			})
			if len(matches) >= options.MaxResults {
				return matches
			}
		}
	}
	return matches
}

func contextLines(lines [][]byte, index, count int) []ContextLine {
	first := index - count
	if first < 0 {
		first = 0
	}
	last := index + count
	if last > len(lines)-1 {
		last = len(lines) - 1
	}
	context := make([]ContextLine, 0, last-first)
	for number := first; number <= last; number++ {
		if number == index {
			continue
		}
		context = append(context, ContextLine{Number: number + 1, Text: truncateLine(string(bytes.TrimSuffix(lines[number], []byte("\r"))))})
	}
	return context
}

func truncateLine(line string) string {
	runes := []rune(line)
	if len(runes) <= maxLineRunes {
		return line
	}
	return string(runes[:maxLineRunes]) + "…"
}
