package kiro

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/google/uuid"
)

const (
	DefaultRegion     = "us-east-1"
	DefaultProfileARN = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"
	RuntimeTarget     = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"
	placeholder       = "(empty placeholder)"
	maxEventSize      = 16 << 20
)

var Models = []string{
	"auto", "claude-opus-5", "claude-sonnet-5", "claude-opus-4.8",
	"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "claude-opus-4.7",
	"claude-opus-4.6", "claude-sonnet-4.6", "claude-opus-4.5",
	"claude-sonnet-4.5", "claude-sonnet-4", "claude-haiku-4.5",
	"deepseek-3.2", "minimax-m2.5", "minimax-m2.1", "glm-5", "qwen3-coder-next",
}

type turn struct {
	Role        string
	Text        string
	Images      []map[string]any
	ToolUses    []map[string]any
	ToolResults []map[string]any
}

// RequestBuild is the converted Kiro payload plus metadata needed by the gateway.
type RequestBuild struct {
	Payload     []byte
	InputTokens int
	ToolNameMap map[string]string
	Thinking    bool
	WebSearch   bool
	SearchQuery string
}

// TransformOptions controls response conversion. Thinking tags are always
// parsed so Claude Code can consume standalone thinking blocks.
type TransformOptions struct {
	Model       string
	InputTokens int
	Stream      bool
	ToolNameMap map[string]string
}

func RuntimeURL(region string) string {
	region = normalizedRegion(region)
	return "https://runtime." + region + ".kiro.dev/generateAssistantResponse"
}

func OIDCTokenURL(region string) string {
	region = normalizedRegion(region)
	return "https://oidc." + region + ".amazonaws.com/token"
}

// SocialRefreshURL is the Kiro desktop auth endpoint used by Google/GitHub
// social login. It is distinct from AWS SSO OIDC used by Builder ID / IdC.
func SocialRefreshURL(region string) string {
	region = normalizedRegion(region)
	return "https://prod." + region + ".auth.desktop.kiro.dev/refreshToken"
}

func normalizedRegion(region string) string {
	if region = strings.TrimSpace(region); region != "" {
		return region
	}
	return DefaultRegion
}

// BuildRequest converts an Anthropic Messages request into Kiro's observable
// GenerateAssistantResponse payload. It intentionally has no dependency on any
// third-party Kiro gateway implementation. conversationSeed, when non-empty,
// pins conversationId/agentContinuationId so the same gateway session can hit
// Kiro's implicit prompt cache.
func BuildRequest(body []byte, model, profileARN, conversationSeed string) ([]byte, int, error) {
	built, err := BuildRequestResult(body, model, profileARN, conversationSeed)
	if err != nil {
		return nil, 0, err
	}
	return built.Payload, built.InputTokens, nil
}

