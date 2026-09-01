package gogs

import (
	"context"
	"net/url"
)

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

type repositoryPermissionsResponse struct {
	Pull  bool `json:"pull"`
	Push  bool `json:"push"`
	Admin bool `json:"admin"`
}

type repositoryResponse struct {
	ID            int64                          `json:"id"`
	Name          string                         `json:"name"`
	FullName      string                         `json:"full_name"`
	Owner         userResponse                   `json:"owner"`
	Description   string                         `json:"description"`
	DefaultBranch string                         `json:"default_branch"`
	Private       bool                           `json:"private"`
	CloneURL      string                         `json:"clone_url"`
	HTMLURL       string                         `json:"html_url"`
	Permissions   *repositoryPermissionsResponse `json:"permissions"`
}

func (c *Client) ListRepositories(ctx context.Context) ([]Repository, error) {
	var response []repositoryResponse
	if err := c.getJSON(ctx, &response, "user", "repos"); err != nil {
		return nil, err
	}

	repositories := make([]Repository, len(response))
	for index := range response {
		repositories[index] = mapRepository(response[index])
	}
	return repositories, nil
}

func (c *Client) GetRepository(ctx context.Context, owner, name string) (Repository, error) {
	var response repositoryResponse
	if err := c.getJSON(ctx, &response, "repos", owner, name); err != nil {
		classified := AsError(err)
		if classified.Code == CodeResourceNotFoundOrForbidden {
			return Repository{}, &Error{
				Code:       classified.Code,
				Message:    "The repository does not exist or the current user cannot access it.",
				Retryable:  classified.Retryable,
				HTTPStatus: classified.HTTPStatus,
				cause:      err,
			}
		}
		return Repository{}, err
	}
	return mapRepository(response), nil
}

func mapRepository(response repositoryResponse) Repository {
	owner := response.Owner.Username
	if owner == "" {
		owner = response.Owner.Login
	}
	permissions := RepositoryPermissions{}
	if response.Permissions != nil {
		permissions = RepositoryPermissions{
			Pull:  response.Permissions.Pull,
			Push:  response.Permissions.Push,
			Admin: response.Permissions.Admin,
		}
	}
	return Repository{
		ID:            response.ID,
		Name:          response.Name,
		FullName:      response.FullName,
		Owner:         owner,
		Description:   response.Description,
		DefaultBranch: response.DefaultBranch,
		Private:       response.Private,
		CloneURL:      response.CloneURL,
		WebURL:        response.HTMLURL,
		Permissions:   permissions,
	}
}

func escapedPathSegments(segments []string) []string {
	escaped := make([]string, len(segments))
	for index, segment := range segments {
		escaped[index] = url.PathEscape(segment)
	}
	return escaped
}
