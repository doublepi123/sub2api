package kiro

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestMCPURLFollowsUsageLimitsHost(t *testing.T) {
	require.Equal(t, "https://codewhisperer.us-east-1.amazonaws.com/mcp", MCPURL(""))
	require.Equal(t, "https://q.eu-central-1.amazonaws.com/mcp", MCPURL("eu-central-1"))
	require.Equal(t, "https://runtime.us-east-1.kiro.dev/generateAssistantResponse", RuntimeURL("us-east-1"))
}

func TestIsStandaloneWebSearch(t *testing.T) {
	require.True(t, IsStandaloneWebSearch([]apicompat.AnthropicTool{{Name: "web_search", Type: "web_search_20250305"}}))
	require.False(t, IsStandaloneWebSearch([]apicompat.AnthropicTool{
		{Name: "web_search"}, {Name: "lookup"},
	}))
	require.False(t, IsStandaloneWebSearch(nil))
}

func TestIsBuiltinKiroToolKeepsTypedWebSearch(t *testing.T) {
	require.True(t, isBuiltinKiroTool(apicompat.AnthropicTool{Name: "web_search", Type: "web_search_20250305"}))
	require.True(t, isBuiltinKiroTool(apicompat.AnthropicTool{Name: "lookup", Type: "web_search_20250305"}))
	require.False(t, isBuiltinKiroTool(apicompat.AnthropicTool{Name: "lookup", Type: "computer_20250124"}))
}

func TestExtractWebSearchQueryStripsClaudeCodePrefix(t *testing.T) {
	require.Equal(t, "golang channels", ExtractWebSearchQuery([]apicompat.AnthropicMessage{{
		Role:    "user",
		Content: json.RawMessage(`"Perform a web search for the query: golang channels"`),
	}}))
	require.Equal(t, "rust ownership", ExtractWebSearchQuery([]apicompat.AnthropicMessage{{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"text","text":"rust ownership"}]`),
	}}))
	require.Empty(t, ExtractWebSearchQuery(nil))
}

func TestNewWebSearchMCPRequestShape(t *testing.T) {
	req, err := NewWebSearchMCPRequest(context.Background(), "us-east-1", "arn:custom", "token", "golang")
	require.NoError(t, err)
	require.Equal(t, "https://codewhisperer.us-east-1.amazonaws.com/mcp", req.URL.String())
	require.Equal(t, "Bearer token", req.Header.Get("Authorization"))
	require.Equal(t, "arn:custom", req.Header.Get("x-amzn-kiro-profile-arn"))
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))
	require.Equal(t, "2.0", payload["jsonrpc"])
	require.Equal(t, "tools/call", payload["method"])
	require.True(t, strings.HasPrefix(payload["id"].(string), "web_search_tooluse_"))
	params := requireMap(t, payload["params"])
	require.Equal(t, "web_search", params["name"])
	require.Equal(t, "golang", requireMap(t, params["arguments"])["query"])
}

func TestParseWebSearchMCPResponse(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Go\",\"url\":\"https://go.dev\",\"snippet\":\"The Go language\",\"publishedDate\":1714521600000}]}"}]}}`)
	results := ParseWebSearchMCPResponse(body)
	require.Len(t, results, 1)
	require.Equal(t, "Go", results[0].Title)
	require.Equal(t, "https://go.dev", results[0].URL)
	require.Equal(t, "May 1, 2024", formatPageAge(results[0].PublishedDate))
	require.Empty(t, ParseWebSearchMCPResponse([]byte(`{"error":{"message":"nope"}}`)))
}

func TestWriteWebSearchResponseStreamAndBuffered(t *testing.T) {
	results := []WebSearchResult{{Title: "Go", URL: "https://go.dev", Snippet: "The Go language", PublishedDate: 1714521600000}}
	var streamOut strings.Builder
	require.NoError(t, WriteWebSearchResponse(&streamOut, "claude-haiku-4.5", "golang", results, 9, true))
	stream := streamOut.String()
	require.Contains(t, stream, `"type":"message_start"`)
	require.Contains(t, stream, `I'll search for \"golang\".`)
	require.Contains(t, stream, `"type":"server_tool_use"`)
	require.Contains(t, stream, `"type":"web_search_tool_result"`)
	require.Contains(t, stream, `"page_age":"May 1, 2024"`)
	require.Contains(t, stream, `"web_search_requests":1`)
	require.Contains(t, stream, `event: message_stop`)

	var buffered strings.Builder
	require.NoError(t, WriteWebSearchResponse(&buffered, "claude-haiku-4.5", "golang", results, 9, false))
	var response map[string]any
	require.NoError(t, json.Unmarshal([]byte(buffered.String()), &response))
	require.Equal(t, "end_turn", response["stop_reason"])
	content := requireSlice(t, response["content"])
	require.Equal(t, "server_tool_use", requireMap(t, content[1])["type"])
	require.Equal(t, "web_search_tool_result", requireMap(t, content[2])["type"])
}
