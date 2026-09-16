package kiro

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestShortenToolNameIsDeterministicAndFitsLimit(t *testing.T) {
	name := strings.Repeat("a", 80)
	first := shortenToolName(name)
	second := shortenToolName(name)
	require.Equal(t, first, second)
	require.Len(t, first, toolNameMaxLen)
	require.NotEqual(t, first, shortenToolName(strings.Repeat("b", 80)))
}

func TestMapToolNameRecordsOriginal(t *testing.T) {
	nameMap := map[string]string{}
	short := mapToolName(strings.Repeat("tool", 20), nameMap)
	require.Len(t, short, toolNameMaxLen)
	require.Equal(t, strings.Repeat("tool", 20), nameMap[short])
	require.Equal(t, "lookup", mapToolName("lookup", nameMap))
	require.Equal(t, strings.Repeat("tool", 20), restoreToolName(short, nameMap))
}

func TestConvertImageSupportsCommonFormatsAndDropsUnknown(t *testing.T) {
	img := convertImage(&apicompat.AnthropicImageSource{Type: "base64", MediaType: "image/png", Data: "abc"})
	require.Equal(t, "png", img["format"])
	require.Equal(t, map[string]any{"bytes": "abc"}, img["source"])
	require.Nil(t, convertImage(&apicompat.AnthropicImageSource{MediaType: "image/svg+xml", Data: "abc"}))
	require.Nil(t, convertImage(&apicompat.AnthropicImageSource{MediaType: "image/png", Data: ""}))
}

func TestPairToolTurnsAddsPlaceholderAndDropsOrphans(t *testing.T) {
	turns := []turn{
		{Role: "assistant", Text: "call", ToolUses: []map[string]any{
			{"name": "lookup", "toolUseId": "tool_1", "input": map[string]any{}},
			{"name": "lookup", "toolUseId": "tool_2", "input": map[string]any{}},
		}},
		{Role: "user", Text: "next", ToolResults: []map[string]any{
			{"toolUseId": "tool_1", "status": "success", "content": []map[string]string{{"text": "ok"}}},
			{"toolUseId": "ghost", "status": "success", "content": []map[string]string{{"text": "nope"}}},
		}},
	}
	got := pairToolTurns(turns)
	require.Len(t, got, 2)
	require.Len(t, got[1].ToolResults, 2)
	require.Equal(t, "tool_2", got[1].ToolResults[0]["toolUseId"])
	content, ok := got[1].ToolResults[0]["content"].([]map[string]string)
	require.True(t, ok)
	require.NotEmpty(t, content)
	require.Equal(t, placeholder, content[0]["text"])
	require.Equal(t, "tool_1", got[1].ToolResults[1]["toolUseId"])
}

func TestPairToolTurnsInsertsUserTurnWhenNeeded(t *testing.T) {
	turns := []turn{
		{Role: "assistant", Text: "call", ToolUses: []map[string]any{
			{"name": "lookup", "toolUseId": "tool_1", "input": map[string]any{}},
		}},
		{Role: "assistant", Text: "more"},
	}
	got := pairToolTurns(turns)
	require.Len(t, got, 3)
	require.Equal(t, "user", got[1].Role)
	require.Equal(t, "tool_1", got[1].ToolResults[0]["toolUseId"])
}

func TestEnsurePlaceholderToolsAddsMissingHistoryTools(t *testing.T) {
	tools := []any{map[string]any{"toolSpecification": map[string]any{"name": "Lookup"}}}
	got := ensurePlaceholderTools(tools, []string{"lookup", "search"})
	require.Len(t, got, 2)
	spec := requireMap(t, requireMap(t, got[1])["toolSpecification"])
	require.Equal(t, "search", spec["name"])
	require.Equal(t, placeholderToolDesc, spec["description"])
}
