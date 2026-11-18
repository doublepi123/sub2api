package service

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// antigravitySystemInContentsEnv turns off moving the client's system prompt into
// contents (default on). Antigravity rejects some ordinary systemInstruction text with
// a fake 429 while accepting the same text in contents; set it to 0/false/off/no to
// roll back to forwarding the client's system prompt in systemInstruction.
const antigravitySystemInContentsEnv = "SUB2API_ANTIGRAVITY_SYSTEM_IN_CONTENTS"

func antigravitySystemInContentsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(antigravitySystemInContentsEnv))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// prepareAntigravityGeminiSystem injects the gateway identity into a native Gemini
// request and, when enabled, moves the client's system text into contents.
func prepareAntigravityGeminiSystem(body []byte) ([]byte, error) {
	body, err := injectIdentityPatchToGeminiRequest(body)
	if err != nil || !antigravitySystemInContentsEnabled() {
		return body, err
	}
	return moveGeminiSystemInstructionToContents(body)
}

// moveGeminiSystemInstructionToContents keeps the gateway identity and any non-text
// parts in systemInstruction and sends every other text part as a leading
// user/model turn pair.
func moveGeminiSystemInstructionToContents(body []byte) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("解析 Gemini 请求失败: %w", err)
	}
	sysInst, ok := request["systemInstruction"].(map[string]any)
	if !ok {
		return body, nil
	}
	parts, _ := sysInst["parts"].([]any)
	identity := antigravity.GetDefaultIdentityPatch()
	kept := []any{map[string]any{"text": identity}}
	var texts []string
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			kept = append(kept, part)
			continue
		}
		text, isText := partMap["text"].(string)
		switch {
		case !isText:
			kept = append(kept, part)
		case text == identity:
		case strings.TrimSpace(text) != "":
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return body, nil
	}

	turns := make([]any, 0, 2)
	for _, turn := range antigravity.SystemInstructionTurns(strings.Join(texts, "\n\n")) {
		turnParts := make([]any, 0, len(turn.Parts))
		for _, p := range turn.Parts {
			turnParts = append(turnParts, map[string]any{"text": p.Text})
		}
		turns = append(turns, map[string]any{"role": turn.Role, "parts": turnParts})
	}
	contents, _ := request["contents"].([]any)
	request["contents"] = append(turns, contents...)
	sysInst["parts"] = kept
	return json.Marshal(request)
}
