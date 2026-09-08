package gogs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

type ContentEntry struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	SHA          string `json:"sha"`
	Target       string `json:"target,omitempty"`
	SubmoduleURL string `json:"submodule_url,omitempty"`
}

type FileContent struct {
	Path         string
	Type         string
	Size         int64
	SHA          string
	Ref          string
	Target       string
	SubmoduleURL string
	Data         []byte
}

type contentResponse struct {
	Type            string `json:"type"`
	Target          string `json:"target"`
	SubmoduleGitURL string `json:"submodule_git_url"`
	Encoding        string `json:"encoding"`
	Size            int64  `json:"size"`
	Name            string `json:"name"`
	Path            string `json:"path"`
	Content         string `json:"content"`
	SHA             string `json:"sha"`
}

type contentMetadataResponse struct {
	Type            string `json:"type"`
	Target          string `json:"target"`
	SubmoduleGitURL string `json:"submodule_git_url"`
	Size            int64  `json:"size"`
	Name            string `json:"name"`
	Path            string `json:"path"`
	SHA             string `json:"sha"`
}

func (c *Client) ListDirectory(ctx context.Context, owner, repo, repositoryPath, ref string) ([]ContentEntry, string, error) {
	entries, usedRef, _, err := c.ListDirectoryPage(ctx, owner, repo, repositoryPath, ref, 0, 0)
	return entries, usedRef, err
}

func (c *Client) ListDirectoryPage(ctx context.Context, owner, repo, repositoryPath, ref string, page, perPage int) ([]ContentEntry, string, int, error) {
	if err := ValidateRepositoryPath(repositoryPath, true); err != nil {
		return nil, "", 0, err
	}
	usedRef, err := c.resolveContentRef(ctx, owner, repo, ref)
	if err != nil {
		return nil, "", 0, err
	}
	sha, err := c.ResolveCommitSHA(ctx, owner, repo, usedRef)
	if err != nil {
		return nil, "", 0, err
	}
	tree, err := c.directoryTree(ctx, owner, repo, repositoryPath, sha)
	if err != nil {
		return nil, "", 0, err
	}
	total := len(tree)
	if page > 0 && perPage > 0 {
		slices.SortFunc(tree, func(a, b treeEntry) int { return cmp.Compare(a.Path, b.Path) })
		start := total
		if page <= (total+perPage-1)/perPage {
			start = (page - 1) * perPage
		}
		tree = tree[start:min(start+perPage, total)]
	}
	entries := make([]ContentEntry, 0, len(tree))
	for _, entry := range tree {
		item := ContentEntry{Name: entry.Path, Path: path.Join(repositoryPath, entry.Path), SHA: entry.SHA, Size: entry.Size}
		switch entry.Type {
		case "tree":
			item.Type = "dir"
		case "blob", "commit":
			// Gogs 0.14.2 reports every blob mode as 120000. Read only the
			// bounded Contents prefix for its true type and optional target.
			metadata, err := c.contentPrefix(ctx, owner, repo, item.Path, sha)
			if err != nil {
				return nil, "", 0, err
			}
			item.Type = metadata.Type
			item.Target = metadata.Target
			item.SubmoduleURL = metadata.SubmoduleGitURL
		default:
			return nil, "", 0, &Error{Code: CodeGogsError, Message: "Gogs returned an unknown Git tree entry type."}
		}
		entries = append(entries, item)
	}
	return entries, usedRef, total, nil
}

func (c *Client) GetFile(ctx context.Context, owner, repo, repositoryPath, ref string) (FileContent, error) {
	if err := ValidateRepositoryPath(repositoryPath, false); err != nil {
		return FileContent{}, err
	}
	usedRef, err := c.resolveContentRef(ctx, owner, repo, ref)
	if err != nil {
		return FileContent{}, err
	}
	sha, err := c.ResolveCommitSHA(ctx, owner, repo, usedRef)
	if err != nil {
		return FileContent{}, err
	}
	parent := path.Dir(repositoryPath)
	if parent == "." {
		parent = ""
	}
	entries, err := c.directoryTree(ctx, owner, repo, parent, sha)
	if err != nil {
		return FileContent{}, err
	}
	var metadata *treeEntry
	for i := range entries {
		if entries[i].Path == path.Base(repositoryPath) {
			metadata = &entries[i]
			break
		}
	}
	if metadata == nil {
		return FileContent{}, &Error{Code: CodeResourceNotFoundOrForbidden, Message: "The file does not exist or is not accessible."}
	}
	if metadata.Type == "tree" {
		return FileContent{}, validationError("The repository path does not identify a file.")
	}
	if metadata.Size > 1<<20 {
		prefix, err := c.contentPrefix(ctx, owner, repo, repositoryPath, sha)
		if err != nil {
			return FileContent{}, err
		}
		return FileContent{Path: repositoryPath, Type: prefix.Type, Size: metadata.Size, SHA: metadata.SHA, Ref: usedRef, Target: prefix.Target, SubmoduleURL: prefix.SubmoduleGitURL}, nil
	}
	raw, err := c.getContent(ctx, owner, repo, repositoryPath, sha)
	if err != nil {
		return FileContent{}, err
	}
	if firstJSONByte(raw) != '{' {
		return FileContent{}, validationError("The repository path does not identify a file.")
	}

	var response contentResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return FileContent{}, invalidJSONError(err)
	}
	content := FileContent{
		Path:         response.Path,
		Type:         response.Type,
		Size:         response.Size,
		SHA:          response.SHA,
		Ref:          usedRef,
		Target:       response.Target,
		SubmoduleURL: response.SubmoduleGitURL,
	}
	if response.Type != "file" {
		return content, nil
	}
	if response.Encoding != "base64" {
		return FileContent{}, &Error{
			Code:    CodeGogsError,
			Message: "Gogs returned an unsupported file encoding.",
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(response.Content)
	if err != nil {
		return FileContent{}, &Error{
			Code:    CodeGogsError,
			Message: "Gogs returned invalid base64 file content.",
			cause:   err,
		}
	}
	content.Data = decoded
	return content, nil
}

func ValidateRepositoryPath(repositoryPath string, allowEmpty bool) error {
	if repositoryPath == "" {
		if allowEmpty {
			return nil
		}
		return validationError("The repository path is required.")
	}
	if strings.ContainsRune(repositoryPath, '\x00') ||
		strings.Contains(repositoryPath, "\\") ||
		strings.HasPrefix(repositoryPath, "/") {
		return validationError("The repository path is invalid.")
	}
	for _, segment := range strings.Split(repositoryPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return validationError("The repository path is invalid.")
		}
	}
	return nil
}

