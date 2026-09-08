package mcpserver

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"gogs-mcp/internal/gogs"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultFileLineCount = 200
	maximumFileLineCount = 1000
	maximumReadableBytes = 1 << 20
	maximumFileTextBytes = 56 << 10
)

type listDirectoryInput struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Path    string `json:"path,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Page    int    `json:"page,omitempty"`
	PerPage int    `json:"per_page,omitempty"`
}

type getFileInput struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	Path      string `json:"path"`
	Ref       string `json:"ref,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	LineCount int    `json:"line_count,omitempty"`
}

func registerContentTools(server *mcp.Server, client Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_directory",
		Description: "List entries in a Gogs repository directory at a branch, tag, or commit.",
		Annotations: readOnlyAnnotations("List directory"),
		InputSchema: listDirectoryInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listDirectoryInput) (*mcp.CallToolResult, ToolResponse[DirectoryPage], error) {
		requestID := newRequestID()
		if err := gogs.ValidateRepositoryPath(input.Path, true); err != nil {
			result, response := contentError[DirectoryPage](requestID, err)
			return result, response, nil
		}
		ctx = gogs.WithRequestMetadata(ctx, requestID, "list_directory")
		entries, usedRef, total, err := client.ListDirectoryPage(ctx, input.Owner, input.Repo, input.Path, input.Ref, input.Page, input.PerPage)
		if err != nil {
			result, response := contentError[DirectoryPage](requestID, err)
			return result, response, nil
		}
		page := paginateDirectory(entries, input.Path, usedRef, 1, input.PerPage)
		page.Page, page.Total = input.Page, total
		var nextPage *int
		if input.Page <= (total-1)/input.PerPage && total > 0 {
			next := input.Page + 1
			nextPage = &next
		}
		return nil, ToolResponse[DirectoryPage]{
			Data: &page,
			Meta: ResponseMeta{RequestID: requestID, NextPage: nextPage},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_file",
		Description: "Read a bounded range from a text file or return safe metadata for non-text content.",
		Annotations: readOnlyAnnotations("Get file"),
		InputSchema: getFileInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getFileInput) (*mcp.CallToolResult, ToolResponse[File], error) {
		requestID := newRequestID()
		if err := gogs.ValidateRepositoryPath(input.Path, false); err != nil {
			result, response := contentError[File](requestID, err)
			return result, response, nil
		}
		ctx = gogs.WithRequestMetadata(ctx, requestID, "get_file")
		content, err := client.GetFile(ctx, input.Owner, input.Repo, input.Path, input.Ref)
		if err != nil {
			result, response := contentError[File](requestID, err)
			return result, response, nil
		}
		file, meta := boundedFile(content, input.StartLine, input.LineCount)
		meta.RequestID = requestID
		return nil, ToolResponse[File]{Data: &file, Meta: meta}, nil
	})
}

func paginateDirectory(entries []gogs.ContentEntry, repositoryPath, ref string, page, perPage int) DirectoryPage {
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(left, right gogs.ContentEntry) int {
		if order := cmp.Compare(left.Path, right.Path); order != 0 {
			return order
		}
		return cmp.Compare(left.SHA, right.SHA)
	})

	start := len(sorted)
	if page <= (len(sorted)+perPage-1)/perPage {
		start = (page - 1) * perPage
	}
	end := min(start+perPage, len(sorted))
	items := make([]DirectoryEntry, end-start)
	for index, entry := range sorted[start:end] {
		items[index] = DirectoryEntry{
			Name:         entry.Name,
			Path:         entry.Path,
			Type:         entry.Type,
			Size:         entry.Size,
			SHA:          entry.SHA,
			Target:       entry.Target,
			SubmoduleURL: entry.SubmoduleURL,
		}
	}

	return DirectoryPage{
		Entries: items,
		Path:    repositoryPath,
		Ref:     ref,
		Page:    page,
		PerPage: perPage,
		Total:   len(sorted),
	}
}

