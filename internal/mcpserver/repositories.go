package mcpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"gogs-mcp/internal/gogs"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultPage    = 1
	defaultPerPage = 30
	maximumPerPage = 100
)

type listRepositoriesInput struct {
	Page    int `json:"page,omitempty"`
	PerPage int `json:"per_page,omitempty"`
}

type searchRepositoriesInput struct {
	Query   string `json:"query"`
	Page    int    `json:"page,omitempty"`
	PerPage int    `json:"per_page,omitempty"`
}

type getRepositoryInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

func registerRepositoryTools(server *mcp.Server, client Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_repositories",
		Description: "List all repositories accessible to the authenticated Gogs user.",
		Annotations: readOnlyAnnotations("List repositories"),
		InputSchema: paginationInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listRepositoriesInput) (*mcp.CallToolResult, ToolResponse[RepositoryPage], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "list_repositories")
		repositories, err := client.ListRepositories(ctx)
		if err != nil {
			return repositoryPageError(requestID, err)
		}
		page, nextPage := paginateRepositories(repositories, input.Page, input.PerPage)
		return nil, ToolResponse[RepositoryPage]{
			Data: &page,
			Meta: ResponseMeta{RequestID: requestID, NextPage: nextPage},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_repositories",
		Description: "Search all accessible Gogs repositories by name, full name, or description.",
		Annotations: readOnlyAnnotations("Search repositories"),
		InputSchema: searchRepositoriesInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchRepositoriesInput) (*mcp.CallToolResult, ToolResponse[RepositoryPage], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "search_repositories")
		repositories, err := client.ListRepositories(ctx)
		if err != nil {
			return repositoryPageError(requestID, err)
		}
		query := strings.ToLower(input.Query)
		matches := make([]gogs.Repository, 0, len(repositories))
		for _, repository := range repositories {
			if strings.Contains(strings.ToLower(repository.Name), query) ||
				strings.Contains(strings.ToLower(repository.FullName), query) ||
				strings.Contains(strings.ToLower(repository.Description), query) {
				matches = append(matches, repository)
			}
		}
		page, nextPage := paginateRepositories(matches, input.Page, input.PerPage)
		return nil, ToolResponse[RepositoryPage]{
			Data: &page,
			Meta: ResponseMeta{RequestID: requestID, NextPage: nextPage},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_repository",
		Description: "Return metadata and permissions for an accessible Gogs repository.",
		Annotations: readOnlyAnnotations("Get repository"),
		InputSchema: getRepositoryInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getRepositoryInput) (*mcp.CallToolResult, ToolResponse[Repository], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "get_repository")
		repository, err := client.GetRepository(ctx, input.Owner, input.Repo)
		if err != nil {
			classified := gogs.AsError(err)
			return &mcp.CallToolResult{IsError: true}, ToolResponse[Repository]{
				Error: mapToolError(classified),
				Meta:  ResponseMeta{RequestID: requestID},
			}, nil
		}
		output := mapRepository(repository)
		return nil, ToolResponse[Repository]{
			Data: &output,
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})
}

func paginateRepositories(repositories []gogs.Repository, page, perPage int) (RepositoryPage, *int) {
	sorted := slices.Clone(repositories)
	slices.SortStableFunc(sorted, func(left, right gogs.Repository) int {
		if order := cmp.Compare(left.FullName, right.FullName); order != 0 {
			return order
		}
		return cmp.Compare(left.ID, right.ID)
	})

	start := len(sorted)
	if page <= (len(sorted)+perPage-1)/perPage {
		start = (page - 1) * perPage
	}
	end := min(start+perPage, len(sorted))
	items := make([]Repository, end-start)
	for index, repository := range sorted[start:end] {
		items[index] = mapRepository(repository)
	}

	var nextPage *int
	if end < len(sorted) {
		next := page + 1
		nextPage = &next
	}
	return RepositoryPage{
		Repositories: items,
		Page:         page,
		PerPage:      perPage,
		Total:        len(sorted),
	}, nextPage
}

func mapRepository(repository gogs.Repository) Repository {
	return Repository{
		ID:            repository.ID,
		Name:          repository.Name,
		FullName:      repository.FullName,
		Owner:         repository.Owner,
		Description:   repository.Description,
		DefaultBranch: repository.DefaultBranch,
		Private:       repository.Private,
		CloneURL:      repository.CloneURL,
		WebURL:        repository.WebURL,
		Permissions: RepositoryPermissions{
			Pull:  repository.Permissions.Pull,
			Push:  repository.Permissions.Push,
			Admin: repository.Permissions.Admin,
		},
	}
}

func repositoryPageError(requestID string, err error) (*mcp.CallToolResult, ToolResponse[RepositoryPage], error) {
	classified := gogs.AsError(err)
	return &mcp.CallToolResult{IsError: true}, ToolResponse[RepositoryPage]{
		Error: mapToolError(classified),
		Meta:  ResponseMeta{RequestID: requestID},
	}, nil
}

func mapToolError(classified *gogs.Error) *ToolError {
	return &ToolError{
		Code:      string(classified.Code),
		Message:   classified.Message,
		Retryable: classified.Retryable,
	}
}

func readOnlyAnnotations(title string) *mcp.ToolAnnotations {
	destructive := false
	openWorld := true
	return &mcp.ToolAnnotations{
		DestructiveHint: &destructive,
		IdempotentHint:  true,
		OpenWorldHint:   &openWorld,
		ReadOnlyHint:    true,
		Title:           title,
	}
}

func paginationInputSchema() *jsonschema.Schema {
	return objectSchema(map[string]*jsonschema.Schema{
		"page":     integerSchema("Page number starting at 1.", defaultPage, 0),
		"per_page": integerSchema("Repositories per page, from 1 through 100.", defaultPerPage, maximumPerPage),
	}, nil)
}

func searchRepositoriesInputSchema() *jsonschema.Schema {
	properties := paginationInputSchema().Properties
	properties["query"] = &jsonschema.Schema{
		Type:        "string",
		Description: "Case-insensitive text to match against repository name, full name, or description.",
		MinLength:   intPointer(1),
	}
	return objectSchema(properties, []string{"query"})
}

func getRepositoryInputSchema() *jsonschema.Schema {
	return objectSchema(map[string]*jsonschema.Schema{
		"owner": {
			Type:        "string",
			Description: "Repository owner username.",
			MinLength:   intPointer(1),
		},
		"repo": {
			Type:        "string",
			Description: "Repository name.",
			MinLength:   intPointer(1),
		},
	}, []string{"owner", "repo"})
}

func objectSchema(properties map[string]*jsonschema.Schema, required []string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           properties,
		Required:             required,
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
}

func integerSchema(description string, defaultValue, maximum int) *jsonschema.Schema {
	minimum := float64(1)
	schema := &jsonschema.Schema{
		Type:        "integer",
		Description: description,
		Default:     json.RawMessage(strconv.Itoa(defaultValue)),
		Minimum:     &minimum,
	}
	if maximum > 0 {
		maximumValue := float64(maximum)
		schema.Maximum = &maximumValue
	}
	return schema
}

func intPointer(value int) *int {
	return &value
}