func (c *Client) resolveContentRef(ctx context.Context, owner, repo, ref string) (string, error) {
	if ref != "" {
		return ref, nil
	}
	repository, err := c.GetRepository(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	if repository.DefaultBranch == "" {
		return "", &Error{
			Code:    CodeGogsError,
			Message: "Gogs returned a repository without a default branch.",
		}
	}
	return repository.DefaultBranch, nil
}

func (c *Client) getContent(ctx context.Context, owner, repo, repositoryPath, ref string) (json.RawMessage, error) {
	segments := []string{"repos", owner, repo, "contents"}
	if repositoryPath != "" {
		segments = append(segments, strings.Split(repositoryPath, "/")...)
	}
	query := make(url.Values)
	query.Set("ref", ref)
	var response json.RawMessage
	if err := c.getJSONWithQuery(ctx, &response, query, segments...); err != nil {
		return nil, err
	}
	return response, nil
}

func firstJSONByte(value []byte) byte {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

func validationError(message string) *Error {
	return &Error{
		Code:    CodeValidationFailed,
		Message: message,
	}
}

func invalidJSONError(err error) *Error {
	return &Error{
		Code:    CodeGogsError,
		Message: "Gogs returned invalid JSON.",
		cause:   errors.Wrap(err, "decode Gogs content response"),
	}
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	SHA  string `json:"sha"`
}

func (c *Client) directoryTree(ctx context.Context, owner, repo, directory, sha string) ([]treeEntry, error) {
	parts := []string{}
	if directory != "" {
		parts = strings.Split(directory, "/")
	}
	for depth := 0; ; depth++ {
		var tree struct {
			Tree []treeEntry `json:"tree"`
		}
		if err := c.getJSON(ctx, &tree, "repos", owner, repo, "git", "trees", sha); err != nil {
			return nil, err
		}
		if depth == len(parts) {
			return tree.Tree, nil
		}
		found := false
		for _, entry := range tree.Tree {
			if entry.Path == parts[depth] {
				if entry.Type != "tree" {
					return nil, validationError("The repository path does not identify a directory.")
				}
				sha = entry.SHA
				found = true
				break
			}
		}
		if !found {
			return nil, &Error{Code: CodeResourceNotFoundOrForbidden, Message: "The directory does not exist or is not accessible."}
		}
	}
}

// contentPrefix stops before the payload field. Gogs puts type, target and
// submodule URL before content, so even huge blobs need only a small prefix.
func (c *Client) contentPrefix(ctx context.Context, owner, repo, repositoryPath, sha string) (contentMetadataResponse, error) {
	segments := append([]string{"repos", owner, repo, "contents"}, strings.Split(repositoryPath, "/")...)
	u := c.apiRoot.JoinPath(escapedPathSegments(segments)...)
	q := url.Values{"ref": {sha}}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return contentMetadataResponse{}, err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return contentMetadataResponse{}, classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		classified := classifyStatus(resp.StatusCode)
		c.logRequest(ctx, resp.StatusCode, time.Since(started), classified.Code)
		return contentMetadataResponse{}, classified
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return contentMetadataResponse{}, &Error{Code: CodeGogsError, Message: "Invalid content metadata."}
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return contentMetadataResponse{}, invalidJSONError(err)
		}
		name, ok := key.(string)
		if !ok {
			return contentMetadataResponse{}, validationError("Invalid metadata key.")
		}
		if name == "content" {
			break
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return contentMetadataResponse{}, invalidJSONError(err)
		}
		fields[name] = value
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return contentMetadataResponse{}, err
	}
	var result contentMetadataResponse
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, invalidJSONError(err)
	}
	if result.Type == "" {
		return result, &Error{Code: CodeGogsError, Message: "Gogs did not provide the content type before its payload."}
	}
	c.logRequest(ctx, resp.StatusCode, time.Since(started), "")
	return result, nil
}
