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
	state := got["conversationState"].(map[string]any)
	history := state["history"].([]any)
	require.Len(t, history, 2)
	first := history[0].(map[string]any)["userInputMessage"].(map[string]any)
	require.Contains(t, first["content"], "system rules")
	assistant := history[1].(map[string]any)["assistantResponseMessage"].(map[string]any)
	require.Len(t, assistant["toolUses"], 1)
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	require.Equal(t, "claude-haiku-4.5", current["modelId"])
	ctx := current["userInputMessageContext"].(map[string]any)
	require.Len(t, ctx["toolResults"], 1)
	tools := ctx["tools"].([]any)
	schema := tools[0].(map[string]any)["toolSpecification"].(map[string]any)["inputSchema"].(map[string]any)["json"].(map[string]any)
	require.NotContains(t, schema, "required")
	require.NotContains(t, schema, "additionalProperties")
}

func TestTransformResponseTextAndToolEvents(t *testing.T) {
	events := [][]byte{
		[]byte(`{"content":"hello","modelId":"claude-haiku-4.5"}`),
		[]byte(`{"name":"lookup","toolUseId":"tool_1","input":{}}`),
		[]byte(`{"input":"{\"q\":\"x\"}"}`),
		[]byte(`{"stop":true}`),
		[]byte(`{"stopReason":"END_TURN"}`),
	}
	var source bytes.Buffer
	for _, event := range events {
		source.Write(encodeEvent(t, event))
	}
	var out bytes.Buffer
	require.NoError(t, TransformResponse(&oneByteReader{r: bytes.NewReader(source.Bytes())}, &out, "claude-haiku-4.5", 12, true))

	stream := out.String()
	require.Contains(t, stream, `"type":"message_start"`)
	require.Contains(t, stream, `"text":"hello","type":"text_delta"`)
	require.Contains(t, stream, `"id":"tool_1","input":{},"name":"lookup","type":"tool_use"`)
	require.Contains(t, stream, `"partial_json":"{\"q\":\"x\"}","type":"input_json_delta"`)
	require.Contains(t, stream, `"stop_reason":"tool_use"`)
	require.Contains(t, stream, `event: message_stop`)
}

func TestTransformResponseBufferedAnthropicJSON(t *testing.T) {
	var source bytes.Buffer
	source.Write(encodeEvent(t, []byte(`{"content":"KIRO_OK"}`)))
	source.Write(encodeEvent(t, []byte(`{"stopReason":"END_TURN"}`)))
	var out bytes.Buffer
	require.NoError(t, TransformResponse(&source, &out, "claude-haiku-4.5", 4, false))

	var response map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &response))
	require.Equal(t, "claude-haiku-4.5", response["model"])
	require.Equal(t, "end_turn", response["stop_reason"])
	require.Equal(t, "KIRO_OK", response["content"].([]any)[0].(map[string]any)["text"])
	require.Equal(t, float64(4), response["usage"].(map[string]any)["input_tokens"])
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
	source.Write(encodeEvent(t, []byte(`{"name":"lookup","toolUseId":"tool_1","input":{}}`)))
	source.Write(encodeEvent(t, []byte(`{"input":"{\"q\":\"long tool argument\"}"}`)))
	source.Write(encodeEvent(t, []byte(`{"stopReason":"END_TURN"}`)))
	var out bytes.Buffer
	require.NoError(t, TransformResponse(&source, &out, "claude-haiku-4.5", 1, false))

	var response map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &response))
	require.Positive(t, response["usage"].(map[string]any)["output_tokens"])
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
