package mcpserver

type ToolResponse[T any] struct {
	Data  *T           `json:"data,omitempty"`
	Error *ToolError   `json:"error,omitempty"`
	Meta  ResponseMeta `json:"meta"`
}

type ResponseMeta struct {
	RequestID     string   `json:"request_id"`
	Truncated     bool     `json:"truncated"`
	NextPage      *int     `json:"next_page,omitempty"`
	NextStartLine *int     `json:"next_start_line,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type ToolError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

type AuthenticatedUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

type RepositoryPermissions struct {
	Pull  bool `json:"pull"`
	Push  bool `json:"push"`
	Admin bool `json:"admin"`
}

type Repository struct {
	ID            int64                 `json:"id"`
	Name          string                `json:"name"`
	FullName      string                `json:"full_name"`
	Owner         string                `json:"owner"`
	Description   string                `json:"description"`
	DefaultBranch string                `json:"default_branch"`
	Private       bool                  `json:"private"`
	CloneURL      string                `json:"clone_url"`
	WebURL        string                `json:"web_url"`
	Permissions   RepositoryPermissions `json:"permissions"`
}

type RepositoryPage struct {
	Repositories []Repository `json:"repositories"`
	Page         int          `json:"page"`
	PerPage      int          `json:"per_page"`
	Total        int          `json:"total"`
}

type DirectoryEntry struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	SHA          string `json:"sha"`
	Target       string `json:"target,omitempty"`
	SubmoduleURL string `json:"submodule_url,omitempty"`
}

type DirectoryPage struct {
	Entries []DirectoryEntry `json:"entries"`
	Path    string           `json:"path"`
	Ref     string           `json:"ref"`
	Page    int              `json:"page"`
	PerPage int              `json:"per_page"`
	Total   int              `json:"total"`
}

type File struct {
	Path         string `json:"path"`
	Ref          string `json:"ref"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	SHA          string `json:"sha"`
	Content      string `json:"content,omitempty"`
	StartLine    int    `json:"start_line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
	TotalLines   int    `json:"total_lines,omitempty"`
	Target       string `json:"target,omitempty"`
	SubmoduleURL string `json:"submodule_url,omitempty"`
}
