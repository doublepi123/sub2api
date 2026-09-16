package kiro

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/google/uuid"
)

const (
	webSearchToolName     = "web_search"
	webSearchQueryPrefix  = "Perform a web search for the query: "
	webSearchSummaryChunk = 100
)

// MCPURL is the Kiro MCP endpoint. The host follows the same regional q.*
// convention already used by UsageLimitsHost; the generateAssistantResponse
// URL stays on runtime.*.kiro.dev.
func MCPURL(region string) string {
	return "https://" + UsageLimitsHost(region) + "/mcp"
}

func IsStandaloneWebSearch(tools []apicompat.AnthropicTool) bool {
	return len(tools) == 1 && strings.TrimSpace(tools[0].Name) == webSearchToolName
}

func isBuiltinKiroTool(tool apicompat.AnthropicTool) bool {
	if strings.TrimSpace(tool.Name) == webSearchToolName {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(tool.Type)), "web_search")
}

func ExtractWebSearchQuery(messages []apicompat.AnthropicMessage) string {
	if len(messages) == 0 {
		return ""
	}
	text := strings.TrimPrefix(firstMessageText(messages[0]), webSearchQueryPrefix)
	return strings.TrimSpace(text)
}

func firstMessageText(msg apicompat.AnthropicMessage) string {
	var plain string
	if json.Unmarshal(msg.Content, &plain) == nil {
		return plain
	}
	var blocks []apicompat.AnthropicContentBlock
	if json.Unmarshal(msg.Content, &blocks) != nil || len(blocks) == 0 {
		return ""
	}
	if blocks[0].Type == "text" {
		return blocks[0].Text
	}
	return ""
}

func NewWebSearchMCPRequest(ctx context.Context, region, profileARN, accessToken, query string) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id := newWebSearchRequestID()
	payload, err := json.Marshal(map[string]any{
		"id":      id,
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      webSearchToolName,
			"arguments": map[string]string{"query": query},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode kiro web search request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, MCPURL(region), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if arn := strings.TrimSpace(profileARN); arn != "" && arn != DefaultProfileARN {
		req.Header.Set("x-amzn-kiro-profile-arn", arn)
	}
	return req, nil
}

type WebSearchResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Snippet       string `json:"snippet"`
	PublishedDate int64  `json:"publishedDate"`
}

type webSearchResults struct {
	Results []WebSearchResult `json:"results"`
}

func ParseWebSearchMCPResponse(body []byte) []WebSearchResult {
	var envelope struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Result == nil || len(envelope.Result.Content) == 0 {
		return nil
	}
	text := envelope.Result.Content[0].Text
	var parsed webSearchResults
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return nil
	}
	return parsed.Results
}

func newWebSearchRequestID() string {
	return "web_search_tooluse_" + randomAlphaNum(22) + "_" + itoa64(time.Now().UnixMilli()) + "_" + randomLowerAlphaNum(8)
}

func newServerToolUseID() string {
	return "srvtoolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:32]
}

func randomAlphaNum(n int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	return randomFromCharset(n, charset)
}

func randomLowerAlphaNum(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	return randomFromCharset(n, charset)
}

func randomFromCharset(n int, charset string) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		sum := sha256Hex(uuid.NewString())
		raw = []byte(sum)
	}
	for i := 0; i < n; i++ {
		buf[i] = charset[int(raw[i%len(raw)])%len(charset)]
	}
	return string(buf)
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func formatPageAge(ms int64) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms).UTC()
	return t.Format("January") + " " + itoa(t.Day()) + ", " + itoa(t.Year())
}

func webSearchSummary(query string, results []WebSearchResult) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "Here are the search results for %q:\n\n", query)
	if len(results) == 0 {
		_, _ = b.WriteString("No results found.\n")
	} else {
		for i, result := range results {
			_, _ = fmt.Fprintf(&b, "%d. **%s**\n", i+1, result.Title)
			if snippet := strings.TrimSpace(result.Snippet); snippet != "" {
				_, _ = fmt.Fprintf(&b, "   %s\n", truncateRunes(snippet, 200))
			}
			_, _ = fmt.Fprintf(&b, "   Source: %s\n\n", result.URL)
		}
	}
	_, _ = b.WriteString("\nPlease note that these are web search results and may not be fully accurate or up-to-date.")
	return b.String()
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i] + "..."
		}
		n++
	}
	return s
}

func chunkRunes(s string, size int) []string {
	if size <= 0 || s == "" {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var chunks []string
	var b strings.Builder
	n := 0
	for _, r := range s {
		_, _ = b.WriteRune(r)
		n++
		if n == size {
			chunks = append(chunks, b.String())
			b.Reset()
			n = 0
		}
	}
	if n > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks
}

func WriteWebSearchResponse(w io.Writer, model, query string, results []WebSearchResult, inputTokens int, stream bool) error {
	toolUseID := newServerToolUseID()
	summary := webSearchSummary(query, results)
	searchContent := make([]any, 0, len(results))
	for _, result := range results {
		item := map[string]any{
			"type":              "web_search_result",
			"title":             result.Title,
			"url":               result.URL,
			"encrypted_content": result.Snippet,
		}
		if age := formatPageAge(result.PublishedDate); age != "" {
			item["page_age"] = age
		} else {
			item["page_age"] = nil
		}
		searchContent = append(searchContent, item)
	}
	id := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if !stream {
		stop := "end_turn"
		response := apicompat.AnthropicResponse{
			ID:    id,
			Type:  "message",
			Role:  "assistant",
			Model: model,
			Content: []apicompat.AnthropicContentBlock{
				{Type: "text", Text: `I'll search for "` + query + `".`},
				{Type: "server_tool_use", ID: toolUseID, Name: webSearchToolName, Input: mustRawJSON(map[string]string{"query": query})},
				{Type: "web_search_tool_result", Content: mustRawJSON(searchContent)},
				{Type: "text", Text: summary},
			},
			StopReason: &stop,
			Usage:      apicompat.AnthropicUsage{InputTokens: inputTokens, OutputTokens: estimateTokens([]byte(summary))},
		}
		return json.NewEncoder(w).Encode(response)
	}

	if err := writeSSE(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "content": []any{},
		"model": model, "stop_reason": nil,
		"usage": map[string]int{"input_tokens": inputTokens, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
	}}); err != nil {
		return err
	}
	decision := `I'll search for "` + query + `".`
	if err := writeTextBlock(w, 0, decision); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 1,
		"content_block": map[string]any{"id": toolUseID, "type": "server_tool_use", "name": webSearchToolName, "input": map[string]string{"query": query}},
	}); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 1}); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 2,
		"content_block": map[string]any{"type": "web_search_tool_result", "content": searchContent},
	}); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 2}); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 3,
		"content_block": map[string]any{"type": "text", "text": ""},
	}); err != nil {
		return err
	}
	for _, chunk := range chunkRunes(summary, webSearchSummaryChunk) {
		if err := writeSSE(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 3,
			"delta": map[string]any{"type": "text_delta", "text": chunk},
		}); err != nil {
			return err
		}
	}
	if err := writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 3}); err != nil {
		return err
	}
	if err := writeSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{
			"output_tokens":   estimateTokens([]byte(summary)),
			"server_tool_use": map[string]int{"web_search_requests": 1},
		},
	}); err != nil {
		return err
	}
	return writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

func writeTextBlock(w io.Writer, index int, text string) error {
	if err := writeSSE(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "text", "text": ""},
	}); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	}); err != nil {
		return err
	}
	return writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func mustRawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
