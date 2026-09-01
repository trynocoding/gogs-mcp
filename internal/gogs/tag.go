package gogs

import "context"

// Tag is the stable tag representation exposed to tools.
type Tag struct {
	Name      string `json:"name"`
	CommitSHA string `json:"commit_sha"`
}

type tagResponse struct {
	Name   string `json:"name"`
	Commit *struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// ListTags returns every tag of the repository with its peeled commit SHA.
// Gogs v0.14.2 has no single-tag endpoint, so callers match on the name.
func (c *Client) ListTags(ctx context.Context, owner, repo string) ([]Tag, error) {
	var response []tagResponse
	if err := c.getJSON(ctx, &response, "repos", owner, repo, "tags"); err != nil {
		return nil, err
	}
	tags := make([]Tag, len(response))
	for index, entry := range response {
		tags[index] = Tag{Name: entry.Name}
		if entry.Commit != nil {
			tags[index].CommitSHA = entry.Commit.SHA
		}
	}
	return tags, nil
}
