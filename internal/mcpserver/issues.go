package mcpserver

import (
	"context"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"gogs-mcp/internal/gogs"

	"github.com/cockroachdb/errors"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMaxComments = 100
	maximumMaxComments = 500
	maxIssueTitleRunes = 255
	maxIssueBodyBytes  = 1 << 20
	// issueToolScope explains the role of Gogs issues relative to Jira; it is
	// part of every issue tool description.
	issueToolScope = "Gogs issues track repository-internal discussion; Jira remains the requirements system of record and this server does not sync with Jira."
)

type listIssuesInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	State string `json:"state,omitempty"`
	Page  int    `json:"page,omitempty"`
}

type getIssueInput struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int64  `json:"number"`
}

type listIssueCommentsInput struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	Number      int64  `json:"number"`
	Since       string `json:"since,omitempty"`
	MaxComments int    `json:"max_comments,omitempty"`
}

type createIssueInput struct {
	Owner     string   `json:"owner"`
	Repo      string   `json:"repo"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Assignee  string   `json:"assignee,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Milestone string   `json:"milestone,omitempty"`
}

// updateIssueInput keeps optional fields as pointers so that an omitted field
// (nil) means "keep the current value" while an explicit value — including an
// empty string, which clears the assignee or milestone — is sent to Gogs.
type updateIssueInput struct {
	Owner     string  `json:"owner"`
	Repo      string  `json:"repo"`
	Number    int64   `json:"number"`
	Title     *string `json:"title,omitempty"`
	Body      *string `json:"body,omitempty"`
	Assignee  *string `json:"assignee,omitempty"`
	Milestone *string `json:"milestone,omitempty"`
	State     string  `json:"state,omitempty"`
}

type createIssueCommentInput struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int64  `json:"number"`
	Body   string `json:"body"`
}

