//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gogsCommit = "5dcb6c64bdf61e38dbdbb941c1d69789c560d0fb"
	gogsTag    = "v0.14.2"
)

type bootstrapResult struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

type authenticatedUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

type toolError struct {
	Code string `json:"code"`
}

type toolResponse struct {
	Data  *authenticatedUser `json:"data,omitempty"`
	Error *toolError         `json:"error,omitempty"`
}

type smokeEnvironment struct {
	tempDir            string
	image              string
	network            string
	container          string
	bootstrapContainer string
}

func TestSmoke(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)

	commandContext, cancelCommands := context.WithTimeout(context.Background(), 2*time.Minute)
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
		"--username", "smoke-user",
		"--password", "smoke-password-"+identifier,
		"--email", "smoke@example.test",
		"--full-name", "Smoke User",
		"--token-name", "smoke-"+identifier,
	)
	require.NoError(t, err)
	bootstrap := parseBootstrapResult(t, bootstrapOutput)
	require.Positive(t, bootstrap.UserID)
	require.Equal(t, "smoke-user", bootstrap.Username)
	require.NotEmpty(t, bootstrap.Token)

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

	t.Log("Calling get_authenticated_user through a real stdio MCP process.")
	successResult, successLogs := callAuthenticatedUser(t, projectRoot, baseURL, bootstrap.Token)
	require.False(t, successResult.IsError)
	var successResponse toolResponse
	decodeStructuredContent(t, successResult.StructuredContent, &successResponse)
	require.NotNil(t, successResponse.Data)
	assert.Equal(t, bootstrap.UserID, successResponse.Data.ID)
	assert.Equal(t, "smoke-user", successResponse.Data.Username)
	assert.Equal(t, "Smoke User", successResponse.Data.FullName)
	assert.Equal(t, "smoke@example.test", successResponse.Data.Email)
	assertNoSecrets(t, successLogs, bootstrap.Token)

	t.Log("Calling the same real Gogs instance with an invalid personal access token.")
	invalidToken := bootstrap.Token + "-invalid"
	failureResult, failureLogs := callAuthenticatedUser(t, projectRoot, baseURL, invalidToken)
	require.True(t, failureResult.IsError)
	var failureResponse toolResponse
	decodeStructuredContent(t, failureResult.StructuredContent, &failureResponse)
	require.NotNil(t, failureResponse.Error)
	assert.Equal(t, "AUTHENTICATION_FAILED", failureResponse.Error.Code)
	assertNoSecrets(t, failureLogs, bootstrap.Token, invalidToken)

	gogsLogs, err := runCommand(commandContext, "read Gogs smoke logs", "docker", "logs", environment.container)
	require.NoError(t, err)
	assertNoSecrets(t, gogsLogs, bootstrap.Token, invalidToken)

	t.Log("Removing all smoke containers, networks, images, and temporary data.")
	environment.cleanup(t)
	assertResourcesRemoved(t, environment)
}

func prepareSmokeEnvironment(t *testing.T) (*smokeEnvironment, string, string) {
	t.Helper()
	requireDocker(t)
	projectRoot := projectRoot(t)
	sourceDir := os.Getenv("GOGS_E2E_SOURCE_DIR")
	if sourceDir == "" {
		sourceDir = filepath.Clean(filepath.Join(projectRoot, "..", "gogs"))
	}
	verifyGogsSource(t, sourceDir)

	identifier := randomIdentifier(t)
	tempDir, err := os.MkdirTemp("", "gogs-mcp-smoke-"+identifier+"-")
	require.NoError(t, err)
	environment := &smokeEnvironment{
		tempDir:            tempDir,
		image:              "gogs-mcp-smoke:" + identifier,
		network:            "gogs-mcp-smoke-" + identifier,
		container:          "gogs-mcp-smoke-" + identifier + "-gogs",
		bootstrapContainer: "gogs-mcp-smoke-" + identifier + "-bootstrap",
	}
	t.Cleanup(func() {
		environment.cleanup(t)
	})

	t.Log("Preparing the pinned Gogs source context.")
	buildContext := filepath.Join(tempDir, "build")
	require.NoError(t, os.MkdirAll(buildContext, 0o700))
	require.NoError(t, exportGitArchive(sourceDir, buildContext))
	require.NoError(t, addSmokeBuildFiles(projectRoot, buildContext))

	t.Log("Building the Gogs v0.14.2 smoke image.")
	buildContextTimeout, cancelBuild := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancelBuild()
	err = runStreamingCommand(
		buildContextTimeout,
		"build the Gogs smoke image",
		"docker",
		"build",
		"--build-arg", "GOGS_COMMIT="+gogsCommit,
		"--file", filepath.Join(buildContext, "Dockerfile.smoke"),
		"--tag", environment.image,
		buildContext,
	)
	require.NoError(t, err)
	assertImageRevision(t, environment.image)
	assertImageVersion(t, environment.image)
	return environment, projectRoot, identifier
}

func requireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := runCommand(ctx, "inspect the Docker daemon", "docker", "info", "--format", "{{.ServerVersion}}")
	require.NoError(t, err)
}

func projectRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func verifyGogsSource(t *testing.T, sourceDir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	revision, err := runCommand(
		ctx,
		"resolve the pinned Gogs revision",
		"git",
		"-C", sourceDir,
		"rev-parse", gogsCommit+"^{commit}",
	)
	require.NoError(t, err)
	assert.Equal(t, gogsCommit, strings.TrimSpace(string(revision)))

	tag, err := runCommand(
		ctx,
		"resolve the pinned Gogs tag",
		"git",
		"-C", sourceDir,
		"describe", "--tags", "--exact-match", gogsCommit,
	)
	require.NoError(t, err)
	assert.Equal(t, gogsTag, strings.TrimSpace(string(tag)))
}

func exportGitArchive(sourceDir, destination string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	archive, err := runCommand(
		ctx,
		"export the pinned Gogs source",
		"git",
		"-C", sourceDir,
		"archive", "--format=tar", gogsCommit,
	)
	if err != nil {
		return err
	}
	return extractArchive(bytes.NewReader(archive), destination)
}

func extractArchive(reader io.Reader, destination string) error {
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.Wrap(err, "read Gogs source archive")
		}

		target, err := archiveTarget(destination, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(target, header.FileInfo().Mode().Perm()); err != nil {
				return errors.Wrap(err, "create archived directory")
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return errors.Wrap(err, "create archived file parent")
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, header.FileInfo().Mode().Perm())
			if err != nil {
				return errors.Wrap(err, "create archived file")
			}
			_, copyErr := io.Copy(file, archive)
			closeErr := file.Close()
			if copyErr != nil {
				return errors.Wrap(copyErr, "extract archived file")
			}
			if closeErr != nil {
				return errors.Wrap(closeErr, "close archived file")
			}
		case tar.TypeSymlink:
			if err := validateSymlinkTarget(header.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return errors.Wrap(err, "create archived symlink parent")
			}
			if err := os.Symlink(filepath.FromSlash(header.Linkname), target); err != nil {
				return errors.Wrap(err, "create archived symlink")
			}
		default:
			return errors.Newf("unsupported entry %q in Gogs source archive", header.Name)
		}
	}
}

func archiveTarget(root, name string) (string, error) {
	if strings.ContainsRune(name, '\x00') || path.IsAbs(name) {
		return "", errors.New("Gogs source archive contains an unsafe path")
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("Gogs source archive escapes the build context")
	}
	return filepath.Join(root, filepath.FromSlash(cleaned)), nil
}

func validateSymlinkTarget(target string) error {
	if strings.ContainsRune(target, '\x00') || path.IsAbs(target) {
		return errors.New("Gogs source archive contains an unsafe symlink")
	}
	cleaned := path.Clean(target)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("Gogs source archive symlink escapes the build context")
	}
	return nil
}

func addSmokeBuildFiles(projectRoot, buildContext string) error {
	testdata := filepath.Join(projectRoot, "tests", "e2e", "testdata")
	if err := copyFile(
		filepath.Join(testdata, "gogs.Dockerfile"),
		filepath.Join(buildContext, "Dockerfile.smoke"),
	); err != nil {
		return err
	}
	bootstrapDirectory := filepath.Join(buildContext, "internal", "e2ebootstrap")
	if err := os.MkdirAll(bootstrapDirectory, 0o755); err != nil {
		return errors.Wrap(err, "create bootstrap source directory")
	}
	if err := copyFile(
		filepath.Join(testdata, "bootstrap", "main.go"),
		filepath.Join(bootstrapDirectory, "main.go"),
	); err != nil {
		return err
	}
	repositoriesDirectory := filepath.Join(buildContext, "internal", "e2erepositories")
	if err := os.MkdirAll(repositoriesDirectory, 0o755); err != nil {
		return errors.Wrap(err, "create repository bootstrap source directory")
	}
	return copyFile(
		filepath.Join(testdata, "repositories-bootstrap", "main.go"),
		filepath.Join(repositoriesDirectory, "main.go"),
	)
}

