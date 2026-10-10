package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHaiku55ResponsesThinkingContract(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max", "none"} {
		t.Run(effort, func(t *testing.T) {
			req := &ResponsesRequest{Model: "claude-haiku-5-5", Input: json.RawMessage(`"hi"`)}
			if effort != "" {
				req.Reasoning = &ResponsesReasoning{Effort: effort}
			}
			out, err := ResponsesToAnthropicRequest(req)
			require.NoError(t, err)
			require.NotNil(t, out.Thinking)
			require.Zero(t, out.Thinking.BudgetTokens)
			want := effort
			if want == "" {
				want = "medium"
			}
			mode := "adaptive"
			if want == "none" {
				want, mode = "low", "disabled"
			}
			require.Equal(t, mode, out.Thinking.Type)
			require.Equal(t, want, out.OutputConfig.Effort)
			chat, err := ChatCompletionsToResponses(&ChatCompletionsRequest{Model: "claude-haiku-5-5", ReasoningEffort: effort, Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}})
			require.NoError(t, err)
			chatOut, err := ResponsesToAnthropicRequest(chat)
			require.NoError(t, err)
			require.Equal(t, out.Thinking, chatOut.Thinking)
			require.Equal(t, out.OutputConfig, chatOut.OutputConfig)
		})
	}
}

func TestHaiku55BufferedSignedHistory(t *testing.T) {
	response := &AnthropicResponse{Model: "claude-haiku-5-5", Content: []AnthropicContentBlock{
		{Type: "thinking", Signature: "signed-empty"},
		{Type: "text", Text: "before"},
		{Type: "redacted_thinking", Data: "opaque-redacted"},
		{Type: "tool_use", ID: "call_1", Name: "lookup", Input: json.RawMessage(`{}`)},
	}}
	out := AnthropicToResponsesResponse(response)
	require.Len(t, out.Output, 4)
	require.Equal(t, "reasoning", out.Output[0].Type)
	require.NotEmpty(t, out.Output[0].EncryptedContent)
	require.Equal(t, "message", out.Output[1].Type)
	require.Equal(t, "reasoning", out.Output[2].Type)
	require.Equal(t, "function_call", out.Output[3].Type)
	input, err := json.Marshal(out.Output)
	require.NoError(t, err)
	input = append(input[:len(input)-1], []byte(`,{"type":"function_call_output","call_id":"call_1","output":"result"}]`)...)
	req, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: response.Model, Input: input})
	require.NoError(t, err)
	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(req.Messages[0].Content, &blocks))
	require.Equal(t, response.Content, blocks)
	require.Len(t, req.Messages, 2)
	var results []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(req.Messages[1].Content, &results))
	require.Equal(t, "tool_result", results[0].Type)
}

func TestHaiku55StreamingSignedHistory(t *testing.T) {
	state := NewAnthropicEventToResponsesState()
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_start", Message: &AnthropicResponse{Model: "claude-haiku-5-5"}}, state)
	blocks := []AnthropicContentBlock{{Type: "thinking", Signature: "signed-empty"}, {Type: "text", Text: "before"}, {Type: "redacted_thinking", Data: "opaque"}, {Type: "tool_use", ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)}}
	for i := range blocks {
		AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_start", Index: &i, ContentBlock: &blocks[i]}, state)
		if blocks[i].Type == "text" {
			AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_delta", Index: &i, Delta: &AnthropicDelta{Type: "text_delta", Text: blocks[i].Text}}, state)
		}
		AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_stop", Index: &i}, state)
	}
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state)
	require.True(t, state.PreserveThinkingSignatures)
	require.Len(t, state.Outputs, 4)
	require.NotEmpty(t, state.Outputs[0].EncryptedContent)
	require.NotEmpty(t, state.Outputs[2].EncryptedContent)
	require.Equal(t, "message", state.Outputs[1].Type)
	require.Equal(t, "function_call", state.Outputs[3].Type)
	raw, err := json.Marshal(state.Outputs)
	require.NoError(t, err)
	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(raw, &items))
	items = append(items, ResponsesInputItem{Type: "function_call_output", CallID: state.Outputs[3].CallID, Output: "ok"})
	raw, err = json.Marshal(items)
	require.NoError(t, err)
	converted, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude-haiku-5-5", Input: raw})
	require.NoError(t, err)
	require.Len(t, converted.Messages, 2)
	var replay []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(converted.Messages[0].Content, &replay))
	require.Len(t, replay, 4)
	require.Equal(t, blocks[0], replay[0])
	require.Equal(t, blocks[2], replay[2])
}

func TestHaiku55EnvelopeValidationAndForcedTools(t *testing.T) {
	for _, input := range []string{`[{"type":"reasoning","encrypted_content":"anthropic-thinking-v1:!"}]`, `[{"type":"reasoning","encrypted_content":"anthropic-thinking-v1:e30"}]`} {
		_, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude-haiku-5-5", Input: json.RawMessage(input)})
		require.Error(t, err)
	}
	for _, choice := range []string{`"required"`, `{"type":"function","name":"lookup"}`} {
		out, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude-haiku-5-5", Input: json.RawMessage(`"hi"`), ToolChoice: json.RawMessage(choice)})
		require.NoError(t, err)
		require.NotEmpty(t, out.ToolChoice)
	}
}