func BuildRequestResult(body []byte, model, profileARN, conversationSeed string) (*RequestBuild, error) {
	var req apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode anthropic request: %w", err)
	}
	if strings.TrimSpace(model) == "" {
		model = req.Model
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("kiro model is required")
	}
	if strings.TrimSpace(profileARN) == "" {
		profileARN = DefaultProfileARN
	}

	turns, err := convertTurns(req.Messages)
	if err != nil {
		return nil, err
	}
	system := extractText(req.System)
	prefix := generateThinkingPrefix(req)
	if prefix != "" && !hasThinkingTags(system) {
		if system != "" {
			system = prefix + "\n" + system
		} else {
			turns = append([]turn{
				{Role: "user", Text: prefix},
				{Role: "assistant", Text: "I will follow these instructions."},
			}, turns...)
		}
	}
	if len(turns) == 0 {
		turns = append(turns, turn{Role: "user", Text: placeholder})
	}
	turns = pairToolTurns(turns)
	turns = normalizeTurns(turns)
	if system != "" {
		turns[0].Text = joinText(system, turns[0].Text)
	}

	nameMap := map[string]string{}
	applyToolNameMap(turns, nameMap)
	tools := convertTools(req.Tools, nameMap)
	tools = ensurePlaceholderTools(tools, collectHistoryToolNames(turns))

	history := make([]any, 0, len(turns)-1)
	for _, item := range turns[:len(turns)-1] {
		history = append(history, encodeHistoryTurn(item, model))
	}
	current := turns[len(turns)-1]
	if current.Role == "assistant" {
		history = append(history, encodeHistoryTurn(current, model))
		current = turn{Role: "user", Text: placeholder}
	}

	ctx := map[string]any{}
	if len(tools) > 0 {
		ctx["tools"] = tools
	}
	if len(current.ToolResults) > 0 {
		ctx["toolResults"] = current.ToolResults
	}
	userInput := map[string]any{
		"content": nonEmpty(current.Text),
		"modelId": model,
		"origin":  "AI_EDITOR",
	}
	if len(current.Images) > 0 {
		userInput["images"] = current.Images
	}
	if len(ctx) > 0 {
		userInput["userInputMessageContext"] = ctx
	} else {
		userInput["userInputMessageContext"] = map[string]any{"tools": []any{}}
	}

	conversationID := conversationIDFromSeed(conversationSeed)
	state := map[string]any{
		"agentContinuationId": agentContinuationIDFromConversation(conversationID),
		"agentTaskType":       "vibe",
		"chatTriggerType":     "MANUAL",
		"conversationId":      conversationID,
		"currentMessage": map[string]any{
			"userInputMessage": userInput,
		},
		"history": history,
	}
	payload, err := json.Marshal(map[string]any{
		"conversationState": state,
		"profileArn":        profileARN,
	})
	if err != nil {
		return nil, fmt.Errorf("encode kiro request: %w", err)
	}
	return &RequestBuild{
		Payload:     payload,
		InputTokens: estimateTokens(body),
		ToolNameMap: nameMap,
		Thinking:    prefix != "",
		WebSearch:   IsStandaloneWebSearch(req.Tools),
		SearchQuery: ExtractWebSearchQuery(req.Messages),
	}, nil
}

func convertTurns(messages []apicompat.AnthropicMessage) ([]turn, error) {
	result := make([]turn, 0, len(messages))
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role != "assistant" {
			role = "user"
		}
		item := turn{Role: role}
		var plain string
		if err := json.Unmarshal(msg.Content, &plain); err == nil {
			item.Text = plain
		} else {
			var blocks []apicompat.AnthropicContentBlock
			if err := json.Unmarshal(msg.Content, &blocks); err != nil {
				return nil, fmt.Errorf("decode %s message content: %w", role, err)
			}
			var texts []string
			var thinkingParts []string
			for _, block := range blocks {
				switch block.Type {
				case "text":
					if strings.TrimSpace(block.Text) != "" {
						texts = append(texts, block.Text)
					}
				case "thinking":
					if strings.TrimSpace(block.Thinking) != "" {
						thinkingParts = append(thinkingParts, block.Thinking)
					}
				case "image":
					if img := convertImage(block.Source); img != nil {
						item.Images = append(item.Images, img)
					}
				case "tool_use":
					var input any = map[string]any{}
					if len(block.Input) > 0 {
						_ = json.Unmarshal(block.Input, &input)
					}
					item.ToolUses = append(item.ToolUses, map[string]any{"name": block.Name, "input": input, "toolUseId": block.ID})
				case "tool_result":
					item.ToolResults = append(item.ToolResults, map[string]any{
						"content":   []map[string]string{{"text": nonEmpty(extractText(block.Content))}},
						"status":    map[bool]string{true: "error", false: "success"}[block.IsError],
						"toolUseId": block.ToolUseID,
					})
				}
			}
			item.Text = wrapAssistantThinking(strings.Join(thinkingParts, ""), strings.Join(texts, "\n"), len(item.ToolUses) > 0)
		}
		result = append(result, item)
	}
	return result, nil
}

