package gogs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildPullDiffHandlesSubmoduleChange guards against the panic that a
// submodule change triggers otherwise: go-git reports file patches whose
// Files() are both nil for gitlink entries, because they carry no readable
// file content on either side, so the path and status must come from the
// tree change itself. The fixture deletes the gitlink on the pull request
// branch, which is how go-git surfaces a submodule removal.
func TestBuildPullDiffHandlesSubmoduleChange(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=E2E", "GIT_AUTHOR_EMAIL=e2e@example.test",
			"GIT_COMMITTER_NAME=E2E", "GIT_COMMITTER_EMAIL=e2e@example.test",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")
	run("config", "user.name", "E2E")
	run("config", "user.email", "e2e@example.test")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "version.txt"), []byte("tag-version\n第二行\nthird\n"), 0o644))
	require.NoError(t, os.Symlink("version.txt", filepath.Join(dir, "src", "version-link")))
	run("add", "--all")
	run("commit", "-m", "C1")

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte("[submodule \"vendor/module\"]\n\tpath = vendor/module\n\turl = https://example.test/module.git\n"), 0o644))
	run("add", ".gitmodules")
	run("update-index", "--add", "--cacheinfo", "160000,"+run("rev-parse", "HEAD")+",vendor/module")
	run("commit", "-m", "C2")

	run("checkout", "-b", "pr")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "version.txt"), []byte("feature-version\n第二行\nthird\n"), 0o644))
	run("commit", "-am", "C3")
	head := run("rev-parse", "HEAD")

	run("checkout", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "version.txt"), []byte("main-version\n第二行\nthird\n"), 0o644))
	run("commit", "-am", "C4")
	forked := run("rev-parse", "HEAD")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	headCommit, err := repo.CommitObject(plumbing.NewHash(head))
	require.NoError(t, err)
	baseCommit, err := repo.CommitObject(plumbing.NewHash(forked))
	require.NoError(t, err)
	bases, err := headCommit.MergeBase(baseCommit)
	require.NoError(t, err)
	require.Len(t, bases, 1)

	mergeBaseTree, err := bases[0].Tree()
	require.NoError(t, err)
	headTree, err := headCommit.Tree()
	require.NoError(t, err)
	changes, err := object.DiffTreeWithOptions(context.Background(), mergeBaseTree, headTree, &object.DiffTreeOptions{
		DetectRenames: true,
		RenameScore:   50,
	})
	require.NoError(t, err)
	patch, err := changes.Patch()
	require.NoError(t, err)

	diff, err := buildPullDiff(changes, patch, nil, bases[0].Hash.String(), 0)
	require.NoError(t, err)

	require.Len(t, diff.Files, 2)
	assert.Equal(t, "src/version.txt", diff.Files[0].Path)
	assert.Equal(t, "modified", diff.Files[0].Status)
	assert.Equal(t, 1, diff.Files[0].Additions)
	assert.Equal(t, 1, diff.Files[0].Deletions)
	assert.False(t, diff.Files[0].IsBinary)
	assert.Equal(t, "vendor/module", diff.Files[1].Path)
	assert.Equal(t, "deleted", diff.Files[1].Status)
	assert.Equal(t, 0, diff.Files[1].Additions)
	assert.Equal(t, 0, diff.Files[1].Deletions)
	assert.True(t, diff.Files[1].IsBinary)
	assert.Contains(t, diff.Diff, "diff --git a/src/version.txt b/src/version.txt")
}
