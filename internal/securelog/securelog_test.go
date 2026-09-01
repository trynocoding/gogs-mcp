package securelog

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
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