func normalizeTurns(input []turn) []turn {
	merged := make([]turn, 0, len(input)+2)
	for _, item := range input {
		if len(merged) == 0 && item.Role != "user" {
			merged = append(merged, turn{Role: "user", Text: placeholder})
		}
		if len(merged) > 0 && merged[len(merged)-1].Role == item.Role {
			if item.Role == "user" {
				merged = append(merged, turn{Role: "assistant", Text: placeholder})
			} else {
				merged = append(merged, turn{Role: "user", Text: placeholder})
			}
		}
		if strings.TrimSpace(item.Text) == "" && len(item.ToolUses) == 0 && len(item.ToolResults) == 0 && len(item.Images) == 0 {
			item.Text = placeholder
		}
		merged = append(merged, item)
	}
	return merged
}

func encodeHistoryTurn(item turn, model string) any {
	if item.Role == "assistant" {
		msg := map[string]any{"content": assistantContent(item)}
		if len(item.ToolUses) > 0 {
			msg["toolUses"] = item.ToolUses
		}
		return map[string]any{"assistantResponseMessage": msg}
	}
	msg := map[string]any{"content": nonEmpty(item.Text), "modelId": model, "origin": "AI_EDITOR"}
	if len(item.Images) > 0 {
		msg["images"] = item.Images
	}
	if len(item.ToolResults) > 0 {
		msg["userInputMessageContext"] = map[string]any{"toolResults": item.ToolResults}
	}
	return map[string]any{"userInputMessage": msg}
}

func assistantContent(item turn) string {
	if len(item.ToolUses) > 0 && strings.TrimSpace(item.Text) == "" {
		return " "
	}
	return nonEmpty(item.Text)
}

func convertTools(tools []apicompat.AnthropicTool, nameMap map[string]string) []any {
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" {
			continue
		}
		if tool.Type != "" && !isBuiltinKiroTool(tool) {
			continue
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.InputSchema) > 0 {
			_ = json.Unmarshal(tool.InputSchema, &schema)
			schema = sanitizeSchema(schema)
		}
		result = append(result, map[string]any{"toolSpecification": map[string]any{
			"name":        mapToolName(tool.Name, nameMap),
			"description": truncateRunesStrict(tool.Description, toolDescriptionMaxLen),
			"inputSchema": map[string]any{"json": schema},
		}})
	}
	return result
}

func sanitizeSchema(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			if key == "additionalProperties" {
				continue
			}
			if key == "required" {
				if a, ok := child.([]any); ok && len(a) == 0 {
					continue
				}
			}
			out[key] = sanitizeSchema(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = sanitizeSchema(v[i])
		}
		return out
	default:
		return value
	}
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if text, ok := block["text"].(string); ok && text != "" {
			texts = append(texts, text)
			continue
		}
		if content, ok := block["content"]; ok {
			if b, err := json.Marshal(content); err == nil {
				if text := extractText(b); text != "" {
					texts = append(texts, text)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

func nonEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return placeholder
	}
	return s
}

func joinText(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	if strings.TrimSpace(b) == "" || b == placeholder {
		return a
	}
	return a + "\n\n" + b
}

func estimateTokens(data []byte) int {
	n := utf8.RuneCount(data)
	if n == 0 {
		return 0
	}
	return (n + 3) / 4
}

func conversationIDFromSeed(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return uuid.NewString()
	}
	return deriveStableUUIDv4("kiro:conv:v1:" + seed)
}

func agentContinuationIDFromConversation(conversationID string) string {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return uuid.NewString()
	}
	return deriveStableUUIDv4("kiro:agent:v1:" + conversationID)
}

func deriveStableUUIDv4(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16])
}

type eventMessage struct {
	Headers map[string]string
	Payload []byte
}

func readEvent(r io.Reader) (*eventMessage, error) {
	prelude := make([]byte, 12)
	if _, err := io.ReadFull(r, prelude); err != nil {
		return nil, err
	}
	total := int(binary.BigEndian.Uint32(prelude[:4]))
	headerLen := int(binary.BigEndian.Uint32(prelude[4:8]))
	if total < 16 || total > maxEventSize || headerLen < 0 || 12+headerLen+4 > total {
		return nil, fmt.Errorf("invalid AWS event-stream lengths: total=%d headers=%d", total, headerLen)
	}
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return nil, errors.New("invalid AWS event-stream prelude CRC")
	}
	rest := make([]byte, total-12)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	full := append(append([]byte(nil), prelude...), rest[:len(rest)-4]...)
	if crc32.ChecksumIEEE(full) != binary.BigEndian.Uint32(rest[len(rest)-4:]) {
		return nil, errors.New("invalid AWS event-stream message CRC")
	}
	headers, err := parseHeaders(rest[:headerLen])
	if err != nil {
		return nil, err
	}
	payload := append([]byte(nil), rest[headerLen:len(rest)-4]...)
	return &eventMessage{Headers: headers, Payload: payload}, nil
}