func copyFile(source, destination string) error {
	contents, err := os.ReadFile(source)
	if err != nil {
		return errors.Wrap(err, "read smoke build file")
	}
	if err := os.WriteFile(destination, contents, 0o644); err != nil {
		return errors.Wrap(err, "write smoke build file")
	}
	return nil
}

func assertImageRevision(t *testing.T, image string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	revision, err := runCommand(
		ctx,
		"inspect the Gogs smoke image revision",
		"docker",
		"image", "inspect",
		"--format", "{{ index .Config.Labels \"org.opencontainers.image.revision\" }}",
		image,
	)
	require.NoError(t, err)
	assert.Equal(t, gogsCommit, strings.TrimSpace(string(revision)))
}

func assertImageVersion(t *testing.T, image string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	version, err := runCommand(
		ctx,
		"read the Gogs smoke image version",
		"docker",
		"run", "--rm", "--entrypoint", "/app/gogs", image, "--version",
	)
	require.NoError(t, err)
	assert.Contains(t, string(version), "0.14.2")
}

func gogsConfig(identifier string) string {
	return fmt.Sprintf(`BRAND_NAME = Gogs MCP smoke
RUN_USER = root
RUN_MODE = prod

[server]
EXTERNAL_URL = http://localhost:3000/
DOMAIN = localhost
PROTOCOL = http
HTTP_ADDR = 0.0.0.0
HTTP_PORT = 3000
APP_DATA_PATH = /data/app
DISABLE_ROUTER_LOG = false
DISABLE_SSH = true

[repository]
ROOT = /data/repositories
DEFAULT_BRANCH = main

[database]
TYPE = sqlite3
PATH = /data/gogs.db
MAX_OPEN_CONNS = 1
MAX_IDLE_CONNS = 1

[security]
INSTALL_LOCK = true
SECRET_KEY = smoke-secret-%s

[auth]
DISABLE_REGISTRATION = true

[log]
MODE = console
LEVEL = Info
ROOT_PATH = /data/log
`, identifier)
}

func parseBootstrapResult(t *testing.T, output []byte) bootstrapResult {
	t.Helper()
	lines := bytes.Split(output, []byte("\n"))
	for index := len(lines) - 1; index >= 0; index-- {
		line := bytes.TrimSpace(lines[index])
		if len(line) == 0 {
			continue
		}
		var result bootstrapResult
		if err := json.Unmarshal(line, &result); err == nil && result.Token != "" {
			return result
		}
	}
	require.FailNow(t, "The Gogs bootstrap command did not return a personal access token.")
	return bootstrapResult{}
}

func waitForGogs(t *testing.T, container string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	portOutput, err := runCommand(ctx, "resolve the Gogs smoke port", "docker", "port", container, "3000/tcp")
	require.NoError(t, err)
	binding := strings.TrimSpace(string(portOutput))
	require.NotEmpty(t, binding)
	baseURL := "http://" + binding

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/healthcheck", nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		if err == nil {
			closeErr := response.Body.Close()
			if closeErr == nil && response.StatusCode == http.StatusOK {
				return baseURL
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	logContext, cancelLogs := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelLogs()
	logs, _ := runCommand(logContext, "read failed Gogs startup logs", "docker", "logs", container)
	require.FailNow(t, "The Gogs smoke container did not become ready.", string(logs))
	return ""
}

func callAuthenticatedUser(t *testing.T, projectRoot, baseURL, token string) (*mcp.CallToolResult, []byte) {
	t.Helper()
	var result *mcp.CallToolResult
	logs := useMCPClient(t, projectRoot, baseURL, token, func(session *mcp.ClientSession) {
		var err error
		result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "get_authenticated_user",
			Arguments: map[string]any{},
		})
		require.NoError(t, err)
	})
	return result, logs
}

