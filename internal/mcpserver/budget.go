package mcpserver

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"gogs-mcp/internal/gogs"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultBodyBytes = 8192

func bodyRangeProperties(properties map[string]*jsonschema.Schema) {
	properties["body_offset"] = &jsonschema.Schema{Type: "integer", Minimum: floatPointer(0), Description: "UTF-8 byte offset returned by next_body_offset; defaults to zero."}
	properties["body_max_bytes"] = integerSchema("Maximum body bytes in this chunk, from 4 through 8192; defaults to 8192.", defaultBodyBytes, defaultBodyBytes)
	properties["body_max_bytes"].Minimum = floatPointer(4)
}

func bodyChunk(body string, offset, maximum int) (string, *int) {
	offset = min(max(0, offset), len(body))
	for offset < len(body) && !utf8.RuneStart(body[offset]) {
		offset++
	}
	if maximum <= 0 {
		maximum = defaultBodyBytes
	}
	maximum = min(maximum, defaultBodyBytes)
	end := min(len(body), offset+maximum)
	for end > offset && end < len(body) && !utf8.RuneStart(body[end]) {
		end--
	}
	// A caller requesting fewer bytes than one rune still makes progress.
	if end == offset && offset < len(body) {
		_, size := utf8.DecodeRuneInString(body[offset:])
		end = offset + size
	}
	var next *int
	if end < len(body) {
		next = &end
	}
	return body[offset:end], next
}

func boundBody(body *string, offset, maximum int, meta *ResponseMeta) {
	chunk, next := bodyChunk(*body, offset, maximum)
	*body = chunk
	meta.NextBodyOffset = next
	if next != nil {
		meta.Truncated = true
		meta.Warnings = append(meta.Warnings, "Body is partial; continue with body_offset=next_body_offset.")
	}
}

// Tool admission is shared by all per-token servers in this process.
var toolSlots = make(chan struct{}, 8)

func toolBudget(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, request)
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		select {
		case toolSlots <- struct{}{}:
			defer func() { <-toolSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		result, err := next(ctx, method, request)
		if err != nil {
			return result, err
		}
		call, ok := result.(*mcp.CallToolResult)
		if !ok || call.StructuredContent == nil {
			return result, nil
		}
		payload, err := json.Marshal(call.StructuredContent)
		if err != nil {
			return result, err
		}
		maximum := 64 << 10
		// Diffs have an explicit larger output contract, including JSON escaping.
		var envelope map[string]any
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return result, err
		}
		data, _ := envelope["data"].(map[string]any)
		if _, ok := data["diff"]; ok {
			maximum = 4 << 20
		}
		if len(payload) <= maximum {
			return result, nil
		}
		meta, _ := envelope["meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			envelope["meta"] = meta
		}
		meta["truncated"] = true
		warnings, _ := meta["warnings"].([]any)
		meta["warnings"] = append(warnings, "Structured output reached its byte budget; narrow the request or continue using its cursor.")
		// Preserve identity and the success/error status, especially after writes.
		// Domain handlers provide continuation for bodies and ordinary lists.
		for limit := 4096; limit >= 1; limit /= 2 {
			trimOutput(data, limit)
			payload, err = json.Marshal(envelope)
			if err != nil {
				return result, err
			}
			if len(payload) <= maximum {
				break
			}
		}
		if len(payload) > maximum {
			requestID, _ := meta["request_id"].(string)
			failed, response := contentError[any](requestID, &gogs.Error{Code: gogs.CodeResponseTooLarge, Message: "The tool response metadata exceeds its output budget; reduce the page size or narrow the request."})
			failed.StructuredContent = response
			encoded, encodeErr := json.Marshal(response)
			if encodeErr != nil {
				return nil, encodeErr
			}
			failed.Content = []mcp.Content{&mcp.TextContent{Text: string(encoded)}}
			return failed, nil
		}
		call.StructuredContent = envelope
		call.Content = []mcp.Content{&mcp.TextContent{Text: string(payload)}}
		return call, nil
	}
}

func trimOutput(value any, limit int) {
	if value, ok := value.(map[string]any); ok {
		for key, item := range value {
			switch item := item.(type) {
			case string:
				if len(item) > limit && (key == "title" || key == "description" || key == "message" || key == "diff") {
					end := limit
					for end > 0 && !utf8.RuneStart(item[end]) {
						end--
					}
					value[key] = item[:end]
					if key == "diff" {
						value["truncated"] = true
					}
				}
			case []any:
				// Keep every page member; trimming arrays would invalidate its cursor.
				for _, entry := range item {
					trimOutput(entry, limit)
				}
			default:
				trimOutput(item, limit)
			}
		}
	}
}

func validateBodyOffset(body string, offset int) *gogs.Error {
	if offset < 0 || offset > len(body) || (offset < len(body) && !utf8.RuneStart(body[offset])) {
		return &gogs.Error{Code: gogs.CodeInvalidArgument, Message: "body_offset must be a UTF-8 boundary within the current body."}
	}
	return nil
}