func parseHeaders(data []byte) (map[string]string, error) {
	result := map[string]string{}
	for len(data) > 0 {
		nameLen := int(data[0])
		data = data[1:]
		if nameLen == 0 || len(data) < nameLen+1 {
			return nil, errors.New("invalid AWS event-stream header")
		}
		name := string(data[:nameLen])
		typ := data[nameLen]
		data = data[nameLen+1:]
		if typ != 7 || len(data) < 2 {
			return nil, fmt.Errorf("unsupported AWS event-stream header type %d", typ)
		}
		valueLen := int(binary.BigEndian.Uint16(data[:2]))
		data = data[2:]
		if len(data) < valueLen {
			return nil, errors.New("truncated AWS event-stream header")
		}
		result[name] = string(data[:valueLen])
		data = data[valueLen:]
	}
	return result, nil
}

type streamState struct {
	model            string
	id               string
	inputTokens      int
	outputTokens     int
	cacheReadTokens  int
	cacheWriteTokens int
	hasUpstreamUsage bool
	outputText       strings.Builder
	blocks           []apicompat.AnthropicContentBlock
	active           int
	hasActive        bool
	hasTool          bool
	stopReason       string
	toolNameMap      map[string]string
	thinking         thinkingParser
}

// TransformResponse converts Kiro's AWS event-stream response into either an
// Anthropic event stream or a buffered Anthropic Messages response.
func TransformResponse(src io.Reader, dst io.Writer, model string, inputTokens int, stream bool) error {
	return TransformResponseWithOptions(src, dst, TransformOptions{Model: model, InputTokens: inputTokens, Stream: stream})
}

func TransformResponseWithOptions(src io.Reader, dst io.Writer, opts TransformOptions) error {
	state := &streamState{
		model: opts.Model, id: "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		inputTokens: opts.InputTokens, toolNameMap: opts.ToolNameMap,
	}
	stream := opts.Stream
	var buffered bytes.Buffer
	sink := dst
	if !stream {
		sink = &buffered
	}
	if err := state.emitStart(sink); err != nil {
		return err
	}
	for {
		msg, err := readEvent(src)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if msg.Headers[":message-type"] == "exception" || strings.HasSuffix(msg.Headers[":event-type"], "Exception") {
			return fmt.Errorf("kiro stream exception: %s", strings.TrimSpace(string(msg.Payload)))
		}
		if err := state.consume(sink, msg.Payload); err != nil {
			return err
		}
	}
	if err := state.flushThinking(sink); err != nil {
		return err
	}
	if state.stopReason == "" {
		state.stopReason = "end_turn"
	}
	if err := state.emitFinish(sink); err != nil {
		return err
	}
	if stream {
		return nil
	}
	stop := state.stopReason
	response := apicompat.AnthropicResponse{
		ID: state.id, Type: "message", Role: "assistant", Content: state.blocks,
		Model: state.model, StopReason: &stop,
		Usage: state.anthropicUsage(),
	}
	return json.NewEncoder(dst).Encode(response)
}

func (s *streamState) emitStart(w io.Writer) error {
	event := map[string]any{"type": "message_start", "message": map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "content": []any{},
		"model": s.model, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": s.inputTokens, "output_tokens": 0},
	}}
	return writeSSE(w, "message_start", event)
}

