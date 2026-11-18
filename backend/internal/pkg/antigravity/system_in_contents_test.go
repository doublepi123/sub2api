//go:build unit

package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTransformClaudeToGemini_MoveSystemToContents(t *testing.T) {
	system, err := json.Marshal("You are Antigravity, but this is the client's own prompt.\nUse the tools carefully.")
	require.NoError(t, err)
	req := &ClaudeRequest{
		System: system,
		Messages: []ClaudeMessage{
			{Role: "system", Content: json.RawMessage(`"Extra system note."`)},
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	off := DefaultTransformOptions()
	on := DefaultTransformOptions()
	on.MoveSystemToContents = true

	offBody, err := TransformClaudeToGeminiWithOptions(req, "project", "gemini-3-flash", off)
	require.NoError(t, err)
	onBody, err := TransformClaudeToGeminiWithOptions(req, "project", "gemini-3-flash", on)
	require.NoError(t, err)

	var offReq, onReq V1InternalRequest
	require.NoError(t, json.Unmarshal(offBody, &offReq))
	require.NoError(t, json.Unmarshal(onBody, &onReq))

	require.Equal(t, offReq.Request.SessionID, onReq.Request.SessionID, "session id comes from the original conversation")

	var systemText string
	for _, p := range onReq.Request.SystemInstruction.Parts {
		systemText += p.Text
	}
	require.Contains(t, systemText, GetDefaultIdentityPatch()[:40], "gateway identity is injected even when the client claims Antigravity")
	require.NotContains(t, systemText, "Use the tools carefully.")
	require.NotContains(t, systemText, "Extra system note.")

	require.Len(t, onReq.Request.Contents, len(offReq.Request.Contents)+2)
	first := onReq.Request.Contents[0]
	require.Equal(t, "user", first.Role)
	require.Contains(t, first.Parts[0].Text, SystemInstructionsTurnPrefix)
	require.Contains(t, first.Parts[0].Text, "Use the tools carefully.")
	require.Contains(t, first.Parts[0].Text, "Extra system note.")
	require.Equal(t, "model", onReq.Request.Contents[1].Role)
	require.Equal(t, SystemInstructionsAck, onReq.Request.Contents[1].Parts[0].Text)
	require.Equal(t, offReq.Request.Contents, onReq.Request.Contents[2:])
}
