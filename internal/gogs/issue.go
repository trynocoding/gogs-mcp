package gogs

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// Issue states accepted by the Gogs v0.14.2 list endpoint. Any other state
// value is interpreted by Gogs as open, so callers must not send it.
const (
	IssueStateOpen   = "open"
	IssueStateClosed = "closed"
)

// Issue is the stable issue representation exposed to tools.
type Issue struct {
	Number      int64
	Title       string
	Body        string
	State       string
	User        User
	Assignee    *User
	Labels      []IssueLabel
	Milestone   *IssueMilestone
	NumComments int
	CreatedAt   string
	UpdatedAt   string
	WebURL      string
}

// IssueSummary is the compact issue representation used by listings; the
// potentially large body is intentionally left out.
type IssueSummary struct {
	Number      int64
	Title       string
	State       string
	User        User
	Labels      []IssueLabel
	NumComments int
	CreatedAt   string
	UpdatedAt   string
}

// IssueLabel is one label attached to an issue.
type IssueLabel struct {
	Name  string
	Color string
}

// IssueMilestone is the milestone an issue belongs to.
type IssueMilestone struct {
	Title string
	State string
}

// IssueComment is one comment on an issue.
type IssueComment struct {
	ID        int64
	User      User
	Body      string
	CreatedAt string
	UpdatedAt string
}

// RepositoryLabel is a label defined in a repository.
type RepositoryLabel struct {
	ID    int64
	Name  string
	Color string
}

// RepositoryMilestone is a milestone defined in a repository.
type RepositoryMilestone struct {
	ID    int64
	Title string
	State string
}

// CreateIssueOptions are the fields of a new issue. Assignee, label, and
// milestone references are administrative fields that Gogs silently drops
// without repository write access.
type CreateIssueOptions struct {
	Title       string
	Body        string
	Assignee    string
	LabelIDs    []int64
	MilestoneID int64
}

type issueUserResponse = userResponse

type issueResponse struct {
	ID        int64                `json:"id"`
	Number    int64                `json:"number"`
	User      *issueUserResponse   `json:"user"`
	Title     string               `json:"title"`
	Body      string               `json:"body"`
	Labels    []issueLabelResponse `json:"labels"`
	Milestone *milestoneResponse   `json:"milestone"`
	Assignee  *issueUserResponse   `json:"assignee"`
	State     string               `json:"state"`
	Comments  int                  `json:"comments"`
	CreatedAt string               `json:"created_at"`
	UpdatedAt string               `json:"updated_at"`
}

type issueLabelResponse struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type labelResponse struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type milestoneResponse struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
}

type issueCommentResponse struct {
	ID        int64              `json:"id"`
	User      *issueUserResponse `json:"user"`
	Body      string             `json:"body"`
	CreatedAt string             `json:"created_at"`
	UpdatedAt string             `json:"updated_at"`
}

// ListIssues returns one page of repository issues in the given state. Gogs
// v0.14.2 fixes the page size server-side and reports the next page through
// its Link header; the returned next page is zero when there is none.
func (c *Client) ListIssues(ctx context.Context, owner, repo, state string, page int) ([]IssueSummary, int, error) {
	query := make(url.Values)
	query.Set("state", state)
	query.Set("page", strconv.Itoa(page))
	var response []issueResponse
	header, err := c.getJSONWithHeaders(ctx, &response, query, "repos", owner, repo, "issues")
	if err != nil {
		return nil, 0, err
	}
	issues := make([]IssueSummary, len(response))
	for index, entry := range response {
		issues[index] = mapIssueSummary(entry)
	}
	return issues, nextPageFromLinkHeader(header), nil
}

