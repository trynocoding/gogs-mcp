package httpserver

import (
	"net/http"
	"time"
)

// statusWriter records the status code for the access log and forwards
// flushes so that the SDK can stream event responses.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusWriter) WriteHeader(status int) {
	if !w.written {
		w.status = status
		w.written = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if !w.written {
		w.status = http.StatusOK
		w.written = true
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// withAccessLog records one line per request. The user fields appear once
// the request has an identity; the credential headers are never read here.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		recorder := &statusWriter{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(recorder, request)

		attributes := []any{
			"method", request.Method,
			"path", request.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds(),
			"remote_addr", request.RemoteAddr,
		}
		if entry, ok := identityFrom(request.Context()); ok {
			attributes = append(attributes, "user_id", entry.user.ID, "username", entry.user.Username)
		}
		s.baseLogger.Info("Handled an MCP request.", attributes...)
	})
}
