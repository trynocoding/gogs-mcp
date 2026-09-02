package gogs

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListPullRequestsRejectsUnknownStateBeforeAnyRequest(t *testing.T) {
	// An empty client must never be touched: the state check runs first.
	client := &Client{}
	_, _, err := client.ListPullRequests(context.Background(), "owner", "calculator", "merged", 30)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidArgument, AsError(err).Code)
}

func TestGitCloneURLStripsAPIRoot(t *testing.T) {
	testCases := map[string]string{
		"http://127.0.0.1:3000/api/v1":    "http://127.0.0.1:3000/owner/calculator.git",
		"http://127.0.0.1:3000/api/v1/":   "http://127.0.0.1:3000/owner/calculator.git",
		"http://127.0.0.1:3000/api/v1//":  "http://127.0.0.1:3000/owner/calculator.git",
		"https://gogs.example.com/api/v1": "https://gogs.example.com/owner/calculator.git",
	}
	for apiRoot, expected := range testCases {
		parsed, err := url.Parse(apiRoot)
		require.NoError(t, err)
		client := &Client{apiRoot: parsed}
		assert.Equal(t, expected, client.gitCloneURL("owner", "calculator").String(), apiRoot)
	}
}