func registerIssueTools(server *mcp.Server, client Client, writeEnabled bool) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_issues",
		Description: "List the issues of a Gogs repository, newest state page first. " + issueToolScope,
		Annotations: readOnlyAnnotations("List issues"),
		InputSchema: listIssuesInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listIssuesInput) (*mcp.CallToolResult, ToolResponse[IssuePage], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "list_issues")
		state := input.State
		if state == "" {
			state = gogs.IssueStateOpen
		}
		page := input.Page
		if page == 0 {
			page = defaultPage
		}
		issues, nextPage, err := client.ListIssues(ctx, input.Owner, input.Repo, state, page)
		if err != nil {
			result, response := issuePageError(requestID, err)
			return result, response, nil
		}
		output := IssuePage{
			Issues: mapIssueSummaries(issues),
			State:  state,
			Page:   page,
		}
		var next *int
		if nextPage > 0 {
			next = &nextPage
		}
		return nil, ToolResponse[IssuePage]{
			Data: &output,
			Meta: ResponseMeta{RequestID: requestID, NextPage: next},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_issue",
		Description: "Return one Gogs issue with its body, creator, assignee, labels, milestone, and comment count. " + issueToolScope,
		Annotations: readOnlyAnnotations("Get issue"),
		InputSchema: getIssueInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getIssueInput) (*mcp.CallToolResult, ToolResponse[Issue], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "get_issue")
		issue, err := client.GetIssue(ctx, input.Owner, input.Repo, input.Number)
		if err != nil {
			result, response := issueError(requestID, err)
			return result, response, nil
		}
		output := mapIssue(issue)
		return nil, ToolResponse[Issue]{
			Data: &output,
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_issue_comments",
		Description: "List the comments of one Gogs issue, newest last, optionally restricted to comments created since an RFC3339 timestamp. " + issueToolScope,
		Annotations: readOnlyAnnotations("List issue comments"),
		InputSchema: listIssueCommentsInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listIssueCommentsInput) (*mcp.CallToolResult, ToolResponse[IssueCommentPage], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "list_issue_comments")
		query, toolErr := resolveIssueCommentsInput(input)
		if toolErr != nil {
			result, response := issueCommentPageError(requestID, toolErr)
			return result, response, nil
		}
		comments, err := client.ListIssueComments(ctx, input.Owner, input.Repo, input.Number, query.since)
		if err != nil {
			result, response := issueCommentPageError(requestID, err)
			return result, response, nil
		}

		meta := ResponseMeta{RequestID: requestID}
		if len(comments) > query.maxComments {
			comments = comments[:query.maxComments]
			meta.Truncated = true
			meta.Warnings = append(meta.Warnings, "The comment list stopped at the max_comments limit of "+strconv.Itoa(query.maxComments)+".")
		}
		selected, dropped := commentsWithin(mapIssueComments(comments), maximumFileTextBytes)
		if dropped {
			meta.Truncated = true
			meta.Warnings = append(meta.Warnings, "Comments reached the 64 KiB structured output limit.")
		}
		output := IssueCommentPage{
			Comments: selected,
			Since:    input.Since,
			Max:      query.maxComments,
		}
		return nil, ToolResponse[IssueCommentPage]{
			Data: &output,
			Meta: meta,
		}, nil
	})

	if !writeEnabled {
		return
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_issue",
		Description: "Create an issue in a Gogs repository with a title and an optional body. An assignee, labels, or a milestone requires repository write access and is verified to exist before the issue is created. " + issueToolScope,
		Annotations: writeAnnotations("Create issue", false),
		InputSchema: createIssueInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input createIssueInput) (*mcp.CallToolResult, ToolResponse[Issue], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "create_issue")
		options, toolErr := resolveCreateIssueInput(ctx, client, input)
		if toolErr != nil {
			result, response := issueError(requestID, toolErr)
			return result, response, nil
		}
		issue, err := client.CreateIssue(ctx, input.Owner, input.Repo, options)
		if err != nil {
			result, response := issueError(requestID, err)
			return result, response, nil
		}
		output := mapIssue(issue)
		return nil, ToolResponse[Issue]{
			Data: &output,
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_issue",
		Description: "Update an issue in a Gogs repository. The issue author may change the title, body, and state; the assignee or milestone additionally requires repository write access and is verified to exist before the update is sent. Omitted fields keep their current value, and an empty assignee or milestone clears it. " + issueToolScope,
		Annotations: writeAnnotations("Update issue", true),
		InputSchema: updateIssueInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input updateIssueInput) (*mcp.CallToolResult, ToolResponse[Issue], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "update_issue")
		options, toolErr := resolveUpdateIssueInput(ctx, client, input)
		if toolErr != nil {
			result, response := issueError(requestID, toolErr)
			return result, response, nil
		}
		issue, err := client.UpdateIssue(ctx, input.Owner, input.Repo, input.Number, options)
		if err != nil {
			result, response := issueError(requestID, err)
			return result, response, nil
		}
		output := mapIssue(issue)
		return nil, ToolResponse[Issue]{
			Data: &output,
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_issue_comment",
		Description: "Add a comment to a Gogs issue as the authenticated user. " + issueToolScope,
		Annotations: writeAnnotations("Create issue comment", false),
		InputSchema: createIssueCommentInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input createIssueCommentInput) (*mcp.CallToolResult, ToolResponse[IssueComment], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "create_issue_comment")
		if err := validateCommentBody(input.Body); err != nil {
			result, response := issueCommentError(requestID, err)
			return result, response, nil
		}
		comment, err := client.CreateIssueComment(ctx, input.Owner, input.Repo, input.Number, input.Body)
		if err != nil {
			result, response := issueCommentError(requestID, err)
			return result, response, nil
		}
		output := mapIssueComment(comment)
		return nil, ToolResponse[IssueComment]{
			Data: &output,
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})
}

type issueCommentsQuery struct {
	since       string
	maxComments int
}

// resolveIssueCommentsInput validates the tool input before Gogs is contacted:
// an invalid since must not reach the server, and the comment bound is the
// caller's responsibility because Gogs v0.14.2 does not paginate this endpoint.
func resolveIssueCommentsInput(input listIssueCommentsInput) (issueCommentsQuery, *gogs.Error) {
	if err := gogs.ValidateIssueSince(input.Since); err != nil {
		return issueCommentsQuery{}, &gogs.Error{Code: gogs.CodeInvalidArgument, Message: err.Error()}
	}
	maxComments := defaultMaxComments
	if input.MaxComments != 0 {
		maxComments = input.MaxComments
	}
	if maxComments < 1 || maxComments > maximumMaxComments {
		return issueCommentsQuery{}, &gogs.Error{
			Code:    gogs.CodeInvalidArgument,
			Message: errors.Newf("max_comments must be between 1 and %d", maximumMaxComments).Error(),
		}
	}
	return issueCommentsQuery{since: input.Since, maxComments: maxComments}, nil
}

