package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const (
	toolNameMaxLen        = 63
	toolDescriptionMaxLen = 10000
	placeholderToolDesc   = "Tool used in conversation history"
)

func shortenToolName(name string) string {
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:8]
	prefixMax := toolNameMaxLen - 1 - 8
	prefix := name
	n := 0
	for i := range name {
		if n == prefixMax {
			prefix = name[:i]
			break
		}
		n++
	}
	return prefix + "_" + suffix
}

func mapToolName(name string, nameMap map[string]string) string {
	if name == "" || len(name) <= toolNameMaxLen {
		return name
	}
	if nameMap == nil {
		return shortenToolName(name)
	}
	short := shortenToolName(name)
	nameMap[short] = name
	return short
}

func applyToolNameMap(turns []turn, nameMap map[string]string) {
	if nameMap == nil {
		return
	}
	for i := range turns {
		for _, use := range turns[i].ToolUses {
			name, _ := use["name"].(string)
			if name == "" {
				continue
			}
			use["name"] = mapToolName(name, nameMap)
		}
	}
}

func restoreToolName(name string, nameMap map[string]string) string {
	if original, ok := nameMap[name]; ok && original != "" {
		return original
	}
	return name
}

func truncateRunesStrict(s string, max int) string {
	if max <= 0 || s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

func convertImage(source *apicompat.AnthropicImageSource) map[string]any {
	if source == nil || strings.TrimSpace(source.Data) == "" {
		return nil
	}
	format := imageFormat(source.MediaType)
	if format == "" {
		return nil
	}
	return map[string]any{
		"format": format,
		"source": map[string]any{"bytes": source.Data},
	}
}

func imageFormat(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return ""
	}
}

func pairToolTurns(turns []turn) []turn {
	unpaired := map[string]int{}
	for i, item := range turns {
		for _, use := range item.ToolUses {
			id, _ := use["toolUseId"].(string)
			if id != "" {
				unpaired[id] = i
			}
		}
	}
	for i := range turns {
		if len(turns[i].ToolResults) == 0 {
			continue
		}
		filtered := turns[i].ToolResults[:0]
		for _, result := range turns[i].ToolResults {
			id, _ := result["toolUseId"].(string)
			if _, ok := unpaired[id]; !ok {
				continue
			}
			filtered = append(filtered, result)
			delete(unpaired, id)
		}
		turns[i].ToolResults = filtered
	}
	if len(unpaired) == 0 {
		return turns
	}

	byTurn := map[int][]string{}
	for id, idx := range unpaired {
		byTurn[idx] = append(byTurn[idx], id)
	}
	indexes := make([]int, 0, len(byTurn))
	for idx := range byTurn {
		indexes = append(indexes, idx)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(indexes)))
	for _, idx := range indexes {
		ids := byTurn[idx]
		sort.Strings(ids)
		placeholders := makePlaceholderToolResults(ids)
		if idx+1 < len(turns) && turns[idx+1].Role == "user" {
			turns[idx+1].ToolResults = append(placeholders, turns[idx+1].ToolResults...)
			continue
		}
		extra := turn{Role: "user", Text: placeholder, ToolResults: placeholders}
		turns = append(turns[:idx+1], append([]turn{extra}, turns[idx+1:]...)...)
	}
	return turns
}

func makePlaceholderToolResults(ids []string) []map[string]any {
	results := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		results = append(results, map[string]any{
			"content":   []map[string]string{{"text": placeholder}},
			"status":    "success",
			"toolUseId": id,
		})
	}
	return results
}

func collectHistoryToolNames(turns []turn) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, item := range turns {
		for _, use := range item.ToolUses {
			name, _ := use["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			names = append(names, name)
		}
	}
	return names
}

func ensurePlaceholderTools(tools []any, historyNames []string) []any {
	defined := map[string]struct{}{}
	for _, raw := range tools {
		spec, _ := raw.(map[string]any)["toolSpecification"].(map[string]any)
		if spec == nil {
			continue
		}
		name, _ := spec["name"].(string)
		if name = strings.TrimSpace(name); name != "" {
			defined[strings.ToLower(name)] = struct{}{}
		}
	}
	for _, name := range historyNames {
		if _, ok := defined[strings.ToLower(name)]; ok {
			continue
		}
		defined[strings.ToLower(name)] = struct{}{}
		tools = append(tools, map[string]any{"toolSpecification": map[string]any{
			"name":        name,
			"description": placeholderToolDesc,
			"inputSchema": map[string]any{"json": map[string]any{
				"$schema":              "http://json-schema.org/draft-07/schema#",
				"type":                 "object",
				"properties":           map[string]any{},
				"required":             []any{},
				"additionalProperties": true,
			}},
		}})
	}
	return tools
}