func (s *streamState) consume(w io.Writer, payload []byte) error {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return fmt.Errorf("decode kiro event: %w", err)
	}
	s.applyTokenUsage(obj)
	if content, ok := obj["content"].(string); ok {
		return s.emitThinkingEvents(w, s.thinking.push(content))
	}
	toolEvent := false
	if name, ok := obj["name"].(string); ok {
		if err := s.flushThinking(w); err != nil {
			return err
		}
		id, _ := obj["toolUseId"].(string)
		if id == "" {
			id = "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		name = restoreToolName(name, s.toolNameMap)
		currentMatches := s.hasActive && s.blocks[s.active].Type == "tool_use" && s.blocks[s.active].ID == id
		if !currentMatches {
			if err := s.closeBlock(w); err != nil {
				return err
			}
			s.blocks = append(s.blocks, apicompat.AnthropicContentBlock{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(`{}`)})
			s.active = len(s.blocks) - 1
			s.hasActive = true
			s.hasTool = true
			if err := writeSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": s.active, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}}); err != nil {
				return err
			}
		}
		toolEvent = true
	}
	if input, exists := obj["input"]; exists && s.hasActive && s.blocks[s.active].Type == "tool_use" {
		if err := s.consumeToolInput(w, input); err != nil {
			return err
		}
		toolEvent = true
	}
	if stop, ok := obj["stop"].(bool); ok && stop {
		return s.closeBlock(w)
	}
	if reason, ok := obj["stopReason"].(string); ok {
		s.stopReason = mapStopReason(reason, s.hasTool)
	}
	if toolEvent {
		return nil
	}
	return nil
}

func (s *streamState) applyTokenUsage(obj map[string]any) {
	if s == nil || len(obj) == 0 {
		return
	}
	raw, ok := obj["tokenUsage"]
	if !ok {
		if nested, nestedOK := obj["metadata"].(map[string]any); nestedOK {
			raw, ok = nested["tokenUsage"]
		}
	}
	usage, ok := raw.(map[string]any)
	if !ok || len(usage) == 0 {
		return
	}
	if v, ok := parseTokenCount(usage["uncachedInputTokens"]); ok {
		s.inputTokens = v
		s.hasUpstreamUsage = true
	}
	if v, ok := parseTokenCount(usage["outputTokens"]); ok {
		s.outputTokens = v
		s.hasUpstreamUsage = true
	}
	if v, ok := parseTokenCount(usage["cacheReadInputTokens"]); ok {
		s.cacheReadTokens = v
		s.hasUpstreamUsage = true
	}
	if v, ok := parseTokenCount(usage["cacheWriteInputTokens"]); ok {
		s.cacheWriteTokens = v
		s.hasUpstreamUsage = true
	}
}

func parseTokenCount(value any) (int, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil || n < 0 {
			return 0, false
		}
		return int(n), true
	case float64:
		if v < 0 {
			return 0, false
		}
		return int(v), true
	case int:
		if v < 0 {
			return 0, false
		}
		return v, true
	case int64:
		if v < 0 {
			return 0, false
		}
		return int(v), true
	default:
		return 0, false
	}
}

func (s *streamState) anthropicUsage() apicompat.AnthropicUsage {
	return apicompat.AnthropicUsage{
		InputTokens:              s.finalInputTokens(),
		OutputTokens:             s.finalOutputTokens(),
		CacheReadInputTokens:     s.cacheReadTokens,
		CacheCreationInputTokens: s.cacheWriteTokens,
	}
}

func (s *streamState) finalInputTokens() int {
	if s == nil {
		return 0
	}
	return s.inputTokens
}

func (s *streamState) finalOutputTokens() int {
	if s == nil {
		return 0
	}
	if s.hasUpstreamUsage && s.outputTokens > 0 {
		return s.outputTokens
	}
	return s.estimatedOutputTokens()
}

func (s *streamState) usageEvent() map[string]int {
	usage := s.anthropicUsage()
	out := map[string]int{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
	}
	if usage.CacheReadInputTokens > 0 {
		out["cache_read_input_tokens"] = usage.CacheReadInputTokens
	}
	if usage.CacheCreationInputTokens > 0 {
		out["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	}
	return out
}

func (s *streamState) consumeToolInput(w io.Writer, input any) error {
	fragment := ""
	switch value := input.(type) {
	case string:
		fragment = value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fragment = string(encoded)
	}
	if fragment == "" || fragment == "{}" || fragment == `""` {
		return nil
	}
	s.blocks[s.active].Input = appendJSONFragment(s.blocks[s.active].Input, fragment)
	return writeSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.active, "delta": map[string]any{"type": "input_json_delta", "partial_json": fragment}})
}