// resolveCreateIssueInput validates the issue fields locally and resolves the
// administrative references against Gogs. Gogs silently drops assignee, label,
// and milestone references from users without repository write access, so any
// managed field first requires repository push permission and every reference
// is verified to exist before the issue is created.
func resolveCreateIssueInput(ctx context.Context, client Client, input createIssueInput) (gogs.CreateIssueOptions, *gogs.Error) {
	if utf8.RuneCountInString(input.Title) > maxIssueTitleRunes {
		return gogs.CreateIssueOptions{}, &gogs.Error{
			Code:    gogs.CodeInvalidArgument,
			Message: errors.Newf("title must be at most %d characters", maxIssueTitleRunes).Error(),
		}
	}
	if len(input.Body) > maxIssueBodyBytes {
		return gogs.CreateIssueOptions{}, &gogs.Error{
			Code:    gogs.CodeInvalidArgument,
			Message: errors.Newf("body must be at most %d bytes", maxIssueBodyBytes).Error(),
		}
	}
	options := gogs.CreateIssueOptions{Title: input.Title, Body: input.Body}
	if input.Assignee == "" && len(input.Labels) == 0 && input.Milestone == "" {
		return options, nil
	}

	repository, err := client.GetRepository(ctx, input.Owner, input.Repo)
	if err != nil {
		return gogs.CreateIssueOptions{}, gogs.AsError(err)
	}
	if !repository.Permissions.Push {
		return gogs.CreateIssueOptions{}, &gogs.Error{
			Code:    gogs.CodePermissionDenied,
			Message: "Setting an assignee, labels, or a milestone requires repository write access; create the issue with only a title and body instead.",
		}
	}

	if len(input.Labels) > 0 {
		labels, err := client.ListRepositoryLabels(ctx, input.Owner, input.Repo)
		if err != nil {
			return gogs.CreateIssueOptions{}, gogs.AsError(err)
		}
		idsByName := make(map[string]int64, len(labels))
		for _, label := range labels {
			idsByName[label.Name] = label.ID
		}
		for _, name := range input.Labels {
			id, ok := idsByName[name]
			if !ok {
				return gogs.CreateIssueOptions{}, &gogs.Error{
					Code:    gogs.CodeInvalidArgument,
					Message: errors.Newf("the label %q does not exist in %s/%s", name, input.Owner, input.Repo).Error(),
				}
			}
			options.LabelIDs = append(options.LabelIDs, id)
		}
	}

	if input.Milestone != "" {
		milestones, err := client.ListRepositoryMilestones(ctx, input.Owner, input.Repo)
		if err != nil {
			return gogs.CreateIssueOptions{}, gogs.AsError(err)
		}
		for _, milestone := range milestones {
			if milestone.Title == input.Milestone {
				options.MilestoneID = milestone.ID
				break
			}
		}
		if options.MilestoneID == 0 {
			return gogs.CreateIssueOptions{}, &gogs.Error{
				Code:    gogs.CodeInvalidArgument,
				Message: errors.Newf("the milestone %q does not exist in %s/%s", input.Milestone, input.Owner, input.Repo).Error(),
			}
		}
	}

	if input.Assignee != "" {
		exists, err := client.UserExists(ctx, input.Assignee)
		if err != nil {
			return gogs.CreateIssueOptions{}, gogs.AsError(err)
		}
		if !exists {
			return gogs.CreateIssueOptions{}, &gogs.Error{
				Code:    gogs.CodeInvalidArgument,
				Message: errors.Newf("the assignee %q does not exist", input.Assignee).Error(),
			}
		}
		options.Assignee = input.Assignee
	}

	return options, nil
}

