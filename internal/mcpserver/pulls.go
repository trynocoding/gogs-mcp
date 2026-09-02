package mcpserver

import (
	"context"

	"gogs-mcp/internal/gogs"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultDiffMaxBytes    = 256 * 1024
	maximumDiffMaxBytes    = 4 * 1024 * 1024
	defaultPullLimitShared = 30
	maximumPullLimitShared = 100
	// pullToolScope explains how pull request data is gathered on a Gogs
	// release without a pull request API; it is part of every pull request
	// tool description.
	pullToolScope = "Gogs v0.14.2 has no pull request API, so the data is assembled from the refs/pull/{number}/head refs that Gogs maintains on the base repository, the underlying issue, and the git protocol."
)

type listPullRequestsInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	State string `json:"state,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type getPullRequestInput struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int64  `json:"number"`
}

type getPullRequestDiffInput struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	Number   int64  `json:"number"`
	BaseRef  string `json:"base_ref,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

func registerPullTools(server *mcp.Server, client Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_pull_requests",
		Description: "List the pull requests of a Gogs repository, newest first, with the total number of advertised pull requests. " + pullToolScope,
		Annotations: readOnlyAnnotations("List pull requests"),
		InputSchema: listPullRequestsInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listPullRequestsInput) (*mcp.CallToolResult, ToolResponse[PullRequestPage], error) {
		requestID := newRequestID()
		state := input.State
		if state == "" {
			state = gogs.PullStateOpen
		}
		ctx = gogs.WithRequestMetadata(ctx, requestID, "list_pull_requests")
		pulls, total, err := client.ListPullRequests(ctx, input.Owner, input.Repo, state, input.Limit)
		if err != nil {
			result, response := contentError[PullRequestPage](requestID, err)
			return result, response, nil
		}
		return nil, ToolResponse[PullRequestPage]{
			Data: &PullRequestPage{
				PullRequests: mapPullRequestSummaries(pulls),
				State:        state,
				Limit:        limitOrDefault(input.Limit, defaultPullLimitShared),
				Total:        total,
			},
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_pull_request",
		Description: "Return one pull request by number with its head SHA, state, author, labels, and comment count. The base_ref field carries the repository default branch, because Gogs does not expose the actual pull request target branch. " + pullToolScope,
		Annotations: readOnlyAnnotations("Get pull request"),
		InputSchema: getPullRequestInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getPullRequestInput) (*mcp.CallToolResult, ToolResponse[PullRequest], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "get_pull_request")
		pull, err := client.GetPullRequest(ctx, input.Owner, input.Repo, input.Number)
		if err != nil {
			result, response := contentError[PullRequest](requestID, err)
			return result, response, nil
		}
		output := mapPullRequest(pull)
		return nil, ToolResponse[PullRequest]{
			Data: &output,
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_pull_request_diff",
		Description: "Return the merge-base unified diff, the changed files with additions and deletions, and the commits of one pull request. The diff is fetched over the git protocol and cached locally; use base_ref to diff against a branch other than the repository default branch. " + pullToolScope,
		Annotations: readOnlyAnnotations("Get pull request diff"),
		InputSchema: getPullRequestDiffInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getPullRequestDiffInput) (*mcp.CallToolResult, ToolResponse[PullRequestDiffPage], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "get_pull_request_diff")
		diff, err := client.GetPullRequestDiff(ctx, input.Owner, input.Repo, input.Number, input.BaseRef, input.MaxBytes)
		if err != nil {
			result, response := contentError[PullRequestDiffPage](requestID, err)
			return result, response, nil
		}
		meta := ResponseMeta{RequestID: requestID}
		if diff.Truncated {
			meta.Truncated = true
			meta.Warnings = append(meta.Warnings, "The rendered diff was cut off at the byte limit; raise max_bytes or narrow the review with base_ref and get_file.")
		}
		if diff.BaseRefAssumed {
			meta.Warnings = append(meta.Warnings, "Gogs does not expose the pull request target branch; the diff uses the repository default branch. Pass base_ref to override it.")
		}
		return nil, ToolResponse[PullRequestDiffPage]{
			Data: &PullRequestDiffPage{
				Number:         diff.Number,
				BaseRef:        diff.BaseRef,
				BaseRefAssumed: diff.BaseRefAssumed,
				MergeBase:      diff.MergeBase,
				Diff:           diff.Diff,
				Files:          mapDiffFiles(diff.Files),
				Commits:        mapPullCommits(diff.Commits),
				Truncated:      diff.Truncated,
			},
			Meta: meta,
		}, nil
	})
}