func (s *streamState) flushThinking(w io.Writer) error {
	return s.emitThinkingEvents(w, s.thinking.flushBoundary())
}

func (s *streamState) emitThinkingEvents(w io.Writer, events []thinkingEvent) error {
	for _, event := range events {
		switch event.kind {
		case thinkingEventStart:
			if err := s.startThinking(w); err != nil {
				return err
			}
		case thinkingEventDelta:
			if event.text == "" && (!s.hasActive || s.blocks[s.active].Type != "thinking") {
				continue
			}
			if !s.hasActive || s.blocks[s.active].Type != "thinking" {
				if err := s.startThinking(w); err != nil {
					return err
				}
			}
			s.blocks[s.active].Thinking += event.text
			_, _ = s.outputText.WriteString(event.text)
			if err := writeSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.active, "delta": map[string]any{"type": "thinking_delta", "thinking": event.text}}); err != nil {
				return err
			}
		case thinkingEventStop:
			if err := s.closeBlock(w); err != nil {
				return err
			}
		case thinkingEventText:
			if err := s.emitText(w, event.text); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamState) emitText(w io.Writer, content string) error {
	if content == "" {
		return nil
	}
	if !s.hasActive || s.blocks[s.active].Type != "text" {
		if err := s.startText(w); err != nil {
			return err
		}
	}
	s.blocks[s.active].Text += content
	_, _ = s.outputText.WriteString(content)
	return writeSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.active, "delta": map[string]any{"type": "text_delta", "text": content}})
}

func (s *streamState) startThinking(w io.Writer) error {
	if err := s.closeBlock(w); err != nil {
		return err
	}
	s.blocks = append(s.blocks, apicompat.AnthropicContentBlock{Type: "thinking", Thinking: ""})
	s.active = len(s.blocks) - 1
	s.hasActive = true
	return writeSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": s.active, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
}

func (s *streamState) startText(w io.Writer) error {
	if err := s.closeBlock(w); err != nil {
		return err
	}
	s.blocks = append(s.blocks, apicompat.AnthropicContentBlock{Type: "text", Text: ""})
	s.active = len(s.blocks) - 1
	s.hasActive = true
	return writeSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": s.active, "content_block": map[string]any{"type": "text", "text": ""}})
}

func (s *streamState) closeBlock(w io.Writer) error {
	if !s.hasActive {
		return nil
	}
	err := writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.active})
	s.hasActive = false
	return err
}

func (s *streamState) emitFinish(w io.Writer) error {
	if err := s.closeBlock(w); err != nil {
		return err
	}
	if err := writeSSE(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": s.stopReason, "stop_sequence": nil}, "usage": s.usageEvent()}); err != nil {
		return err
	}
	return writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

func (s *streamState) estimatedOutputTokens() int {
	var output strings.Builder
	for _, block := range s.blocks {
		_, _ = output.WriteString(block.Text)
		_, _ = output.WriteString(block.Thinking)
		if block.Type == "tool_use" {
			_, _ = output.WriteString(block.Name)
			_, _ = output.Write(block.Input)
		}
	}
	return estimateTokens([]byte(output.String()))
}

func writeSSE(w io.Writer, event string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

func mapStopReason(reason string, hasTool bool) string {
	if hasTool {
		return "tool_use"
	}
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS", "LENGTH":
		return "max_tokens"
	case "STOP_SEQUENCE":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func appendJSONFragment(current json.RawMessage, fragment string) json.RawMessage {
	if len(current) == 0 || string(current) == "{}" {
		return json.RawMessage(fragment)
	}
	return append(current, fragment...)
}

// DecodeEventStream is exported for diagnostics and focused framing tests.
func DecodeEventStream(r io.Reader) ([]json.RawMessage, error) {
	reader := bufio.NewReader(r)
	var result []json.RawMessage
	for {
		msg, err := readEvent(reader)
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		result = append(result, append(json.RawMessage(nil), msg.Payload...))
	}
}