func boundedFile(content gogs.FileContent, startLine, lineCount int) (File, ResponseMeta) {
	file := File{
		Path:         content.Path,
		Ref:          content.Ref,
		Type:         content.Type,
		Size:         content.Size,
		SHA:          content.SHA,
		Target:       content.Target,
		SubmoduleURL: content.SubmoduleURL,
	}
	if content.Type != "file" {
		return file, ResponseMeta{}
	}
	if content.Size > maximumReadableBytes {
		file.Type = "too_large"
		return file, ResponseMeta{
			Truncated: true,
			Warnings:  []string{"File content exceeds the 1 MiB read limit and was not returned."},
		}
	}
	if isBinary(content.Data) {
		file.Type = "binary"
		return file, ResponseMeta{}
	}

	file.Type = "text"
	// splitTextLines drops the empty element after a trailing newline, so
	// that fact is reported separately to keep the original bytes recoverable.
	file.TrailingNewline = bytes.HasSuffix(content.Data, []byte("\n"))
	lines := splitTextLines(content.Data)
	file.TotalLines = len(lines)
	if startLine > len(lines) {
		return file, ResponseMeta{}
	}
	startIndex := startLine - 1
	requestedEnd := min(startIndex+lineCount, len(lines))
	selected := make([]string, 0, requestedEnd-startIndex)
	selectedBytes := 0
	meta := ResponseMeta{}
	for index := startIndex; index < requestedEnd; index++ {
		separatorBytes := 0
		if len(selected) > 0 {
			separatorBytes = 2
		}
		lineBytes := encodedStringLength(lines[index])
		if selectedBytes+separatorBytes+lineBytes > maximumFileTextBytes {
			meta.Truncated = true
			meta.Warnings = append(meta.Warnings, "File content reached the 64 KiB structured output limit.")
			if len(selected) == 0 {
				selected = append(selected, boundedJSONPrefix(lines[index], maximumFileTextBytes))
				file.StartLine = index + 1
				file.EndLine = index + 1
				if index+1 < len(lines) {
					next := index + 2
					meta.NextStartLine = &next
				}
			} else {
				next := index + 1
				meta.NextStartLine = &next
			}
			break
		}
		selected = append(selected, lines[index])
		selectedBytes += separatorBytes + lineBytes
		if file.StartLine == 0 {
			file.StartLine = index + 1
		}
		file.EndLine = index + 1
	}
	file.Content = strings.Join(selected, "\n")
	if !meta.Truncated && requestedEnd < len(lines) {
		meta.Truncated = true
		next := requestedEnd + 1
		meta.NextStartLine = &next
		meta.Warnings = append(meta.Warnings, "File content continues after the returned line range.")
	}
	return file, meta
}

func isBinary(content []byte) bool {
	prefix := content[:min(len(content), 8<<10)]
	return bytes.IndexByte(prefix, 0) >= 0 || !utf8.Valid(content)
}

func splitTextLines(content []byte) []string {
	if len(content) == 0 {
		return []string{}
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func boundedJSONPrefix(value string, maximum int) string {
	if encodedStringLength(value) <= maximum {
		return value
	}
	used := 0
	end := 0
	for index, character := range value {
		characterBytes := encodedStringLength(string(character))
		if used+characterBytes > maximum {
			break
		}
		used += characterBytes
		end = index + utf8.RuneLen(character)
	}
	return value[:end]
}

func encodedStringLength(value string) int {
	encoded, _ := json.Marshal(value)
	return len(encoded) - 2
}

func contentError[T any](requestID string, err error) (*mcp.CallToolResult, ToolResponse[T]) {
	classified := gogs.AsError(err)
	return &mcp.CallToolResult{IsError: true}, ToolResponse[T]{
		Error: mapToolError(classified),
		Meta:  ResponseMeta{RequestID: requestID},
	}
}

func listDirectoryInputSchema() *jsonschema.Schema {
	properties := repositoryReferenceProperties()
	properties["path"] = stringSchema("Repository-relative directory path.", false, true)
	properties["page"] = integerSchema("Page number starting at 1.", defaultPage, 0)
	properties["per_page"] = integerSchema("Directory entries per page, from 1 through 100.", defaultPerPage, maximumPerPage)
	return objectSchema(properties, []string{"owner", "repo"})
}

func getFileInputSchema() *jsonschema.Schema {
	properties := repositoryReferenceProperties()
	properties["path"] = stringSchema("Repository-relative file path.", true, false)
	properties["start_line"] = integerSchema("First line to return, starting at 1.", 1, 0)
	properties["line_count"] = integerSchema("Maximum lines to return, from 1 through 1000.", defaultFileLineCount, maximumFileLineCount)
	return objectSchema(properties, []string{"owner", "repo", "path"})
}

func repositoryReferenceProperties() map[string]*jsonschema.Schema {
	return map[string]*jsonschema.Schema{
		"owner": stringSchema("Repository owner username.", true, false),
		"repo":  stringSchema("Repository name.", true, false),
		"ref":   stringSchema("Branch, tag, or commit SHA. Uses the default branch when omitted.", false, true),
	}
}

func stringSchema(description string, required, defaultEmpty bool) *jsonschema.Schema {
	schema := &jsonschema.Schema{Type: "string", Description: description}
	if required {
		schema.MinLength = intPointer(1)
	}
	if defaultEmpty {
		schema.Default = json.RawMessage(`""`)
	}
	return schema
}