// resolveUpdateIssueInput validates the update fields locally and resolves the
// managed references against Gogs. An update that touches no field is rejected
// before Gogs is contacted. The issue author may change the title, body, and
// state, but Gogs silently ignores assignee and milestone changes from users
// without repository write access, so those fields first require repository
// push permission and every reference is verified to exist before the update
// is sent.
func resolveUpdateIssueInput(ctx context.Context, client Client, input updateIssueInput) (gogs.UpdateIssueOptions, *gogs.Error) {
	options := gogs.UpdateIssueOptions{}
	updates := 0
	if input.Title != nil {
		updates++
		if utf8.RuneCountInString(*input.Title) > maxIssueTitleRunes {
			return gogs.UpdateIssueOptions{}, &gogs.Error{
				Code:    gogs.CodeInvalidArgument,
				Message: errors.Newf("title must be at most %d characters", maxIssueTitleRunes).Error(),
			}
		}
		options.Title = *input.Title
	}
	if input.Body != nil {
		updates++
		if len(*input.Body) > maxIssueBodyBytes {
			return gogs.UpdateIssueOptions{}, &gogs.Error{
				Code:    gogs.CodeInvalidArgument,
				Message: errors.Newf("body must be at most %d bytes", maxIssueBodyBytes).Error(),
			}
		}
		options.Body = input.Body
	}
	if input.State != "" {
		updates++
		state := input.State
		options.State = &state
	}
	if input.Assignee == nil && input.Milestone == nil {
		if updates == 0 {
			return gogs.UpdateIssueOptions{}, &gogs.Error{
				Code:    gogs.CodeInvalidArgument,
				Message: "Provide at least one of title, body, assignee, milestone, or state to update.",
			}
		}
		return options, nil
	}

	repository, err := client.GetRepository(ctx, input.Owner, input.Repo)
	if err != nil {
		return gogs.UpdateIssueOptions{}, gogs.AsError(err)
	}
	if !repository.Permissions.Push {
		return gogs.UpdateIssueOptions{}, &gogs.Error{
			Code:    gogs.CodePermissionDenied,
			Message: "Changing the assignee or milestone requires repository write access; update the title, body, or state instead.",
		}
	}

	if input.Assignee != nil {
		if *input.Assignee != "" {
			exists, err := client.UserExists(ctx, *input.Assignee)
			if err != nil {
				return gogs.UpdateIssueOptions{}, gogs.AsError(err)
			}
			if !exists {
				return gogs.UpdateIssueOptions{}, &gogs.Error{
					Code:    gogs.CodeInvalidArgument,
					Message: errors.Newf("the assignee %q does not exist", *input.Assignee).Error(),
				}
			}
		}
		options.Assignee = input.Assignee
	}

	if input.Milestone != nil {
		if *input.Milestone != "" {
			milestones, err := client.ListRepositoryMilestones(ctx, input.Owner, input.Repo)
			if err != nil {
				return gogs.UpdateIssueOptions{}, gogs.AsError(err)
			}
			var milestoneID int64
			for _, milestone := range milestones {
				if milestone.Title == *input.Milestone {
					milestoneID = milestone.ID
					break
				}
			}
			if milestoneID == 0 {
				return gogs.UpdateIssueOptions{}, &gogs.Error{
					Code:    gogs.CodeInvalidArgument,
					Message: errors.Newf("the milestone %q does not exist in %s/%s", *input.Milestone, input.Owner, input.Repo).Error(),
				}
			}
			options.Milestone = &milestoneID
		} else {
			cleared := int64(0)
			options.Milestone = &cleared
		}
	}

	return options, nil
}

// validateCommentBody enforces the comment body bounds before Gogs is
// contacted: an empty body is a pointless write and an oversized one would be
// rejected after content was already transmitted.
func validateCommentBody(body string) *gogs.Error {
	if body == "" {
		return &gogs.Error{Code: gogs.CodeInvalidArgument, Message: "The comment body must not be empty."}
	}
	if len(body) > maxIssueBodyBytes {
		return &gogs.Error{
			Code:    gogs.CodeInvalidArgument,
			Message: errors.Newf("body must be at most %d bytes", maxIssueBodyBytes).Error(),
		}
	}
	return nil
}

func mapIssueSummaries(issues []gogs.IssueSummary) []IssueSummary {
	mapped := make([]IssueSummary, len(issues))
	for index, issue := range issues {
		mapped[index] = IssueSummary{
			Number:      issue.Number,
			Title:       issue.Title,
			State:       issue.State,
			User:        mapIssueUser(issue.User),
			Labels:      mapIssueLabels(issue.Labels),
			NumComments: issue.NumComments,
			CreatedAt:   issue.CreatedAt,
			UpdatedAt:   issue.UpdatedAt,
		}
	}
	return mapped
}

