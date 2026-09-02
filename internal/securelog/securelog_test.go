package securelog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoggerRedactsSecretsFromMessagesAndAttributes(t *testing.T) {
	const token = "sensitive-personal-access-token"
	var output bytes.Buffer
	logger := New(&output, "debug", token, "token "+token)

	logger.Debug("Request used "+token, "authorization", "token "+token)

	assert.NotContains(t, output.String(), token)
	assert.NotContains(t, output.String(), "token "+token)
	assert.Contains(t, output.String(), "[REDACTED]")
}

func TestLoggerHonorsConfiguredLevel(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output, "warn")

	logger.Info("This should not be emitted.")
	logger.Warn("This should be emitted.")

	assert.NotContains(t, output.String(), "not be emitted")
	assert.Contains(t, output.String(), "should be emitted")
}

func TestRedactorKnowsOnlyItsOwnSecrets(t *testing.T) {
	var output bytes.Buffer
	destination := NewWriter(&output)
	first := NewRedactingLogger(destination, "info", "first-secret")
	second := NewRedactingLogger(destination, "info", "second-secret")

	first.Info("Request used first-secret and second-secret is untouched.")
	second.Info("Request used second-secret and first-secret is untouched.")

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "second-secret")
	assert.NotContains(t, lines[0], "first-secret")
	assert.Contains(t, lines[1], "first-secret")
	assert.NotContains(t, lines[1], "second-secret")
}

func TestConcurrentRedactorsWriteCompleteJSONLines(t *testing.T) {
	var output lockedBuffer
	destination := NewWriter(&output)
	logger := NewRedactingLogger(destination, "info", "shared-secret")

	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 25; j++ {
				logger.Info("Concurrent request used shared-secret.", "sequence", j)
			}
		}()
	}
	group.Wait()

	scanner := bufio.NewScanner(strings.NewReader(output.String()))
	lines := 0
	for scanner.Scan() {
		var record map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &record))
		lines++
	}
	require.NoError(t, scanner.Err())
	assert.Equal(t, 16*25, lines)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
