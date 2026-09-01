package snapshot

import (
	"path"
	"strings"

	"github.com/cockroachdb/errors"
)

// ValidatePathPatterns validates include and exclude patterns up front so
// that invalid globs fail before any snapshot is downloaded.
func ValidatePathPatterns(include, exclude []string) error {
	for _, pattern := range include {
		if err := validatePathPattern(pattern); err != nil {
			return errors.Wrap(err, "invalid include pattern")
		}
	}
	for _, pattern := range exclude {
		if err := validatePathPattern(pattern); err != nil {
			return errors.Wrap(err, "invalid exclude pattern")
		}
	}
	return nil
}

// validatePathPattern accepts a restricted slash-separated glob. Absolute
// paths, parent components, backslashes, and NUL bytes are rejected so a
// pattern can only ever describe paths inside the snapshot.
func validatePathPattern(pattern string) error {
	if pattern == "" {
		return errors.New("the path pattern is empty")
	}
	if strings.ContainsRune(pattern, '\x00') {
		return errors.New("the path pattern contains a NUL byte")
	}
	if strings.ContainsRune(pattern, '\\') {
		return errors.New("the path pattern must use / separators")
	}
	if strings.HasPrefix(pattern, "/") {
		return errors.New("the path pattern must not be absolute")
	}
	cleaned := path.Clean(pattern)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("the path pattern must not reference parent directories")
	}
	if cleaned != pattern {
		return errors.New("the path pattern is not a clean relative path")
	}
	return nil
}

// matchPathPattern reports whether the slash-separated snapshot path matches
// the pattern. A pattern matches the full path or any trailing path suffix, so
// "*.go" matches "src/main.go" and "src/*" matches every path with a src/
// segment at any depth.
func matchPathPattern(pattern, snapshotPath string) bool {
	if matched, err := path.Match(pattern, snapshotPath); err == nil && matched {
		return true
	}
	segments := strings.Split(snapshotPath, "/")
	for index := range segments {
		if matched, err := path.Match(pattern, strings.Join(segments[index:], "/")); err == nil && matched {
			return true
		}
	}
	return false
}
