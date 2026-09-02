package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"gogs-mcp/internal/gogs"
)

const maxTokenHeaderBytes = 4096

type identityKey struct{}

func identityFrom(ctx context.Context) (*userEntry, bool) {
	entry, ok := ctx.Value(identityKey{}).(*userEntry)
	return entry, ok
}

// withAuthentication resolves the caller's Gogs token before the request
// reaches the MCP transport, so an authentication failure is always a 401
// and never a transport-level error.
func (s *Server) withAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token, ok := extractToken(request.Header.Get(s.options.TokenHeader))
		if !ok {
			s.writeUnauthorized(writer)
			return
		}
		hash := tokenHash(token)
		entry, ok := s.users.get(hash)
		if !ok {
			var err error
			entry, err = s.resolve(request.Context(), token, hash)
			if err != nil {
				classified := gogs.AsError(err)
				switch classified.Code {
				case gogs.CodeAuthenticationFailed, gogs.CodePermissionDenied:
					s.writeUnauthorized(writer)
				default:
					s.writeUnavailable(writer, classified)
				}
				return
			}
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), identityKey{}, entry)))
	})
}

// extractToken accepts "token <t>" and "Bearer <t>" schemes, both spelled in
// any case, and falls back to treating the whole value as the token itself.
// The value is bounded so that hostile headers cannot cost unbounded work.
func extractToken(value string) (string, bool) {
	if value == "" || len(value) > maxTokenHeaderBytes {
		return "", false
	}
	if scheme, rest, found := strings.Cut(value, " "); found {
		switch strings.ToLower(scheme) {
		case "bearer", "token":
			credential := strings.TrimSpace(rest)
			if credential == "" {
				return "", false
			}
			return credential, true
		}
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

type headerError struct {
	Error headerErrorBody `json:"error"`
}

type headerErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) writeUnauthorized(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("WWW-Authenticate", `Bearer realm="gogs-mcp"`)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(writer).Encode(headerError{
		Error: headerErrorBody{
			Code:    string(gogs.CodeAuthenticationFailed),
			Message: "A valid Gogs personal access token is required in the " + s.options.TokenHeader + " header.",
		},
	})
}

// writeUnavailable reports that the caller's credentials could not be
// verified right now. The token is neither accepted nor rejected, so the
// negative cache is untouched and the caller may retry.
func (s *Server) writeUnavailable(writer http.ResponseWriter, classified *gogs.Error) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Retry-After", "5")
	writer.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(writer).Encode(headerError{
		Error: headerErrorBody{
			Code:    string(classified.Code),
			Message: classified.Message,
		},
	})
}