func mapPullRequestSummaries(pulls []gogs.PullRequestSummary) []PullRequestSummary {
	summaries := make([]PullRequestSummary, len(pulls))
	for index, pull := range pulls {
		summaries[index] = PullRequestSummary{
			Number:      pull.Number,
			Title:       pull.Title,
			State:       pull.State,
			User:        IssueUser{Username: pull.User.Username, FullName: pull.User.FullName},
			NumComments: pull.NumComments,
			CreatedAt:   pull.CreatedAt,
			UpdatedAt:   pull.UpdatedAt,
			HeadSHA:     pull.HeadSHA,
		}
	}
	return summaries
}

func mapPullRequest(pull gogs.PullRequest) PullRequest {
	labels := make([]IssueLabel, len(pull.Labels))
	for index, label := range pull.Labels {
		labels[index] = IssueLabel{Name: label.Name, Color: label.Color}
	}
	return PullRequest{
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		State:          pull.State,
		User:           IssueUser{Username: pull.User.Username, FullName: pull.User.FullName},
		Labels:         labels,
		NumComments:    pull.NumComments,
		CreatedAt:      pull.CreatedAt,
		UpdatedAt:      pull.UpdatedAt,
		WebURL:         pull.WebURL,
		HeadSHA:        pull.HeadSHA,
		BaseRef:        pull.BaseRef,
		BaseRefAssumed: pull.BaseRefAssumed,
	}
}

func limitOrDefault(value, defaultValue int) int {
	if value <= 0 {
		return defaultValue
	}
	return value
}

func mapDiffFiles(files []gogs.DiffFileStat) []DiffFileStat {
	stats := make([]DiffFileStat, len(files))
	for index, file := range files {
		stats[index] = DiffFileStat{
			Path:      file.Path,
			Status:    file.Status,
			Additions: file.Additions,
			Deletions: file.Deletions,
			IsBinary:  file.IsBinary,
		}
	}
	return stats
}

func mapPullCommits(commits []gogs.PullCommit) []PullCommit {
	entries := make([]PullCommit, len(commits))
	for index, commit := range commits {
		entries[index] = PullCommit{
			SHA:     commit.SHA,
			Message: commit.Message,
			Author:  commit.Author,
			Date:    commit.Date,
		}
	}
	return entries
}

func listPullRequestsInputSchema() *jsonschema.Schema {
	properties := repositoryIdentityProperties()
	properties["state"] = &jsonschema.Schema{
		Type:        "string",
		Description: "Pull request state to list: open, closed, or all. Defaults to open.",
		Enum:        []any{"open", "closed", "all"},
	}
	properties["limit"] = integerSchema("Maximum number of pull requests to return, from 1 through 100.", defaultPullLimitShared, maximumPullLimitShared)
	return objectSchema(properties, []string{"owner", "repo"})
}

func getPullRequestInputSchema() *jsonschema.Schema {
	properties := repositoryIdentityProperties()
	properties["number"] = integerSchema("Pull request number.", 1, 0)
	return objectSchema(properties, []string{"owner", "repo", "number"})
}

func getPullRequestDiffInputSchema() *jsonschema.Schema {
	properties := repositoryIdentityProperties()
	properties["number"] = integerSchema("Pull request number.", 1, 0)
	properties["base_ref"] = stringSchema("Branch to diff against. Defaults to the repository default branch, which is not guaranteed to be the pull request target.", false, false)
	properties["max_bytes"] = integerSchema("Maximum rendered diff size in bytes, from 1 through 4194304.", defaultDiffMaxBytes, maximumDiffMaxBytes)
	return objectSchema(properties, []string{"owner", "repo", "number"})
}