func mapIssue(issue gogs.Issue) Issue {
	var assignee *IssueUser
	if issue.Assignee != nil {
		mapped := mapIssueUser(*issue.Assignee)
		assignee = &mapped
	}
	var milestone *IssueMilestone
	if issue.Milestone != nil {
		milestone = &IssueMilestone{Title: issue.Milestone.Title, State: issue.Milestone.State}
	}
	return Issue{
		Number:      issue.Number,
		Title:       issue.Title,
		Body:        issue.Body,
		State:       issue.State,
		User:        mapIssueUser(issue.User),
		Assignee:    assignee,
		Labels:      mapIssueLabels(issue.Labels),
		Milestone:   milestone,
		NumComments: issue.NumComments,
		CreatedAt:   issue.CreatedAt,
		UpdatedAt:   issue.UpdatedAt,
		WebURL:      issue.WebURL,
	}
}

func mapIssueComments(comments []gogs.IssueComment) []IssueComment {
	mapped := make([]IssueComment, len(comments))
	for index, comment := range comments {
		mapped[index] = mapIssueComment(comment)
	}
	return mapped
}

func mapIssueComment(comment gogs.IssueComment) IssueComment {
	return IssueComment{
		ID:        comment.ID,
		User:      mapIssueUser(comment.User),
		Body:      comment.Body,
		CreatedAt: comment.CreatedAt,
		UpdatedAt: comment.UpdatedAt,
	}
}

func mapIssueUser(user gogs.User) IssueUser {
	return IssueUser{Username: user.Username, FullName: user.FullName}
}

func mapIssueLabels(labels []gogs.IssueLabel) []IssueLabel {
	mapped := make([]IssueLabel, len(labels))
	for index, label := range labels {
		mapped[index] = IssueLabel{Name: label.Name, Color: label.Color}
	}
	return mapped
}

// commentsWithin keeps the comments that fit in the shared structured output
// byte budget and reports whether any were dropped.
func commentsWithin(comments []IssueComment, maximum int) ([]IssueComment, bool) {
	selected := make([]IssueComment, 0, min(len(comments), 64))
	encoded := 0
	dropped := false
	for _, comment := range comments {
		payload, err := json.Marshal(comment)
		if err != nil {
			dropped = true
			break
		}
		separator := 0
		if len(selected) > 0 {
			separator = 1
		}
		if encoded+separator+len(payload) > maximum {
			dropped = true
			break
		}
		encoded += separator + len(payload)
		selected = append(selected, comment)
	}
	return selected, dropped
}

func issuePageError(requestID string, err error) (*mcp.CallToolResult, ToolResponse[IssuePage]) {
	classified := gogs.AsError(err)
	return &mcp.CallToolResult{IsError: true}, ToolResponse[IssuePage]{
		Error: mapToolError(classified),
		Meta:  ResponseMeta{RequestID: requestID},
	}
}

func issueError(requestID string, err error) (*mcp.CallToolResult, ToolResponse[Issue]) {
	classified := gogs.AsError(err)
	return &mcp.CallToolResult{IsError: true}, ToolResponse[Issue]{
		Error: mapToolError(classified),
		Meta:  ResponseMeta{RequestID: requestID},
	}
}

func issueCommentPageError(requestID string, err error) (*mcp.CallToolResult, ToolResponse[IssueCommentPage]) {
	classified := gogs.AsError(err)
	return &mcp.CallToolResult{IsError: true}, ToolResponse[IssueCommentPage]{
		Error: mapToolError(classified),
		Meta:  ResponseMeta{RequestID: requestID},
	}
}

func issueCommentError(requestID string, err error) (*mcp.CallToolResult, ToolResponse[IssueComment]) {
	classified := gogs.AsError(err)
	return &mcp.CallToolResult{IsError: true}, ToolResponse[IssueComment]{
		Error: mapToolError(classified),
		Meta:  ResponseMeta{RequestID: requestID},
	}
}

func listIssuesInputSchema() *jsonschema.Schema {
	return objectSchema(map[string]*jsonschema.Schema{
		"owner": stringSchema("Repository owner username.", true, false),
		"repo":  stringSchema("Repository name.", true, false),
		"state": {
			Type:        "string",
			Description: "Issue state to list. Defaults to open.",
			Enum:        []any{gogs.IssueStateOpen, gogs.IssueStateClosed},
			Default:     json.RawMessage(`"open"`),
		},
		"page": integerSchema("Page number starting at 1. The page size is fixed by the Gogs server.", defaultPage, 0),
	}, []string{"owner", "repo"})
}

