package kiro

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSocialRefreshURLUsesDesktopAuthHost(t *testing.T) {
	require.Equal(t, "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken", SocialRefreshURL(""))
	require.Equal(t, "https://prod.eu-central-1.auth.desktop.kiro.dev/refreshToken", SocialRefreshURL("eu-central-1"))
}

func TestBuildRequestConvertsHistoryToolsAndSanitizesSchema(t *testing.T) {
	body := []byte(`{
      "model":"public-alias",
      "system":[{"type":"text","text":"system rules"}],
      "messages":[
        {"role":"user","content":"question"},
        {"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"tool_1","name":"lookup","input":{"q":"x"}}]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"result"}]}
      ],
      "tools":[{"name":"lookup","description":"Lookup","input_schema":{"type":"object","properties":{"q":{"type":"string","additionalProperties":false}},"required":[],"additionalProperties":false}}]
    }`)

	payload, tokens, err := BuildRequest(body, "claude-haiku-4.5", "arn:test")
	require.NoError(t, err)
	require.Positive(t, tokens)

	var got map[string]any
	require.NoError(t, json.Unmarshal(payload, &got))
	require.Equal(t, "arn:test", got["profileArn"])
	state := requireMap(t, got["conversationState"])
	history := requireSlice(t, state["history"])
	require.Len(t, history, 2)
	first := requireMap(t, requireMap(t, history[0])["userInputMessage"])
	require.Contains(t, first["content"], "system rules")
	assistant := requireMap(t, requireMap(t, history[1])["assistantResponseMessage"])
	require.Len(t, assistant["toolUses"], 1)
	current := requireMap(t, requireMap(t, state["currentMessage"])["userInputMessage"])
	require.Equal(t, "claude-haiku-4.5", current["modelId"])
	ctx := requireMap(t, current["userInputMessageContext"])
	require.Len(t, ctx["toolResults"], 1)
	tools := requireSlice(t, ctx["tools"])
	spec := requireMap(t, requireMap(t, tools[0])["toolSpecification"])
	inputSchema := requireMap(t, spec["inputSchema"])
	schema := requireMap(t, inputSchema["json"])
	require.NotContains(t, schema, "required")
	require.NotContains(t, schema, "additionalProperties")
}

func TestTransformResponseTextAndToolEvents(t *testing.T) {
	events := [][]byte{
		[]byte(`{"content":"hello","modelId":"claude-haiku-4.5"}`),
		[]byte(`{"name":"lookup","toolUseId":"tool_1","input":{}}`),
		[]byte(`{"name":"lookup","toolUseId":"tool_1","input":"{\"q\":"}`),
		[]byte(`{"name":"lookup","toolUseId":"tool_1","input":"\"x\"}"}`),
		[]byte(`{"name":"lookup","toolUseId":"tool_1","input":{},"stop":true}`),
		[]byte(`{"stopReason":"END_TURN"}`),
	}
	var source bytes.Buffer
	for _, event := range events {
		_, err := source.Write(encodeEvent(t, event))
		require.NoError(t, err)
	}
	var out bytes.Buffer
	require.NoError(t, TransformResponse(&oneByteReader{r: bytes.NewReader(source.Bytes())}, &out, "claude-haiku-4.5", 12, true))

	stream := out.String()
	require.Contains(t, stream, `"type":"message_start"`)
	require.Contains(t, stream, `"text":"hello","type":"text_delta"`)
	require.Contains(t, stream, `"id":"tool_1","input":{},"name":"lookup","type":"tool_use"`)
	require.Contains(t, stream, `"partial_json":"{\"q\":"`)
	require.Contains(t, stream, `"partial_json":"\"x\"}"`)
	require.Equal(t, 1, bytes.Count(out.Bytes(), []byte(`"type":"tool_use"`)))
	require.Contains(t, stream, `"stop_reason":"tool_use"`)
	require.Contains(t, stream, `event: message_stop`)
}

func TestTransformResponseBufferedAnthropicJSON(t *testing.T) {
	var source bytes.Buffer
	_, err := source.Write(encodeEvent(t, []byte(`{"content":"KIRO_OK"}`)))
	require.NoError(t, err)
	_, err = source.Write(encodeEvent(t, []byte(`{"stopReason":"END_TURN"}`)))
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, TransformResponse(&source, &out, "claude-haiku-4.5", 4, false))

	var response map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &response))
	require.Equal(t, "claude-haiku-4.5", response["model"])
	require.Equal(t, "end_turn", response["stop_reason"])
	content := requireSlice(t, response["content"])
	require.Equal(t, "KIRO_OK", requireMap(t, content[0])["text"])
	require.Equal(t, float64(4), requireMap(t, response["usage"])["input_tokens"])
}

func TestDecodeEventStreamRejectsBadCRC(t *testing.T) {
	frame := encodeEvent(t, []byte(`{"content":"x"}`))
	frame[len(frame)-1] ^= 0xff
	_, err := DecodeEventStream(bytes.NewReader(frame))
	require.ErrorContains(t, err, "message CRC")
}

func TestTransformResponseRejectsTruncatedEvent(t *testing.T) {
	frame := encodeEvent(t, []byte(`{"content":"x"}`))
	var out bytes.Buffer
	err := TransformResponse(bytes.NewReader(frame[:len(frame)-1]), &out, "claude-haiku-4.5", 1, false)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestToolUseContributesToEstimatedOutputTokens(t *testing.T) {
	var source bytes.Buffer
	_, err := source.Write(encodeEvent(t, []byte(`{"name":"lookup","toolUseId":"tool_1","input":{}}`)))
	require.NoError(t, err)
	_, err = source.Write(encodeEvent(t, []byte(`{"input":"{\"q\":\"long tool argument\"}"}`)))
	require.NoError(t, err)
	_, err = source.Write(encodeEvent(t, []byte(`{"stopReason":"END_TURN"}`)))
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, TransformResponse(&source, &out, "claude-haiku-4.5", 1, false))

	var response map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &response))
	require.Positive(t, requireMap(t, response["usage"])["output_tokens"])
}

func requireMap(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	require.True(t, ok)
	return result
}

func requireSlice(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	require.True(t, ok)
	return result
}

type oneByteReader struct{ r io.Reader }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.r.Read(p)
}

func encodeEvent(t *testing.T, payload []byte) []byte {
	t.Helper()
	headers := encodeHeader(":message-type", "event")
	headers = append(headers, encodeHeader(":event-type", "assistantResponseEvent")...)
	total := 12 + len(headers) + len(payload) + 4
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payload)
	binary.BigEndian.PutUint32(frame[total-4:], crc32.ChecksumIEEE(frame[:total-4]))
	return frame
}

func encodeHeader(name, value string) []byte {
	result := []byte{byte(len(name))}
	result = append(result, name...)
	result = append(result, 7, 0, 0)
	binary.BigEndian.PutUint16(result[len(result)-2:], uint16(len(value)))
	return append(result, value...)
}
