//go:build e2e

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOfflinePackage(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)

	commandContext, cancelCommands := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancelCommands()
	_, err := runCommand(commandContext, "create the smoke network", "docker", "network", "create", environment.network)
	require.NoError(t, err)

	dataDir := filepath.Join(environment.tempDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "log"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "app.ini"),
		[]byte(gogsConfig(identifier)),
		0o600,
	))

	t.Log("Creating an isolated SQLite user and personal access token.")
	bootstrapOutput, err := runSensitiveCommand(
		commandContext,
		"bootstrap the Gogs smoke database",
		"run",
		"--rm",
		"--name", environment.bootstrapContainer,
		"--network", environment.network,
		"--volume", dataDir+":/data:Z",
		"--entrypoint", "/app/gogs-e2e-bootstrap",
		environment.image,
		"--config", "/data/app.ini",
		"--username", "package-user",
		"--password", "package-password-"+identifier,
		"--email", "package@example.test",
		"--full-name", "Package User",
		"--token-name", "package-"+identifier,
	)
	require.NoError(t, err)
	bootstrap := parseBootstrapResult(t, bootstrapOutput)

	t.Log("Starting the isolated Gogs container.")
	_, err = runCommand(
		commandContext,
		"start the Gogs smoke container",
		"docker",
		"run",
		"--detach",
		"--name", environment.container,
		"--network", environment.network,
		"--publish", "127.0.0.1::3000",
		"--volume", dataDir+":/data:Z",
		environment.image,
	)
	require.NoError(t, err)
	baseURL := waitForGogs(t, environment.container)

	t.Log("Assembling the offline bundle with task package.")
	require.NoError(t, os.RemoveAll(filepath.Join(projectRoot, "dist")))
	runTask(t, projectRoot, "package")
	archive := uniquePackageArchive(t, projectRoot)

	t.Log("Extracting the bundle and verifying it inside a network-less namespace.")
	bundleDir := filepath.Join(environment.tempDir, "bundle")
	require.NoError(t, os.MkdirAll(bundleDir, 0o700))
	extractTarGz(t, archive, bundleDir)
	entries, err := os.ReadDir(bundleDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the bundle archive holds exactly one package directory")
	packageDir := filepath.Join(bundleDir, entries[0].Name())

	prefix := filepath.Join(environment.tempDir, "prefix")
	home := filepath.Join(environment.tempDir, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))
	// Replace HOME instead of appending it: the scripts isolate configuration
	// and cache paths, and a duplicated HOME entry would keep the original.
	packageEnv := []string{}
	for _, entry := range filteredEnvironment(os.Environ()) {
		if strings.HasPrefix(entry, "HOME=") {
			continue
		}
		packageEnv = append(packageEnv, entry)
	}
	packageEnv = append(packageEnv, "HOME="+home)
	install := filepath.Join(packageDir, "scripts", "install.sh")
	verify := filepath.Join(packageDir, "scripts", "verify-offline.sh")
	uninstall := filepath.Join(packageDir, "scripts", "uninstall.sh")

	// The install, verify, and uninstall scripts must succeed with every
	// interface disabled, which proves they never touch the network.
	offlineScript := "set -eu\n" +
		"'" + install + "' --prefix '" + prefix + "'\n" +
		"'" + verify + "' '" + packageDir + "'\n" +
		"'" + uninstall + "' --prefix '" + prefix + "'\n"
	runFencedScript(t, packageEnv, offlineScript)

	t.Log("Reinstalling outside the namespace fence for the connection test.")
	runPackageScript(t, packageEnv, install, "--prefix", prefix)

	t.Log("Checking that the installed files and the product manifest agree.")
	installedList, err := os.ReadFile(filepath.Join(prefix, "share", "gogs-mcp", "installed-files.list"))
	require.NoError(t, err)
	installed := strings.Split(strings.TrimSpace(string(installedList)), "\n")
	require.NotEmpty(t, installed)
	for _, file := range installed {
		require.FileExists(t, file, "every recorded installation file exists")
	}
	require.FileExists(t, filepath.Join(prefix, "bin", "gogs-mcp"))
	require.FileExists(t, filepath.Join(prefix, "share", "doc", "gogs-mcp", "docs", "install.md"))

	t.Log("Calling get_authenticated_user through the installed binary.")
	useMCPClientAt(t, filepath.Join(prefix, "bin", "gogs-mcp"), baseURL, bootstrap.Token, "", nil, e2eToolNames, func(session *mcp.ClientSession) {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "get_authenticated_user",
			Arguments: map[string]any{},
		})
		require.NoError(t, err)
		require.False(t, result.IsError)
		var response toolResponse
		decodeStructuredContent(t, result.StructuredContent, &response)
		require.NotNil(t, response.Data)
		assert.Equal(t, "package-user", response.Data.Username)
	})

	t.Log("Uninstalling while keeping configuration, tokens, and unrelated files.")
	tokenDir := filepath.Join(home, ".config", "gogs-mcp")
	require.NoError(t, os.MkdirAll(tokenDir, 0o700))
	tokenFile := filepath.Join(tokenDir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(bootstrap.Token+"\n"), 0o600))
	unrelated := filepath.Join(prefix, "unrelated.txt")
	require.NoError(t, os.WriteFile(unrelated, []byte("not part of the product\n"), 0o644))

	runPackageScript(t, packageEnv, filepath.Join(packageDir, "scripts", "uninstall.sh"), "--prefix", prefix)
	require.NoFileExists(t, filepath.Join(prefix, "bin", "gogs-mcp"))
	require.FileExists(t, tokenFile, "the default uninstall keeps the personal access token")
	require.FileExists(t, unrelated, "the uninstall never deletes files outside the product manifest")
	require.NoFileExists(t, filepath.Join(prefix, "share", "gogs-mcp", "installed-files.list"))
	require.NoDirExists(t, filepath.Join(prefix, "share", "doc", "gogs-mcp"))

	t.Log("Reinstalling and purging all user data explicitly.")
	runPackageScript(t, packageEnv, install, "--prefix", prefix)
	require.FileExists(t, filepath.Join(prefix, "bin", "gogs-mcp"))
	runPackageScript(t, packageEnv, filepath.Join(packageDir, "scripts", "uninstall.sh"), "--prefix", prefix, "--purge")
	require.NoFileExists(t, filepath.Join(prefix, "bin", "gogs-mcp"))
	require.NoFileExists(t, tokenFile, "--purge removes the configuration and the token")
	require.NoDirExists(t, tokenDir)
	require.NoDirExists(t, filepath.Join(home, ".cache", "gogs-mcp"))
	require.FileExists(t, unrelated, "--purge still never deletes files outside the product manifest")
}