func getIssueInputSchema() *jsonschema.Schema {
	return objectSchema(map[string]*jsonschema.Schema{
		"owner":  stringSchema("Repository owner username.", true, false),
		"repo":   stringSchema("Repository name.", true, false),
		"number": integerSchema("Issue number.", 1, 0),
	}, []string{"owner", "repo", "number"})
}

func listIssueCommentsInputSchema() *jsonschema.Schema {
	return objectSchema(map[string]*jsonschema.Schema{
		"owner":  stringSchema("Repository owner username.", true, false),
		"repo":   stringSchema("Repository name.", true, false),
		"number": integerSchema("Issue number.", 1, 0),
		"since": {
			Type:        "string",
			Description: "Only return comments created at or after this RFC3339 timestamp, for example 2024-01-02T15:04:05Z.",
		},
		"max_comments": integerSchema("Maximum number of comments to return, from 1 through 500. Defaults to 100.", defaultMaxComments, maximumMaxComments),
	}, []string{"owner", "repo", "number"})
}

func createIssueInputSchema() *jsonschema.Schema {
	return objectSchema(map[string]*jsonschema.Schema{
		"owner": stringSchema("Repository owner username.", true, false),
		"repo":  stringSchema("Repository name.", true, false),
		"title": {
			Type:        "string",
			Description: "Issue title.",
			MinLength:   intPointer(1),
			MaxLength:   intPointer(maxIssueTitleRunes),
		},
		"body": stringSchema("Issue body in Markdown.", false, false),
		"assignee": {
			Type:        "string",
			Description: "Username to assign the issue to. Requires repository write access.",
			MinLength:   intPointer(1),
		},
		"labels": {
			Type:        "array",
			Description: "Names of labels to attach. Requires repository write access; unknown names are rejected instead of silently dropped.",
			Items:       &jsonschema.Schema{Type: "string", MinLength: intPointer(1)},
			UniqueItems: true,
		},
		"milestone": {
			Type:        "string",
			Description: "Title of the milestone to assign. Requires repository write access; unknown titles are rejected instead of silently dropped.",
			MinLength:   intPointer(1),
		},
	}, []string{"owner", "repo", "title"})
}

func updateIssueInputSchema() *jsonschema.Schema {
	return objectSchema(map[string]*jsonschema.Schema{
		"owner":  stringSchema("Repository owner username.", true, false),
		"repo":   stringSchema("Repository name.", true, false),
		"number": integerSchema("Issue number.", 1, 0),
		"title": {
			Type:        "string",
			Description: "New issue title.",
			MinLength:   intPointer(1),
			MaxLength:   intPointer(maxIssueTitleRunes),
		},
		"body": {
			Type:        "string",
			Description: "New issue body in Markdown. An empty string clears the body.",
		},
		"assignee": {
			Type:        "string",
			Description: "Username to assign the issue to. Requires repository write access; an empty string clears the assignee.",
		},
		"milestone": {
			Type:        "string",
			Description: "Title of the milestone to assign. Requires repository write access and the title must exist; an empty string clears the milestone.",
		},
		"state": {
			Type:        "string",
			Description: "New issue state.",
			Enum:        []any{gogs.IssueStateOpen, gogs.IssueStateClosed},
		},
	}, []string{"owner", "repo", "number"})
}

func createIssueCommentInputSchema() *jsonschema.Schema {
	return objectSchema(map[string]*jsonschema.Schema{
		"owner":  stringSchema("Repository owner username.", true, false),
		"repo":   stringSchema("Repository name.", true, false),
		"number": integerSchema("Issue number.", 1, 0),
		"body": {
			Type:        "string",
			Description: "Comment body in Markdown.",
			MinLength:   intPointer(1),
		},
	}, []string{"owner", "repo", "number", "body"})
}

// writeAnnotations mirrors readOnlyAnnotations for tools that change Gogs
// state: the write is neither read-only nor idempotent, and the destructive
// hint distinguishes updates that replace existing content from additive
// writes.
func writeAnnotations(title string, destructive bool) *mcp.ToolAnnotations {
	openWorld := true
	return &mcp.ToolAnnotations{
		DestructiveHint: &destructive,
		OpenWorldHint:   &openWorld,
		Title:           title,
	}
}