func useMCPClient(t *testing.T, projectRoot, baseURL, token string, action func(*mcp.ClientSession)) []byte {
	t.Helper()
	var stderr bytes.Buffer
	defer func() {
		assertNoSecrets(t, stderr.Bytes(), token)
	}()

	command := exec.Command(filepath.Join(projectRoot, ".bin", "gogs-mcp"), "serve")
	command.Env = append(filteredEnvironment(os.Environ()),
		"GOGS_BASE_URL="+baseURL,
		"GOGS_TOKEN="+token,
		"GOGS_ALLOW_INSECURE_HTTP=true",
		"NO_PROXY=127.0.0.1,localhost",
	)
	command.Stderr = &stderr

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "gogs-mcp-e2e",
		Version: "test",
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{
		Command:           command,
		TerminateDuration: 5 * time.Second,
	}, nil)
	require.NoError(t, err)
	closed := false
	defer func() {
		if !closed {
			_ = session.Close()
		}
	}()

	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	names := make([]string, len(tools.Tools))
	for index, tool := range tools.Tools {
		names[index] = tool.Name
	}
	assert.ElementsMatch(t, []string{
		"get_authenticated_user",
		"list_repositories",
		"search_repositories",
		"get_repository",
		"list_directory",
		"get_file",
		"list_branches",
		"get_branch",
		"list_commits",
		"get_commit",
	}, names)
	action(session)
	require.NoError(t, session.Close())
	closed = true
	return bytes.Clone(stderr.Bytes())
}

func filteredEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "GOGS_") || strings.EqualFold(name, "NO_PROXY") {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func decodeStructuredContent(t *testing.T, source, destination any) {
	t.Helper()
	encoded, err := json.Marshal(source)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, destination))
}

func assertNoSecrets(t *testing.T, output []byte, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(output, []byte(secret)) {
			t.Error("A smoke test log contained a personal access token.")
		}
	}
}

func randomIdentifier(t *testing.T) string {
	t.Helper()
	random := make([]byte, 6)
	_, err := rand.Read(random)
	require.NoError(t, err)
	return hex.EncodeToString(random)
}

func runCommand(ctx context.Context, operation, command string, arguments ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, arguments...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.Wrapf(err, "%s: %s", operation, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func runSensitiveCommand(ctx context.Context, operation string, arguments ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", arguments...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.Wrap(err, operation)
	}
	return output, nil
}

func runStreamingCommand(ctx context.Context, operation, command string, arguments ...string) error {
	cmd := exec.CommandContext(ctx, command, arguments...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, operation)
	}
	return nil
}

func (environment *smokeEnvironment) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, containerErr := runCommand(ctx, "remove the smoke Gogs container", "docker", "rm", "--force", environment.container)
	if containerErr != nil && resourceExists(ctx, "container", environment.container) {
		t.Error("Could not remove the smoke Gogs container.")
	}
	_, bootstrapErr := runCommand(ctx, "remove the smoke bootstrap container", "docker", "rm", "--force", environment.bootstrapContainer)
	if bootstrapErr != nil && resourceExists(ctx, "container", environment.bootstrapContainer) {
		t.Error("Could not remove the smoke bootstrap container.")
	}
	_, networkErr := runCommand(ctx, "remove the smoke network", "docker", "network", "rm", environment.network)
	if networkErr != nil && resourceExists(ctx, "network", environment.network) {
		t.Error("Could not remove the smoke Docker network.")
	}
	_, imageErr := runCommand(ctx, "remove the smoke image", "docker", "image", "rm", "--force", environment.image)
	if imageErr != nil && resourceExists(ctx, "image", environment.image) {
		t.Error("Could not remove the smoke Docker image.")
	}
	if err := os.RemoveAll(environment.tempDir); err != nil {
		t.Error("Could not remove the smoke temporary directory.")
	}
}

func assertResourcesRemoved(t *testing.T, environment *smokeEnvironment) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assert.False(t, resourceExists(ctx, "container", environment.container))
	assert.False(t, resourceExists(ctx, "container", environment.bootstrapContainer))
	assert.False(t, resourceExists(ctx, "network", environment.network))
	assert.False(t, resourceExists(ctx, "image", environment.image))
	_, err := os.Stat(environment.tempDir)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func resourceExists(ctx context.Context, resourceType, name string) bool {
	var arguments []string
	switch resourceType {
	case "container":
		arguments = []string{"container", "inspect", name}
	case "network":
		arguments = []string{"network", "inspect", name}
	case "image":
		arguments = []string{"image", "inspect", name}
	default:
		return false
	}
	command := exec.CommandContext(ctx, "docker", arguments...)
	return command.Run() == nil
}