// GetIssue returns a single repository issue by its number. A missing or
// inaccessible issue reports the shared not-found code without revealing
// which of the two applies.
func (c *Client) GetIssue(ctx context.Context, owner, repo string, number int64) (Issue, error) {
	var response issueResponse
	if err := c.getJSON(ctx, &response, "repos", owner, repo, "issues", strconv.FormatInt(number, 10)); err != nil {
		classified := AsError(err)
		if classified.Code == CodeResourceNotFoundOrForbidden {
			return Issue{}, &Error{
				Code:       classified.Code,
				Message:    "The issue does not exist or the current user cannot access it.",
				Retryable:  classified.Retryable,
				HTTPStatus: classified.HTTPStatus,
				cause:      err,
			}
		}
		return Issue{}, err
	}
	return c.withWebURL(owner, repo, mapIssue(response)), nil
}

// CreateIssue creates an issue with exactly one POST request. A write is
// never retried automatically: whenever the request may have reached Gogs
// without a usable response, the outcome is reported as WRITE_OUTCOME_UNKNOWN
// so the caller can query the issue list instead of creating a duplicate.
func (c *Client) CreateIssue(ctx context.Context, owner, repo string, options CreateIssueOptions) (Issue, error) {
	payload := struct {
		Title     string  `json:"title"`
		Body      string  `json:"body"`
		Assignee  string  `json:"assignee,omitempty"`
		Milestone int64   `json:"milestone,omitempty"`
		Labels    []int64 `json:"labels,omitempty"`
	}{
		Title:     options.Title,
		Body:      options.Body,
		Assignee:  options.Assignee,
		Milestone: options.MilestoneID,
		Labels:    options.LabelIDs,
	}
	var response issueResponse
	if err := c.postJSON(ctx, &response, payload, "repos", owner, repo, "issues"); err != nil {
		return Issue{}, err
	}
	return c.withWebURL(owner, repo, mapIssue(response)), nil
}

// ListRepositoryLabels returns every label defined in the repository, closed
// or not, so callers can resolve label names to IDs before creating issues.
func (c *Client) ListRepositoryLabels(ctx context.Context, owner, repo string) ([]RepositoryLabel, error) {
	var response []labelResponse
	if err := c.getJSON(ctx, &response, "repos", owner, repo, "labels"); err != nil {
		return nil, err
	}
	labels := make([]RepositoryLabel, len(response))
	for index, entry := range response {
		labels[index] = RepositoryLabel(entry)
	}
	return labels, nil
}

// ListRepositoryMilestones returns every milestone of the repository, open
// and closed, so callers can resolve milestone titles to IDs before creating
// issues.
func (c *Client) ListRepositoryMilestones(ctx context.Context, owner, repo string) ([]RepositoryMilestone, error) {
	var response []milestoneResponse
	if err := c.getJSON(ctx, &response, "repos", owner, repo, "milestones"); err != nil {
		return nil, err
	}
	milestones := make([]RepositoryMilestone, len(response))
	for index, entry := range response {
		milestones[index] = RepositoryMilestone(entry)
	}
	return milestones, nil
}

