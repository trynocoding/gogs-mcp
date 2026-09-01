package gogs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"

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
	if err := ValidateRepositoryPath(repositoryPath, true); err != nil {
		return nil, "", err
	}
	usedRef, err := c.resolveContentRef(ctx, owner, repo, ref)
	if err != nil {
		return nil, "", err
	}
	raw, err := c.getContent(ctx, owner, repo, repositoryPath, usedRef)
	if err != nil {
		return nil, "", err
	}
	if firstJSONByte(raw) != '[' {
		return nil, "", validationError("The repository path does not identify a directory.")
	}

	var response []contentMetadataResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, "", invalidJSONError(err)
	}
	entries := make([]ContentEntry, len(response))
	for index, entry := range response {
		entries[index] = ContentEntry{
			Name:         entry.Name,
			Path:         entry.Path,
			Type:         entry.Type,
			Size:         entry.Size,
			SHA:          entry.SHA,
			Target:       entry.Target,
			SubmoduleURL: entry.SubmoduleGitURL,
		}
	}
	return entries, usedRef, nil
}

func (c *Client) GetFile(ctx context.Context, owner, repo, repositoryPath, ref string) (FileContent, error) {
	if err := ValidateRepositoryPath(repositoryPath, false); err != nil {
		return FileContent{}, err
	}
	usedRef, err := c.resolveContentRef(ctx, owner, repo, ref)
	if err != nil {
		return FileContent{}, err
	}
	raw, err := c.getContent(ctx, owner, repo, repositoryPath, usedRef)
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
