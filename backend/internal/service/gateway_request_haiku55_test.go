//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// issue #7919：claude-haiku-5-5 请求校验（官方 migration guide 口径）。
func TestValidateClaude55Request_Haiku55(t *testing.T) {
	const model = "claude-haiku-5-5"
	const prefix = `{"model":"claude-haiku-5-5","max_tokens":16000,"messages":[{"role":"user","content":"hi"}]`
	cases := []struct {
		name string
		// extra 为追加进最小合法请求的 JSON 片段（含前导逗号）
		extra   string
		wantErr string // 为空表示期望通过
	}{
		{name: "adaptive thinking passes", extra: `,"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}`},
		{name: "omitted thinking passes", extra: ``},
		{name: "enabled thinking rejected", extra: `,"thinking":{"type":"enabled","budget_tokens":8000}`, wantErr: "requires adaptive thinking"},
		{name: "disabled thinking allowed at medium effort", extra: `,"thinking":{"type":"disabled"},"output_config":{"effort":"medium"}`},
		{name: "disabled thinking allowed when effort omitted", extra: `,"thinking":{"type":"disabled"}`},
		{name: "disabled thinking rejected at xhigh", extra: `,"thinking":{"type":"disabled"},"output_config":{"effort":"xhigh"}`, wantErr: "supports only low, medium or high effort"},
		{name: "disabled thinking rejected at max", extra: `,"thinking":{"type":"disabled"},"output_config":{"effort":"max"}`, wantErr: "supports only low, medium or high effort"},
		{name: "temperature 1 passes", extra: `,"temperature":1`},
		{name: "non-default temperature rejected", extra: `,"temperature":0.5`, wantErr: "non-default temperature"},
		{name: "top_p 0.99 passes", extra: `,"top_p":0.99`},
		{name: "top_p 1 rejected", extra: `,"top_p":1`, wantErr: "non-default top_p"},
		{name: "between tools rejected", extra: `,"thinking":{"type":"between_tools"}`, wantErr: "claude-haiku-5-5"},
		{name: "adaptive budget rejected", extra: `,"thinking":{"type":"adaptive","budget_tokens":1000}`, wantErr: "budget_tokens"},
		{name: "disabled block binding rejected", extra: `,"thinking":{"type":"disabled","block_binding":"relaxed"}`, wantErr: "block_binding"},
		{name: "disabled low allowed", extra: `,"thinking":{"type":"disabled"},"output_config":{"effort":"low"}`},
		{name: "disabled high allowed", extra: `,"thinking":{"type":"disabled"},"output_config":{"effort":"high"}`},
		{name: "adaptive summarized allowed", extra: `,"thinking":{"type":"adaptive","display":"summarized"}`},
		{name: "named tool choice allowed", extra: `,"tool_choice":{"type":"tool","name":"lookup"}`},
		{name: "top_p 0.5 rejected", extra: `,"top_p":0.5`, wantErr: "non-default top_p"},
		{name: "top_k rejected", extra: `,"top_k":5`, wantErr: "does not support top_k"},
		{name: "temperature and top_p together rejected", extra: `,"temperature":1,"top_p":0.99`, wantErr: "does not allow temperature and top_p together"},
		{name: "forced tool_choice any allowed", extra: `,"tool_choice":{"type":"any"}`},
		{name: "forced tool_choice required allowed", extra: `,"tool_choice":"required"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(prefix + tc.extra + "}")
			err := validateClaude55Request(body, model)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.True(t, strings.Contains(err.Error(), tc.wantErr),
				"error %q should contain %q", err.Error(), tc.wantErr)
		})
	}
}

func TestHaiku55RejectsAssistantPrefill(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"prefix"}]}`)
	require.ErrorContains(t, validateClaude55Request(body, "claude-haiku-5-5"), "prefill")
	require.NoError(t, validateClaude55Request(body, "claude-haiku-4-5-20251001"))
}

func TestHaiku55OAuthPreservesDisabledSummarizedAndForcedToolChoice(t *testing.T) {
	for _, choice := range []string{`{"type":"any"}`, `{"type":"tool","name":"sessions_list"}`} {
		body := []byte(`{"model":"claude-haiku-5-5","max_tokens":512,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"sessions_list","input_schema":{"type":"object"}}],"thinking":{"type":"disabled"},"output_config":{"effort":"low"},"tool_choice":` + choice + `}`)
		require.NoError(t, validateClaude55Request(body, "claude-haiku-5-5"))
		out, _ := normalizeClaudeOAuthRequestBody(body, "claude-haiku-5-5", claudeOAuthNormalizeOptions{})
		require.Equal(t, "disabled", gjson.GetBytes(out, "thinking.type").String())
		require.JSONEq(t, choice, gjson.GetBytes(out, "tool_choice").Raw)
		rw := buildToolNameRewriteFromBody(out)
		out = applyToolNameRewriteToBody(out, rw)
		if gjson.GetBytes(out, "tool_choice.type").String() == "tool" {
			require.Equal(t, gjson.GetBytes(out, "tools.0.name").String(), gjson.GetBytes(out, "tool_choice.name").String())
		}
	}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive","display":"summarized"},"top_p":0.99}`)
	out, _ := normalizeClaudeOAuthRequestBody(body, "claude-haiku-5-5", claudeOAuthNormalizeOptions{})
	require.Equal(t, "summarized", gjson.GetBytes(out, "thinking.display").String())
	require.False(t, gjson.GetBytes(out, "temperature").Exists())
}
