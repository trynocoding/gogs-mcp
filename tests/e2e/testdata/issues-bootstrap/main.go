package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cockroachdb/errors"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/database"
)

type identity struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

type result struct {
	User               identity `json:"user"`
	Reader             identity `json:"reader"`
	Repository         string   `json:"repository"`
	OpenIssueNumber    int64    `json:"open_issue_number"`
	CommentIssueNumber int64    `json:"comment_issue_number"`
	ClosedIssueNumber  int64    `json:"closed_issue_number"`
	FirstCommentAt     string   `json:"first_comment_at"`
	SecondCommentAt    string   `json:"second_comment_at"`
	LabelName          string   `json:"label_name"`
	MilestoneTitle     string   `json:"milestone_title"`
}

func main() {
	if err := run(); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "Path to the Gogs configuration file.")
	flag.Parse()
	if *configPath == "" {
		return errors.New("config is required")
	}
	if err := conf.Init(*configPath); err != nil {
		return errors.Wrap(err, "initialize Gogs configuration")
	}
	conf.InitLogging(true)
	if err := database.NewEngine(); err != nil {
		return errors.Wrap(err, "initialize Gogs database")
	}

	ctx := context.Background()
	owner, ownerIdentity, err := createIdentity(ctx, "owner", "owner@example.test")
	if err != nil {
		return err
	}
	reader, readerIdentity, err := createIdentity(ctx, "reader", "reader@example.test")
	if err != nil {
		return err
	}

	repository, err := createRepository(owner, "issue-lab", "Issue tracking laboratory")
	if err != nil {
		return err
	}
	// The reader is a collaborator with read-only access, so it can create
	// plain issues through the API but cannot use administrative fields.
	if err = repository.AddCollaborator(reader); err != nil {
		return errors.Wrap(err, "add Gogs E2E issue reader")
	}
	if err = repository.ChangeCollaborationAccessMode(reader.ID, database.AccessModeRead); err != nil {
		return errors.Wrap(err, "set Gogs E2E issue reader access")
	}

	label := &database.Label{RepoID: repository.ID, Name: "bug", Color: "#ff0000"}
	if err = database.NewLabels(label); err != nil {
		return errors.Wrap(err, "create Gogs E2E label")
	}
	// NewLabels batch-inserts the label without setting its auto-increment
	// ID, so the label must be read back before it can be attached to an issue.
	label, err = database.GetLabelOfRepoByName(repository.ID, label.Name)
	if err != nil {
		return errors.Wrap(err, "refetch Gogs E2E label")
	}
	milestone := &database.Milestone{RepoID: repository.ID, Name: "v1.0"}
	if err = database.NewMilestone(milestone); err != nil {
		return errors.Wrap(err, "create Gogs E2E milestone")
	}

	// A plain open issue with no comments.
	empty := &database.Issue{
		RepoID:   repository.ID,
		PosterID: owner.ID,
		Poster:   owner,
		Title:    "Open issue without comments",
		Content:  "Nothing has been said about this issue yet.",
	}
	if err = database.NewIssue(repository, empty, nil, nil); err != nil {
		return errors.Wrap(err, "create Gogs E2E empty issue")
	}

	// NewIssue reads the issue index from the in-memory repository, so the
	// repository must be refetched to pick up the incremented issue count.
	repository, err = refetchRepository(owner, repository)
	if err != nil {
		return err
	}

	// An open issue with labels, a milestone, an assignee, and two comments
	// created far enough apart to exercise the since filter.
	commented := &database.Issue{
		RepoID:      repository.ID,
		PosterID:    owner.ID,
		Poster:      owner,
		Title:       "Open issue with comments",
		Content:     "The parser fails on empty input.",
		AssigneeID:  owner.ID,
		MilestoneID: milestone.ID,
	}
	if err = database.NewIssue(repository, commented, nil, nil); err != nil {
		return errors.Wrap(err, "create Gogs E2E commented issue")
	}
	if err = database.NewIssueLabel(commented, label); err != nil {
		return errors.Wrap(err, "attach Gogs E2E label")
	}

	repository, err = refetchRepository(owner, repository)
	if err != nil {
		return err
	}
	first, err := database.CreateIssueComment(owner, repository, commented, "First comment before the cutoff.", nil)
	if err != nil {
		return errors.Wrap(err, "create Gogs E2E first comment")
	}
	time.Sleep(2 * time.Second)
	second, err := database.CreateIssueComment(owner, repository, commented, "Second comment after the cutoff.", nil)
	if err != nil {
		return errors.Wrap(err, "create Gogs E2E second comment")
	}
	// The created comments carry no Created timestamp until they are read back.
	first, err = database.GetCommentByID(first.ID)
	if err != nil {
		return errors.Wrap(err, "refetch Gogs E2E first comment")
	}
	second, err = database.GetCommentByID(second.ID)
	if err != nil {
		return errors.Wrap(err, "refetch Gogs E2E second comment")
	}

	// A closed issue.
	closed := &database.Issue{
		RepoID:   repository.ID,
		PosterID: owner.ID,
		Poster:   owner,
		Title:    "Closed issue",
		Content:  "This issue was already resolved.",
	}
	if err = database.NewIssue(repository, closed, nil, nil); err != nil {
		return errors.Wrap(err, "create Gogs E2E closed issue")
	}
	if err = closed.ChangeStatus(owner, repository, true); err != nil {
		return errors.Wrap(err, "close Gogs E2E issue")
	}

	if err = json.NewEncoder(os.Stdout).Encode(result{
		User:               ownerIdentity,
		Reader:             readerIdentity,
		Repository:         repository.Name,
		OpenIssueNumber:    empty.Index,
		CommentIssueNumber: commented.Index,
		ClosedIssueNumber:  closed.Index,
		FirstCommentAt:     first.Created.Format(time.RFC3339),
		SecondCommentAt:    second.Created.Format(time.RFC3339),
		LabelName:          label.Name,
		MilestoneTitle:     milestone.Name,
	}); err != nil {
		return errors.Wrap(err, "write issues bootstrap result")
	}
	return nil
}

func createIdentity(ctx context.Context, username, email string) (*database.User, identity, error) {
	user, err := database.Handle.Users().Create(ctx, username, email, database.CreateUserOptions{
		Password:  "smoke-password-" + username,
		FullName:  username + " user",
		Activated: true,
	})
	if err != nil {
		return nil, identity{}, errors.Wrap(err, "create Gogs E2E user")
	}
	token, err := database.Handle.AccessTokens().Create(ctx, user.ID, "e2e-"+username)
	if err != nil {
		return nil, identity{}, errors.Wrap(err, "create Gogs E2E personal access token")
	}
	return user, identity{
		UserID:   user.ID,
		Username: user.Name,
		Token:    token.Sha1,
	}, nil
}

func createRepository(owner *database.User, name, description string) (*database.Repository, error) {
	repository, err := database.CreateRepository(owner, owner, database.CreateRepoOptionsLegacy{
		Name:        name,
		Description: description,
		IsPrivate:   true,
	})
	if err != nil {
		return nil, errors.Wrap(err, "create Gogs E2E repository")
	}
	return repository, nil
}

func refetchRepository(owner *database.User, repository *database.Repository) (*database.Repository, error) {
	refetched, err := database.GetRepositoryByName(owner.ID, repository.Name)
	if err != nil {
		return nil, errors.Wrap(err, "refetch Gogs E2E repository")
	}
	return refetched, nil
}
