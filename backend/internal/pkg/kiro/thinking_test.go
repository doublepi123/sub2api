package kiro

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestGenerateThinkingPrefixEnabledAndAdaptive(t *testing.T) {
	require.Equal(t,
		"<thinking_mode>enabled</thinking_mode><max_thinking_length>8000</max_thinking_length>",
		generateThinkingPrefix(apicompat.AnthropicRequest{Thinking: &apicompat.AnthropicThinking{Type: "enabled", BudgetTokens: 8000}}),
	)
	require.Equal(t,
		"<thinking_mode>adaptive</thinking_mode><thinking_effort>high</thinking_effort>",
		generateThinkingPrefix(apicompat.AnthropicRequest{Thinking: &apicompat.AnthropicThinking{Type: "adaptive"}}),
	)
	require.Equal(t,
		"<thinking_mode>adaptive</thinking_mode><thinking_effort>medium</thinking_effort>",
		generateThinkingPrefix(apicompat.AnthropicRequest{
			Thinking:     &apicompat.AnthropicThinking{Type: "adaptive"},
			OutputConfig: &apicompat.AnthropicOutputConfig{Effort: "medium"},
		}),
	)
	require.Empty(t, generateThinkingPrefix(apicompat.AnthropicRequest{}))
	require.Empty(t, generateThinkingPrefix(apicompat.AnthropicRequest{Thinking: &apicompat.AnthropicThinking{Type: "disabled"}}))
}

func TestHasThinkingTags(t *testing.T) {
	require.True(t, hasThinkingTags("<thinking_mode>enabled</thinking_mode>"))
	require.True(t, hasThinkingTags("x<max_thinking_length>1</max_thinking_length>"))
	require.False(t, hasThinkingTags("plain system prompt"))
}

func TestThinkingParserSplitsQuotedTagsAndLeadingBlank(t *testing.T) {
	events := collectThinkingEvents("\n\n<thinking>\nfirst thought", "`</thinking>` still thinking</thinking>\n\nvisible")
	require.Equal(t, "first thought`</thinking>` still thinking", concatDelta(events))
	require.Equal(t, "visible", lastText(events))
	require.Contains(t, eventKinds(events), thinkingEventStart)
	require.Contains(t, eventKinds(events), thinkingEventStop)
}

func TestThinkingParserHoldsPartialEndTagAcrossChunks(t *testing.T) {
	var p thinkingParser
	_ = p.push("<thinking>abc")
	mid := p.push("</think")
	require.Empty(t, concatDelta(mid), "partial end tag must stay buffered")

	events := append(mid, p.push("ing>\n\nafter")...)
	require.Equal(t, "abc", concatDelta(events))
	require.Equal(t, "after", lastText(events))
}

func TestThinkingParserBoundaryClosesWithoutDoubleNewline(t *testing.T) {
	events := collectThinkingEvents("<thinking>plan</thinking>")
	require.Equal(t, "plan", concatDelta(events))
	require.Contains(t, eventKinds(events), thinkingEventStart)
	require.Contains(t, eventKinds(events), thinkingEventStop)
}

func TestThinkingParserSkipsQuotedStartTag(t *testing.T) {
	events := collectThinkingEvents("see `<thinking>` mentioned")
	require.Equal(t, []thinkingEventKind{thinkingEventText, thinkingEventText}, eventKinds(events))
	require.Equal(t, "see `<thinking>` mentioned", concatText(events))
}

func TestWrapAssistantThinking(t *testing.T) {
	require.Equal(t, "<thinking>plan</thinking>\n\nanswer", wrapAssistantThinking("plan", "answer", false))
	require.Equal(t, "<thinking>plan</thinking>", wrapAssistantThinking("plan", "", true))
	require.Equal(t, " ", wrapAssistantThinking("", "", true))
	require.Equal(t, "answer", wrapAssistantThinking("", "answer", false))
}

func TestFindCharBoundaryDoesNotSplitUTF8(t *testing.T) {
	s := "你好"
	pos := findCharBoundary(s, 1)
	require.Equal(t, 0, pos)
	require.True(t, strings.HasPrefix(s, s[:pos]))
}

func collectThinkingEvents(chunks ...string) []thinkingEvent {
	var p thinkingParser
	var events []thinkingEvent
	for _, chunk := range chunks {
		events = append(events, p.push(chunk)...)
	}
	return append(events, p.flushBoundary()...)
}

func eventKinds(events []thinkingEvent) []thinkingEventKind {
	got := make([]thinkingEventKind, len(events))
	for i, e := range events {
		got[i] = e.kind
	}
	return got
}

func concatDelta(events []thinkingEvent) string {
	var b strings.Builder
	for _, e := range events {
		if e.kind == thinkingEventDelta {
			_, _ = b.WriteString(e.text)
		}
	}
	return b.String()
}

func lastText(events []thinkingEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].kind == thinkingEventText {
			return events[i].text
		}
	}
	return ""
}

func concatText(events []thinkingEvent) string {
	var b strings.Builder
	for _, e := range events {
		if e.kind == thinkingEventText {
			_, _ = b.WriteString(e.text)
		}
	}
	return b.String()
}
