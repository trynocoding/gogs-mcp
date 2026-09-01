package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	Owner            identity       `json:"owner"`
	Collaborator     identity       `json:"collaborator"`
	Outsider         identity       `json:"outsider"`
	SharedRepository string         `json:"shared_repository"`
	Refs             repositoryRefs `json:"refs"`
}

type repositoryRefs struct {
	DefaultBranch string `json:"default_branch"`
	FeatureBranch string `json:"feature_branch"`
	Tag           string `json:"tag"`
	MainCommitSHA string `json:"main_commit_sha"`
	FeatureSHA    string `json:"feature_sha"`
	TagSHA        string `json:"tag_sha"`
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
	collaborator, collaboratorIdentity, err := createIdentity(ctx, "collaborator", "collaborator@example.test")
	if err != nil {
		return err
	}
	outsider, outsiderIdentity, err := createIdentity(ctx, "outsider", "outsider@example.test")
	if err != nil {
		return err
	}

	shared, err := createRepository(owner, "private-shared", "Needle private collaborator repository")
	if err != nil {
		return err
	}
	refs, err := seedRepository(shared)
	if err != nil {
		return err
	}
	if err := shared.AddCollaborator(collaborator); err != nil {
		return errors.Wrap(err, "add repository collaborator")
	}
	if err := shared.ChangeCollaborationAccessMode(collaborator.ID, database.AccessModeRead); err != nil {
		return errors.Wrap(err, "set repository collaborator access")
	}
	if _, err := createRepository(collaborator, "collaborator-owned", "Repository owned by collaborator"); err != nil {
		return err
	}
	if _, err := createRepository(outsider, "outsider-private", "Repository owned by outsider"); err != nil {
		return err
	}

	if err := json.NewEncoder(os.Stdout).Encode(result{
		Owner:            ownerIdentity,
		Collaborator:     collaboratorIdentity,
		Outsider:         outsiderIdentity,
		SharedRepository: shared.Name,
		Refs:             refs,
	}); err != nil {
		return errors.Wrap(err, "write repository bootstrap result")
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

func seedRepository(repository *database.Repository) (repositoryRefs, error) {
	repositoryPath := repository.RepoPath()
	if err := os.RemoveAll(filepath.Join(repositoryPath, "hooks")); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "remove Gogs delegate hooks")
	}
	worktree, err := os.MkdirTemp("", "gogs-mcp-content-")
	if err != nil {
		return repositoryRefs{}, errors.Wrap(err, "create repository seed worktree")
	}
	defer func() {
		_ = os.RemoveAll(worktree)
	}()

	if err := runGit("", "clone", repositoryPath, worktree); err != nil {
		return repositoryRefs{}, err
	}
	if err := runGit(worktree, "config", "user.name", "Gogs MCP E2E"); err != nil {
		return repositoryRefs{}, err
	}
	if err := runGit(worktree, "config", "user.email", "gogs-mcp@example.test"); err != nil {
		return repositoryRefs{}, err
	}
	if err := os.MkdirAll(filepath.Join(worktree, "src"), 0o755); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "create source directory")
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "version.txt"), []byte("tag-version\n第二行\nthird\n"), 0o644); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "write version fixture")
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "unicode.txt"), []byte("你好，Gogs\nemoji 😀\n"), 0o644); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "write Unicode fixture")
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "binary.dat"), []byte{'G', 'O', 'G', 'S', 0, 0xff}, 0o644); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "write binary fixture")
	}
	largeContent := bytes.Repeat([]byte("large line with Unicode 数据\n"), 50000)
	if err := os.WriteFile(filepath.Join(worktree, "src", "large.txt"), largeContent, 0o644); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "write oversized fixture")
	}
	lineFixtures := make([]string, 250)
	for index := range lineFixtures {
		lineFixtures[index] = fmt.Sprintf("line-%03d", index+1)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "lines.txt"), []byte(strings.Join(lineFixtures, "\n")+"\n"), 0o644); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "write line range fixture")
	}
	if err := os.Symlink("version.txt", filepath.Join(worktree, "src", "version-link")); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "create symlink fixture")
	}
	if err := runGit(worktree, "add", "--all"); err != nil {
		return repositoryRefs{}, err
	}
	if err := runGit(worktree, "commit", "-m", "Add content fixtures"); err != nil {
		return repositoryRefs{}, err
	}
	baseSHA, err := gitOutput(worktree, "rev-parse", "HEAD")
	if err != nil {
		return repositoryRefs{}, err
	}

	modules := "[submodule \"vendor/module\"]\n\tpath = vendor/module\n\turl = https://example.test/module.git\n"
	if err := os.WriteFile(filepath.Join(worktree, ".gitmodules"), []byte(modules), 0o644); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "write submodule fixture")
	}
	if err := runGit(worktree, "add", ".gitmodules"); err != nil {
		return repositoryRefs{}, err
	}
	if err := runGit(worktree, "update-index", "--add", "--cacheinfo", "160000,"+baseSHA+",vendor/module"); err != nil {
		return repositoryRefs{}, err
	}
	if err := runGit(worktree, "commit", "-m", "Add submodule fixture"); err != nil {
		return repositoryRefs{}, err
	}
	tagSHA, err := gitOutput(worktree, "rev-parse", "HEAD")
	if err != nil {
		return repositoryRefs{}, err
	}
	if err := runGit(worktree, "tag", "v1.0.0"); err != nil {
		return repositoryRefs{}, err
	}

	if err := runGit(worktree, "checkout", "-b", "feature/content"); err != nil {
		return repositoryRefs{}, err
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "version.txt"), []byte("feature-version\n第二行\nthird\n"), 0o644); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "write feature fixture")
	}
	if err := runGit(worktree, "commit", "-am", "Change feature version"); err != nil {
		return repositoryRefs{}, err
	}
	featureSHA, err := gitOutput(worktree, "rev-parse", "HEAD")
	if err != nil {
		return repositoryRefs{}, err
	}

	if err := runGit(worktree, "checkout", "main"); err != nil {
		return repositoryRefs{}, err
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "version.txt"), []byte("main-version\n第二行\nthird\n"), 0o644); err != nil {
		return repositoryRefs{}, errors.Wrap(err, "write main fixture")
	}
	if err := runGit(worktree, "commit", "-am", "Change main version"); err != nil {
		return repositoryRefs{}, err
	}
	mainSHA, err := gitOutput(worktree, "rev-parse", "HEAD")
	if err != nil {
		return repositoryRefs{}, err
	}
	if err := runGit(worktree, "push", "origin", "main", "feature/content", "--tags"); err != nil {
		return repositoryRefs{}, err
	}

	return repositoryRefs{
		DefaultBranch: "main",
		FeatureBranch: "feature/content",
		Tag:           "v1.0.0",
		MainCommitSHA: mainSHA,
		FeatureSHA:    featureSHA,
		TagSHA:        tagSHA,
	}, nil
}

func runGit(directory string, arguments ...string) error {
	_, err := gitOutput(directory, arguments...)
	return err
}

func gitOutput(directory string, arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	if directory != "" {
		command.Dir = directory
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "", errors.Wrapf(err, "run git %s: %s", arguments[0], strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
