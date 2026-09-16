package antigravity

import (
	"errors"
	"testing"
)

func TestTransformGeminiToClaudeRejectsMalformedFunctionCall(t *testing.T) {
	body := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"","thoughtSignature":"sig-empty"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}}`)

	response, usage, err := TransformGeminiToClaude(body, "gemini-3.7-flash")

	if !errors.Is(err, ErrMalformedFunctionCall) {
		t.Fatalf("expected ErrMalformedFunctionCall, got %v", err)
	}
	if response != nil {
		t.Fatalf("malformed call must not produce a successful response: %s", response)
	}
	if usage != nil {
		t.Fatalf("malformed call must not produce billable success usage: %#v", usage)
	}
}

func TestStreamingProcessorRejectsMalformedFunctionCallBeforeEmittingEvents(t *testing.T) {
	processor := NewStreamingProcessor("gemini-3.7-flash")
	line := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"","thoughtSignature":"sig-empty"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}}`

	events := processor.ProcessLine(line)
	finalEvents, _ := processor.Finish()

	if len(events) != 0 || len(finalEvents) != 0 {
		t.Fatalf("malformed call must not be converted to a successful terminal stream: events=%q final=%q", events, finalEvents)
	}
	if !processor.MalformedFunctionCall() {
		t.Fatal("processor did not preserve malformed function call state")
	}
}
