package securelog

import (
	"io"
	"log/slog"
	"strings"
	"sync"
)

// Writer serializes log lines from many emitters so that JSON records never
// interleave. It is safe for concurrent use and is shared by every user of
// the process.
type Writer struct {
	mu     sync.Mutex
	writer io.Writer
}

func NewWriter(writer io.Writer) *Writer {
	return &Writer{writer: writer}
}

func (w *Writer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

// redactor removes a fixed set of secrets from every line before handing it
// to the shared destination. Each user owns one redactor with only their own
// secrets, so the cost of a log line does not grow with the number of users.
type redactor struct {
	destination *Writer
	secrets     []string
}

func NewRedactor(destination *Writer, secrets ...string) io.Writer {
	return &redactor{destination: destination, secrets: secrets}
}

func (r *redactor) Write(data []byte) (int, error) {
	redacted := string(data)
	for _, secret := range r.secrets {
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
		}
	}
	return r.destination.Write([]byte(redacted))
}

// NewLogger returns a logger without secret redaction. It must never receive
// credential material.
func NewLogger(destination *Writer, level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(destination, &slog.HandlerOptions{Level: parseLevel(level)}))
}

func NewRedactingLogger(destination *Writer, level string, secrets ...string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(
		NewRedactor(destination, secrets...),
		&slog.HandlerOptions{Level: parseLevel(level)},
	))
}

// New builds a redacting logger over any writer. It stays for the stdio
// deployment, which has exactly one user.
func New(writer io.Writer, level string, secrets ...string) *slog.Logger {
	return NewRedactingLogger(NewWriter(writer), level, secrets...)
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
