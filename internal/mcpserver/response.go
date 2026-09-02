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
	CacheHit      bool     `json:"cache_hit,omitempty"`
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

type Branch struct {
	Name    string `json:"name"`
	HeadSHA string `json:"head_sha"`
}

type BranchPage struct {
	Branches []Branch `json:"branches"`
	Page     int      `json:"page"`
	PerPage  int      `json:"per_page"`
	Total    int      `json:"total"`
}

type CommitSummary struct {
	SHA        string `json:"sha"`
	Message    string `json:"message"`
	AuthorName string `json:"author_name"`
	AuthorDate string `json:"author_date"`
}

type CommitPage struct {
	Commits []CommitSummary `json:"commits"`
	Limit   int             `json:"limit"`
}

type CommitPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type Commit struct {
	SHA        string       `json:"sha"`
	Message    string       `json:"message"`
	WebURL     string       `json:"web_url"`
	Author     CommitPerson `json:"author"`
	Committer  CommitPerson `json:"committer"`
	ParentSHAs []string     `json:"parent_shas,omitempty"`
}

type SearchContextLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

type SearchMatch struct {
	Path     string              `json:"path"`
	Line     int                 `json:"line"`
	Column   int                 `json:"column"`
	LineText string              `json:"line_text"`
	Context  []SearchContextLine `json:"context"`
}

type SearchPage struct {
	Query     string        `json:"query"`
	Ref       string        `json:"ref"`
	CommitSHA string        `json:"commit_sha"`
	Mode      string        `json:"mode"`
	Matches   []SearchMatch `json:"matches"`
}

type IssueUser struct {
	Username string `json:"username"`
	FullName string `json:"full_name"`
}

type IssueLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type IssueMilestone struct {
	Title string `json:"title"`
	State string `json:"state"`
}

// IssueSummary is the compact issue listing entry; bodies are only returned
// by get_issue to keep listing output bounded.
type IssueSummary struct {
	Number      int64        `json:"number"`
	Title       string       `json:"title"`
	State       string       `json:"state"`
	User        IssueUser    `json:"user"`
	Labels      []IssueLabel `json:"labels"`
	NumComments int          `json:"num_comments"`
	CreatedAt   string       `json:"created_at"`
	UpdatedAt   string       `json:"updated_at"`
}

type IssuePage struct {
	Issues []IssueSummary `json:"issues"`
	State  string         `json:"state"`
	Page   int            `json:"page"`
}

type Issue struct {
	Number      int64           `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	User        IssueUser       `json:"user"`
	Assignee    *IssueUser      `json:"assignee,omitempty"`
	Labels      []IssueLabel    `json:"labels"`
	Milestone   *IssueMilestone `json:"milestone,omitempty"`
	NumComments int             `json:"num_comments"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	WebURL      string          `json:"web_url"`
}

type IssueComment struct {
	ID        int64     `json:"id"`
	User      IssueUser `json:"user"`
	Body      string    `json:"body"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
}

type IssueCommentPage struct {
	Comments []IssueComment `json:"comments"`
	Since    string         `json:"since,omitempty"`
	Max      int            `json:"max_comments"`
}

// PullRequestSummary is the compact pull request listing entry; bodies are
// only returned by get_pull_request to keep listing output bounded.
type PullRequestSummary struct {
	Number      int64     `json:"number"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	User        IssueUser `json:"user"`
	NumComments int       `json:"num_comments"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
	HeadSHA     string    `json:"head_sha"`
}

type PullRequestPage struct {
	PullRequests []PullRequestSummary `json:"pull_requests"`
	State        string               `json:"state"`
	Limit        int                  `json:"limit"`
	Total        int                  `json:"total"`
}

type PullRequest struct {
	Number         int64        `json:"number"`
	Title          string       `json:"title"`
	Body           string       `json:"body"`
	State          string       `json:"state"`
	User           IssueUser    `json:"user"`
	Labels         []IssueLabel `json:"labels"`
	NumComments    int          `json:"num_comments"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
	WebURL         string       `json:"web_url"`
	HeadSHA        string       `json:"head_sha"`
	BaseRef        string       `json:"base_ref"`
	BaseRefAssumed bool         `json:"base_ref_assumed"`
}

type DiffFileStat struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	IsBinary  bool   `json:"is_binary"`
}

type PullCommit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

type PullRequestDiffPage struct {
	Number             int64          `json:"number"`
	BaseRef            string         `json:"base_ref"`
	BaseRefAssumed     bool           `json:"base_ref_assumed"`
	MergeBase          string         `json:"merge_base"`
	Diff               string         `json:"diff"`
	Files              []DiffFileStat `json:"files"`
	Commits            []PullCommit   `json:"commits"`
	Truncated          bool           `json:"truncated"`
	MergeState         string         `json:"merge_state"`
	MergeConflictPaths []string       `json:"merge_conflict_paths,omitempty"`
	// BaseCommits counts the commits the assumed or passed base branch
	// carries since the merge base; a large count suggests the pull request
	// targets a different branch.
	BaseCommits int `json:"base_commits"`
}
