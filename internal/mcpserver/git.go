package mcpserver

import (
	"cmp"
	"context"
	"slices"

	"gogs-mcp/internal/gogs"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultCommitLimit        = 30
	maximumCommitLimit        = 100
	maximumCommitSummaryRunes = 200
)

type getBranchInput struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

type getCommitInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
}

type listCommitsInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Limit int    `json:"limit,omitempty"`
}

func registerGitTools(server *mcp.Server, client Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_branches",
		Description: "List every branch of a Gogs repository with its head commit SHA.",
		Annotations: readOnlyAnnotations("List branches"),
		InputSchema: listBranchesInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listBranchesInput) (*mcp.CallToolResult, ToolResponse[BranchPage], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "list_branches")
		branches, err := client.ListBranches(ctx, input.Owner, input.Repo)
		if err != nil {
			result, response := contentError[BranchPage](requestID, err)
			return result, response, nil
		}
		page, nextPage := paginateBranches(branches, input.Page, input.PerPage)
		return nil, ToolResponse[BranchPage]{
			Data: &page,
			Meta: ResponseMeta{RequestID: requestID, NextPage: nextPage},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_branch",
		Description: "Return a single branch and its head commit SHA. Branch names may contain slashes.",
		Annotations: readOnlyAnnotations("Get branch"),
		InputSchema: getBranchInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getBranchInput) (*mcp.CallToolResult, ToolResponse[Branch], error) {
		requestID := newRequestID()
		if err := gogs.ValidateBranchName(input.Branch); err != nil {
			result, response := contentError[Branch](requestID, err)
			return result, response, nil
		}
		ctx = gogs.WithRequestMetadata(ctx, requestID, "get_branch")
		branch, err := client.GetBranch(ctx, input.Owner, input.Repo, input.Branch)
		if err != nil {
			result, response := contentError[Branch](requestID, err)
			return result, response, nil
		}
		return nil, ToolResponse[Branch]{
			Data: &Branch{Name: branch.Name, HeadSHA: branch.HeadSHA},
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_commits",
		Description: "Return the most recent commits of the default branch with the first line of each message. Gogs v0.14.2 always starts at HEAD and does not support paging or other refs.",
		Annotations: readOnlyAnnotations("List commits"),
		InputSchema: listCommitsInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listCommitsInput) (*mcp.CallToolResult, ToolResponse[CommitPage], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "list_commits")
		commits, err := client.ListCommits(ctx, input.Owner, input.Repo, input.Limit)
		if err != nil {
			result, response := contentError[CommitPage](requestID, err)
			return result, response, nil
		}
		page, meta := summarizeCommits(commits, input.Limit)
		meta.RequestID = requestID
		return nil, ToolResponse[CommitPage]{Data: &page, Meta: meta}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_commit",
		Description: "Return a single commit by SHA with author, committer, message subject, parents, and web URL. Gogs v0.14.2 only exposes the first line of the commit message.",
		Annotations: readOnlyAnnotations("Get commit"),
		InputSchema: getCommitInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getCommitInput) (*mcp.CallToolResult, ToolResponse[Commit], error) {
		requestID := newRequestID()
		if err := gogs.ValidateCommitRevision(input.SHA); err != nil {
			result, response := contentError[Commit](requestID, err)
			return result, response, nil
		}
		ctx = gogs.WithRequestMetadata(ctx, requestID, "get_commit")
		commit, err := client.GetCommit(ctx, input.Owner, input.Repo, input.SHA)
		if err != nil {
			result, response := contentError[Commit](requestID, err)
			return result, response, nil
		}
		output := Commit{
			SHA:        commit.SHA,
			Message:    commit.Message,
			WebURL:     commit.WebURL,
			Author:     CommitPerson(commit.Author),
			Committer:  CommitPerson(commit.Committer),
			ParentSHAs: commit.ParentSHAs,
		}
		return nil, ToolResponse[Commit]{
			Data: &output,
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})
}

