package securelog

import (
	"io"
	"log/slog"
	"strings"
	"sync"
)

type redactingWriter struct {
	mu      sync.Mutex
	writer  io.Writer
	secrets []string
}

func (w *redactingWriter) Write(data []byte) (int, error) {
	redacted := string(data)
	for _, secret := range w.secrets {
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := io.WriteString(w.writer, redacted); err != nil {
		return 0, err
	}
	return len(data), nil
}

func New(writer io.Writer, level string, secrets ...string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(
		&redactingWriter{writer: writer, secrets: secrets},
		&slog.HandlerOptions{Level: parseLevel(level)},
	))
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