// runTask runs a Taskfile target from the project root.
func runTask(t *testing.T, projectRoot, target string) {
	t.Helper()
	taskBinary, err := exec.LookPath("task")
	if err != nil {
		taskBinary = "/root/.local/bin/task"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, taskBinary, target)
	command.Dir = projectRoot
	output, err := command.CombinedOutput()
	require.NoError(t, err, "task %s failed:\n%s", target, output)
}

// uniquePackageArchive returns the single bundle archive of the dist
// directory after task package removed the previous ones.
func uniquePackageArchive(t *testing.T, projectRoot string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(projectRoot, "dist", "gogs-mcp-*-fedora43-amd64-offline.tar.gz"))
	require.NoError(t, err)
	require.Len(t, matches, 1, "task package leaves exactly one bundle archive")
	return matches[0]
}

// extractTarGz unpacks a gzipped archive into destination.
func extractTarGz(t *testing.T, archivePath, destination string) {
	t.Helper()
	archive, err := os.Open(archivePath)
	require.NoError(t, err)
	defer func() { _ = archive.Close() }()
	decompressed, err := gzip.NewReader(archive)
	require.NoError(t, err)
	defer func() { _ = decompressed.Close() }()
	reader := tar.NewReader(decompressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		require.NoError(t, err)
		target := filepath.Join(destination, header.Name)
		if !strings.HasPrefix(target, destination+string(filepath.Separator)) && target != destination {
			require.FailNow(t, "the bundle archive contains an unsafe path", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			require.NoError(t, os.MkdirAll(target, 0o755))
		case tar.TypeReg:
			require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, header.FileInfo().Mode().Perm())
			require.NoError(t, err)
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			require.NoError(t, copyErr)
			require.NoError(t, closeErr)
		default:
			require.FailNow(t, "unsupported bundle entry", header.Name)
		}
	}
}

// runFencedScript runs a shell script with all networking removed through a
// new network namespace, or directly with a note when the environment
// forbids new namespaces.
func runFencedScript(t *testing.T, environment []string, script string) {
	t.Helper()
	if networkFencingAvailable(t) {
		runShell(t, environment, "unshare", "-n", "sh", "-c", script)
		return
	}
	t.Log("Network namespaces are unavailable; running the scripts unfenced.")
	runShell(t, environment, "sh", "-c", script)
}

func networkFencingAvailable(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "unshare", "-n", "true").Run() == nil
}

// runPackageScript runs one of the package scripts from an unrelated working
// directory, which the scripts must tolerate.
func runPackageScript(t *testing.T, environment []string, script string, arguments ...string) {
	t.Helper()
	runShell(t, environment, "sh", append([]string{"-c", "'" + script + "' \"$@\"", "install-test"}, arguments...)...)
}

func runShell(t *testing.T, environment []string, name string, arguments ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, name, arguments...)
	command.Env = environment
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s %s failed:\n%s", name, strings.Join(arguments, " "), output)
}
