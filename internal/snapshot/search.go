package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
)

const (
	// ContextLines is the number of context lines shown before and after each
	// matched line.
	ContextLines = 2
	// MaxMatches caps how many matches one search reports. The tool layer
	// additionally bounds the encoded output size.
	MaxMatches = 50
	// maxLineRunes caps the text reported for a single line; longer lines are
	// reported truncated with an ellipsis.
	maxLineRunes = 500
	// maxLineBytes is the scan buffer ceiling. Files containing a longer line
	// are skipped instead of being loaded into memory.
	maxLineBytes = 1 << 20
)

// Match is one literal text hit inside a snapshot. Path always uses forward
// slashes and is relative to the snapshot root. Column is the 1-based byte
// offset of the match within its line.
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

// Search walks the snapshot content root and reports every case-sensitive
// literal occurrence of the query, sorted by path, line, and column.
// Collection stops once MaxMatches have been found. Only regular files are
// searched. Files with a line longer than maxLineBytes are skipped so that
// pathological content cannot exhaust memory.
func Search(ctx context.Context, root, query string) ([]Match, error) {
	if query == "" {
		return nil, errors.New("the search query is required")
	}

	var matches []Match
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Unreadable snapshot content must not fail the whole search;
			// skipping the entry keeps scanning the rest of the snapshot.
			return fs.SkipDir
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		found, err := searchFile(root, path, query)
		if err != nil {
			// The same tolerance applies to files that cannot be scanned.
			return fs.SkipDir
		}
		matches = append(matches, found...)
		if len(matches) >= MaxMatches {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(matches) > MaxMatches {
		matches = matches[:MaxMatches]
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
	return matches, nil
}

// searchFile scans one file for the query. The whole file is read into memory
// so that context lines can be reported around a match; only single lines are
// bounded here, and a per-file byte limit is the scope of the bounded-search
// delivery.
func searchFile(root, path, query string) ([]Match, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		if !errors.Is(err, bufio.ErrTooLong) {
			return nil, err
		}
		// The lines read before the overlong one are still complete; the rest
		// of the file is ignored instead of being loaded into memory.
	}

	relativePath, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	relativePath = filepath.ToSlash(relativePath)

	queryBytes := []byte(query)
	var matches []Match
	for index, line := range lines {
		lineBytes := []byte(line)
		column := 0
		for {
			offset := bytes.Index(lineBytes[column:], queryBytes)
			if offset < 0 {
				break
			}
			column += offset
			matches = append(matches, Match{
				Path:     relativePath,
				Line:     index + 1,
				Column:   column + 1,
				LineText: truncateLine(line),
				Context:  contextLines(lines, index),
			})
			column += len(query)
			if len(matches) >= MaxMatches {
				return matches, nil
			}
		}
	}
	return matches, nil
}

func contextLines(lines []string, index int) []ContextLine {
	first := index - ContextLines
	if first < 0 {
		first = 0
	}
	last := index + ContextLines
	if last > len(lines)-1 {
		last = len(lines) - 1
	}
	context := make([]ContextLine, 0, last-first)
	for number := first; number <= last; number++ {
		if number == index {
			continue
		}
		context = append(context, ContextLine{Number: number + 1, Text: truncateLine(lines[number])})
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
