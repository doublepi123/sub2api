//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const movedClientSystem = "You are opencode, an agent.\nUse execute to call tools.opencode.session_move({ id }) to switch worktrees."

func forwardWithClientSystem(t *testing.T, protocol, model string) []byte {
	t.Helper()
	var request map[string]any
	switch protocol {
	case "responses":
		request = map[string]any{"model": model, "instructions": movedClientSystem, "input": "hi"}
	case "chat":
		request = map[string]any{"model": model, "messages": []map[string]any{{"role": "system", "content": movedClientSystem}, {"role": "user", "content": "hi"}}}
	case "messages":
		request = map[string]any{"model": model, "max_tokens": 64, "system": movedClientSystem, "messages": []map[string]any{{"role": "user", "content": "hi"}}}
	case "gemini":
		request = map[string]any{
			"contents":          []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hi"}}}},
			"systemInstruction": map[string]any{"parts": []map[string]any{{"text": movedClientSystem}}},
		}
	}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	var forwarded []byte
	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{antigravityCompatSuccessResponse()},
		onCall: func(req *http.Request, _ *queuedHTTPUpstreamStub) {
			forwarded, err = io.ReadAll(req.Body)
			require.NoError(t, err)
		},
	}
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/"+protocol, body)
	switch protocol {
	case "responses":
		_, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
	case "chat":
		_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
	case "messages":
		_, err = svc.Forward(context.Background(), c, account, body, false)
	case "gemini":
		_, err = svc.ForwardGemini(context.Background(), c, account, model, "generateContent", false, body, false)
	}
	require.NoError(t, err)
	require.NotEmpty(t, forwarded)
	return forwarded
}

func systemInstructionText(forwarded []byte) string {
	var texts []string
	for _, p := range gjson.GetBytes(forwarded, "request.systemInstruction.parts").Array() {
		texts = append(texts, p.Get("text").String())
	}
	return strings.Join(texts, "\n")
}

func TestAntigravityMovesClientSystemIntoContentsAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := map[string][]string{
		"gemini-3.1-pro-high": {"responses", "chat", "messages", "gemini"},
		"claude-sonnet-4-6":   {"responses", "chat", "messages"},
	}
	for model, protocols := range cases {
		for _, protocol := range protocols {
			t.Run(model+"/"+protocol, func(t *testing.T) {
				forwarded := forwardWithClientSystem(t, protocol, model)

				system := systemInstructionText(forwarded)
				require.Contains(t, system, "You are Antigravity")
				require.NotContains(t, system, "Use execute to call")
				require.NotContains(t, strings.ToLower(system), "session_move")

				contents := gjson.GetBytes(forwarded, "request.contents").Array()
				require.GreaterOrEqual(t, len(contents), 3)
				require.Equal(t, "user", contents[0].Get("role").String())
				instructions := contents[0].Get("parts.0.text").String()
				require.True(t, strings.HasPrefix(instructions, antigravity.SystemInstructionsTurnPrefix), instructions)
				require.Contains(t, instructions, "Use execute to call")
				require.Contains(t, instructions, "tools.opencode['session_move']", "moved text is still neutralized")
				require.NotContains(t, strings.ToLower(instructions), "you are opencode")
				require.Equal(t, "model", contents[1].Get("role").String())
				require.Equal(t, antigravity.SystemInstructionsAck, contents[1].Get("parts.0.text").String())
				require.Equal(t, "user", contents[2].Get("role").String())
				require.Equal(t, "hi", contents[2].Get("parts.0.text").String())
			})
		}
	}
}

func TestAntigravityKeepsClientSystemWhenMoveDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(antigravitySystemInContentsEnv, "off")
	for _, protocol := range []string{"chat", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			forwarded := forwardWithClientSystem(t, protocol, "gemini-3.1-pro-high")
			require.Contains(t, systemInstructionText(forwarded), "Use execute to call")
			require.Equal(t, "hi", gjson.GetBytes(forwarded, "request.contents.0.parts.0.text").String())
		})
	}
}