// UserExists reports whether a user with the given username exists. Gogs
// v0.14.2 has no dedicated user lookup endpoint, but the repository listing
// of a user answers 404 exactly when that user does not exist.
func (c *Client) UserExists(ctx context.Context, username string) (bool, error) {
	var response []repositoryResponse
	if err := c.getJSON(ctx, &response, "users", username, "repos"); err != nil {
		if AsError(err).Code == CodeResourceNotFoundOrForbidden {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// withWebURL fills in the browser URL of an issue. The Gogs v0.14.2 issue
// payload carries no URL field, so it is derived from the instance root the
// same way Gogs builds issue.HTMLURL().
func (c *Client) withWebURL(owner, repo string, issue Issue) Issue {
	root := *c.apiRoot
	root.Path = strings.TrimSuffix(root.Path, "api/v1/")
	root.RawPath = ""
	issue.WebURL = strings.TrimSuffix(root.String(), "/") + "/" +
		url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/issues/" + strconv.FormatInt(issue.Number, 10)
	return issue
}

// ListIssueComments returns every comment of one issue that was created at or
// after the RFC3339 since timestamp; an empty since returns all comments.
// Gogs v0.14.2 does not paginate this endpoint, so callers bound the result.
func (c *Client) ListIssueComments(ctx context.Context, owner, repo string, number int64, since string) ([]IssueComment, error) {
	query := make(url.Values)
	if since != "" {
		query.Set("since", since)
	}
	var response []issueCommentResponse
	if err := c.getJSONWithQuery(ctx, &response, query, "repos", owner, repo, "issues", strconv.FormatInt(number, 10), "comments"); err != nil {
		return nil, err
	}
	comments := make([]IssueComment, len(response))
	for index, entry := range response {
		comments[index] = mapIssueComment(entry)
	}
	return comments, nil
}

func mapIssueSummary(response issueResponse) IssueSummary {
	summary := IssueSummary{
		Number:      response.Number,
		Title:       response.Title,
		State:       response.State,
		User:        mapIssueUser(derefIssueUser(response.User)),
		Labels:      mapIssueLabels(response.Labels),
		NumComments: response.Comments,
		CreatedAt:   response.CreatedAt,
		UpdatedAt:   response.UpdatedAt,
	}
	return summary
}

func mapIssue(response issueResponse) Issue {
	return Issue{
		Number:      response.Number,
		Title:       response.Title,
		Body:        response.Body,
		State:       response.State,
		User:        mapIssueUser(derefIssueUser(response.User)),
		Assignee:    mapIssueUserPointer(response.Assignee),
		Labels:      mapIssueLabels(response.Labels),
		Milestone:   mapIssueMilestone(response.Milestone),
		NumComments: response.Comments,
		CreatedAt:   response.CreatedAt,
		UpdatedAt:   response.UpdatedAt,
	}
}

func mapIssueComment(response issueCommentResponse) IssueComment {
	return IssueComment{
		ID:        response.ID,
		User:      mapIssueUser(derefIssueUser(response.User)),
		Body:      response.Body,
		CreatedAt: response.CreatedAt,
		UpdatedAt: response.UpdatedAt,
	}
}

func derefIssueUser(response *issueUserResponse) issueUserResponse {
	if response == nil {
		return issueUserResponse{}
	}
	return *response
}

func mapIssueUser(response issueUserResponse) User {
	username := response.Username
	if username == "" {
		username = response.Login
	}
	return User{
		ID:       response.ID,
		Username: username,
		FullName: response.FullName,
		Email:    response.Email,
	}
}

func mapIssueUserPointer(response *issueUserResponse) *User {
	if response == nil {
		return nil
	}
	user := mapIssueUser(*response)
	return &user
}

func mapIssueLabels(responses []issueLabelResponse) []IssueLabel {
	labels := make([]IssueLabel, len(responses))
	for index, entry := range responses {
		labels[index] = IssueLabel(entry)
	}
	return labels
}

func mapIssueMilestone(response *milestoneResponse) *IssueMilestone {
	if response == nil {
		return nil
	}
	return &IssueMilestone{Title: response.Title, State: response.State}
}

// nextPageFromLinkHeader extracts the page number of the rel="next" link of a
// Gogs Link header. Gogs v0.14.2 only emits links of the form
// <URL?page=N>; rel="next", so a missing or unexpected header means no page.
func nextPageFromLinkHeader(header http.Header) int {
	if header == nil {
		return 0
	}
	match := nextPageLinkPattern.FindStringSubmatch(header.Get("Link"))
	if match == nil {
		return 0
	}
	page, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return page
}

var nextPageLinkPattern = regexp.MustCompile(`<[^>]*[?&]page=([0-9]+)>;\s*rel="next"`)

// ValidateIssueSince reports whether the since timestamp is a valid RFC3339
// time, the only format Gogs v0.14.2 accepts for comment filters.
func ValidateIssueSince(since string) error {
	if since == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, since); err != nil {
		return errors.Wrap(err, "since must be an RFC3339 timestamp")
	}
	return nil
}