type listBranchesInput struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Page    int    `json:"page,omitempty"`
	PerPage int    `json:"per_page,omitempty"`
}

func paginateBranches(branches []gogs.Branch, page, perPage int) (BranchPage, *int) {
	sorted := slices.Clone(branches)
	slices.SortStableFunc(sorted, func(left, right gogs.Branch) int {
		if order := cmp.Compare(left.Name, right.Name); order != 0 {
			return order
		}
		return cmp.Compare(left.HeadSHA, right.HeadSHA)
	})

	start := len(sorted)
	if page <= (len(sorted)+perPage-1)/perPage {
		start = (page - 1) * perPage
	}
	end := min(start+perPage, len(sorted))
	items := make([]Branch, end-start)
	for index, branch := range sorted[start:end] {
		items[index] = Branch{Name: branch.Name, HeadSHA: branch.HeadSHA}
	}

	var nextPage *int
	if end < len(sorted) {
		next := page + 1
		nextPage = &next
	}
	return BranchPage{
		Branches: items,
		Page:     page,
		PerPage:  perPage,
		Total:    len(sorted),
	}, nextPage
}

func summarizeCommits(commits []gogs.Commit, limit int) (CommitPage, ResponseMeta) {
	items := make([]CommitSummary, len(commits))
	shortened := false
	for index, commit := range commits {
		message, truncated := truncateRunes(commit.Message, maximumCommitSummaryRunes)
		shortened = shortened || truncated
		items[index] = CommitSummary{
			SHA:        commit.SHA,
			Message:    message,
			AuthorName: commit.Author.Name,
			AuthorDate: commit.Author.Date,
		}
	}
	meta := ResponseMeta{}
	if shortened {
		meta.Truncated = true
		meta.Warnings = append(meta.Warnings, "Commit messages longer than 200 characters were shortened. Gogs v0.14.2 only exposes the first line of a commit message; get_commit returns that line untruncated.")
	}
	return CommitPage{
		Commits: items,
		Limit:   limit,
	}, meta
}

// truncateRunes shortens value to at most maximum runes and reports whether
// any runes were dropped.
func truncateRunes(value string, maximum int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value, false
	}
	return string(runes[:maximum]), true
}

// repositoryIdentityProperties returns the owner and repo properties shared by
// the git tools. They must not include ref because none of the git tools
// resolves anything through a ref: list_commits is fixed to the default branch
// and get_commit takes the revision directly.
func repositoryIdentityProperties() map[string]*jsonschema.Schema {
	return map[string]*jsonschema.Schema{
		"owner": stringSchema("Repository owner username.", true, false),
		"repo":  stringSchema("Repository name.", true, false),
	}
}

func listBranchesInputSchema() *jsonschema.Schema {
	properties := repositoryIdentityProperties()
	properties["page"] = integerSchema("Page number starting at 1.", defaultPage, 0)
	properties["per_page"] = integerSchema("Branches per page, from 1 through 100.", defaultPerPage, maximumPerPage)
	return objectSchema(properties, []string{"owner", "repo"})
}

func listCommitsInputSchema() *jsonschema.Schema {
	properties := repositoryIdentityProperties()
	properties["limit"] = integerSchema("Maximum number of recent default-branch commits to return, from 1 through 100.", defaultCommitLimit, maximumCommitLimit)
	return objectSchema(properties, []string{"owner", "repo"})
}

func getBranchInputSchema() *jsonschema.Schema {
	properties := repositoryIdentityProperties()
	properties["branch"] = stringSchema("Branch name. May contain slashes.", true, false)
	return objectSchema(properties, []string{"owner", "repo", "branch"})
}

func getCommitInputSchema() *jsonschema.Schema {
	properties := repositoryIdentityProperties()
	properties["sha"] = stringSchema("Commit SHA, short SHA, or other slash-free Git revision. Unknown revisions return an error from Gogs.", true, false)
	return objectSchema(properties, []string{"owner", "repo", "sha"})
}
