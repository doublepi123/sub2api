package service

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Gemini 原生请求（/v1beta/models/{model}:generateContent 等）经 Antigravity 账号转发时，
// 客户端（如 Antigravity CLI / go-genai）习惯发"裸"模型名（gemini-3.8-flash）并用
// generationConfig.thinkingConfig 表达思考深度；而 Antigravity 上游的模型目录只有
// gemini-3.8-flash-low / -medium / -high / -tiered 这类带后缀的变体，裸名直接转发会被上游
// 以 404 "Requested entity was not found." 拒绝。
//
// 这里在账号 model_mapping 没有为裸名配置显式条目、但配置了它的后缀变体时，
// 按 thinkingConfig 自动挑一个变体：
//   - thinkingLevel: "low" / "medium" / "high" → 同名后缀
//   - thinkingBudget: -1（动态）→ high；0 → low；1..1024 → low；1025..8192 → medium；>8192 → high
//   - 未携带 thinkingConfig → high（与 Gemini 3 系列默认开启动态思考一致；
//     Carbon 例外：默认档位是上游的 medium）
// 选中的后缀在映射表里不存在时按 high → medium → low → tiered 的顺序降级到存在的变体。
// 显式映射（裸名映射到其他模型）始终优先；裸名自映射仅在存在精确变体键时继续推导。

var geminiThinkingVariantSuffixes = []string{"-low", "-medium", "-high", "-tiered"}

const (
	geminiThinkingBudgetLowMax    = 1024
	geminiThinkingBudgetMediumMax = 8192
)

type geminiThinkingConfigProbe struct {
	GenerationConfig struct {
		ThinkingConfig *struct {
			ThinkingBudget *json.Number `json:"thinkingBudget"`
			ThinkingLevel  string       `json:"thinkingLevel"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
}

// hasGeminiThinkingVariantSuffix 判断模型名是否已经带了思考深度后缀。
func hasGeminiThinkingVariantSuffix(model string) bool {
	for _, suffix := range geminiThinkingVariantSuffixes {
		if strings.HasSuffix(model, suffix) {
			return true
		}
	}
	return false
}

// geminiThinkingLevelFromBody 从 Gemini 原生请求体推导期望的思考档位（low/medium/high）。
// 解析失败或未携带 thinkingConfig 时返回 "high"。
func geminiThinkingLevelFromBody(body []byte) string {
	if level, ok := geminiThinkingLevelFromBodyIfSpecified(body); ok {
		return level
	}
	return "high"
}

// geminiThinkingLevelFromBodyIfSpecified 与 geminiThinkingLevelFromBody 同源，但把
// "客户端没有指定档位"与"显式 high"区分开：Antigravity 的 Carbon 默认档位是
// medium（上游 defaultAgentModelId=gemini-4-carbon-medium），默认值必须由模型决定，
// 不能统一按 high 兜底。
func geminiThinkingLevelFromBodyIfSpecified(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	var probe geminiThinkingConfigProbe
	if err := json.Unmarshal(body, &probe); err != nil || probe.GenerationConfig.ThinkingConfig == nil {
		return "", false
	}
	tc := probe.GenerationConfig.ThinkingConfig
	switch strings.ToLower(strings.TrimSpace(tc.ThinkingLevel)) {
	case "low":
		return "low", true
	case "medium":
		return "medium", true
	case "high":
		return "high", true
	}
	if tc.ThinkingBudget == nil {
		return "", false
	}
	budget, err := tc.ThinkingBudget.Float64()
	if err != nil {
		return "", false
	}
	switch {
	case budget < 0:
		return "high", true // -1 = 动态思考
	case budget <= geminiThinkingBudgetLowMax:
		return "low", true // 含 0（关闭思考）：上游没有"无思考"变体，取最浅档
	case budget <= geminiThinkingBudgetMediumMax:
		return "medium", true
	default:
		return "high", true
	}
}

// geminiThinkingLevelFromClaudeThinking 用 Claude Messages 协议的 thinking 配置推导档位，
// 阈值与 geminiThinkingLevelFromBody 保持一致，使同一请求无论走 Gemini 原生还是
// Chat Completions / Messages 兼容层都落到同一个上游变体。
func geminiThinkingLevelFromClaudeThinking(thinking *antigravity.ThinkingConfig) string {
	if thinking == nil {
		return "high"
	}
	if strings.EqualFold(strings.TrimSpace(thinking.Type), "disabled") {
		return "low"
	}
	budget := thinking.BudgetTokens
	switch {
	case budget <= 0:
		return "high" // 动态思考 / 未指定预算
	case budget <= geminiThinkingBudgetLowMax:
		return "low"
	case budget <= geminiThinkingBudgetMediumMax:
		return "medium"
	default:
		return "high"
	}
}

// geminiThinkingLevelFromReasoningEffort uses the same variant scale for
// OpenAI reasoning effort and Anthropic output_config.effort.
func geminiThinkingLevelFromReasoningEffort(effort *string) (string, bool) {
	if effort == nil {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(*effort)) {
	case "none", "minimal", "low":
		return "low", true
	case "medium":
		return "medium", true
	case "high", "xhigh", "max":
		return "high", true
	default:
		return "", false
	}
}

// Read the original client body: protocol conversion can synthesize a budget
// (or omit thinking for low), neither of which is an explicit client budget.
// Keep disabled/budget semantics, then honor effort before defaulting to high.
func geminiThinkingLevelFromClaudeRequest(body []byte, reasoningEffort *string) string {
	if level, ok := geminiThinkingLevelFromClaudeRequestIfSpecified(body, reasoningEffort); ok {
		return level
	}
	return "high"
}

// geminiThinkingLevelFromClaudeRequestIfSpecified mirrors
// geminiThinkingLevelFromClaudeRequest but reports whether the client actually
// pinned a level, so Carbon can fall back to its published medium default.
func geminiThinkingLevelFromClaudeRequestIfSpecified(body []byte, reasoningEffort *string) (string, bool) {
	typ := gjson.GetBytes(body, "thinking.type").String()
	if strings.EqualFold(strings.TrimSpace(typ), "disabled") {
		return "low", true
	}
	if budget := gjson.GetBytes(body, "thinking.budget_tokens"); budget.Exists() {
		return geminiThinkingLevelFromClaudeThinking(&antigravity.ThinkingConfig{
			Type: typ, BudgetTokens: int(budget.Int()),
		}), true
	}
	effort := gjson.GetBytes(body, "output_config.effort").String()
	if level, ok := geminiThinkingLevelFromReasoningEffort(&effort); ok {
		return level, true
	}
	if level, ok := geminiThinkingLevelFromReasoningEffort(reasoningEffort); ok {
		return level, true
	}
	return "", false
}

// antigravityCarbonModelBase 是 Antigravity 上 Carbon 的模型族前缀。
// 上游 fetchAvailableModels 只登记 -low/-medium/-high 三档后缀，没有裸名；
// 裸 gemini-4-carbon 只作为客户端别名（默认落到 medium）。
const antigravityCarbonModelBase = "gemini-4-carbon"

// isAntigravityCarbonModel 判断模型名（含 models/ 前缀与三档后缀）是否属于 Carbon 族。
func isAntigravityCarbonModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(model, "models/")))
	if !strings.HasPrefix(name, antigravityCarbonModelBase) {
		return false
	}
	rest := strings.TrimPrefix(name, antigravityCarbonModelBase)
	return rest == "" || rest == "-low" || rest == "-medium" || rest == "-high"
}

// antigravityCarbonDefaultLevel 识别“裸名映射到自身某一档后缀”的默认映射，
// 例如 gemini-4-carbon → gemini-4-carbon-medium。这种映射表达的是默认档位，
// 不是指向其它模型的显式别名，客户端显式请求 low/high 时仍应继续在后缀里挑选。
func antigravityCarbonDefaultLevel(model, mapped string) (string, bool) {
	if !isAntigravityCarbonModel(model) || model != antigravityCarbonModelBase {
		return "", false
	}
	for _, level := range []string{"low", "medium", "high"} {
		if mapped == model+"-"+level {
			return level, true
		}
	}
	return "", false
}

// normalizeCarbonNativeThinking 归一 Carbon 原生请求里的 legacy thinkingBudget：
// Antigravity 的 Carbon 只接受 thinkingLevel，带 thinkingBudget 的请求可能被上游拒绝，
// 因此删除 thinkingBudget，并在裸名（尚未落到后缀）时把它换算成 thinkingLevel。
// 已经带后缀的请求由后缀决定档位，只删除 budget，不再写入 level。
func normalizeCarbonNativeThinking(body []byte, mappedModel string) []byte {
	if len(body) == 0 || !isAntigravityCarbonModel(mappedModel) {
		return body
	}
	const thinkingConfigPath = "generationConfig.thinkingConfig"
	thinkingConfig := gjson.GetBytes(body, thinkingConfigPath)
	if !thinkingConfig.Exists() || !thinkingConfig.IsObject() {
		return body
	}
	if !thinkingConfig.Get("thinkingBudget").Exists() {
		return body
	}
	level := strings.ToLower(strings.TrimSpace(thinkingConfig.Get("thinkingLevel").String()))
	out, err := sjson.DeleteBytes(body, thinkingConfigPath+".thinkingBudget")
	if err != nil {
		return body
	}
	if level == "" && !hasGeminiThinkingVariantSuffix(strings.ToLower(strings.TrimPrefix(mappedModel, "models/"))) {
		if derived, ok := geminiThinkingLevelFromBodyIfSpecified(body); ok {
			if updated, err := sjson.SetBytes(out, thinkingConfigPath+".thinkingLevel", derived); err == nil {
				out = updated
			}
		}
	}
	return out
}

// resolveGeminiThinkingVariant 为裸 Gemini 模型名挑选账号映射表里存在的思考深度变体。
// 返回 (映射后的上游模型名, 是否命中)。未命中时调用方应回退到常规 getMappedModel 流程。
func resolveGeminiThinkingVariant(account *Account, requestedModel string, body []byte) (string, bool) {
	return resolveGeminiThinkingVariantForLevel(account, requestedModel, geminiThinkingLevelFromBody(body))
}

// resolveGeminiThinkingVariantForLevel 是 resolveGeminiThinkingVariant 的协议无关内核：
// 调用方负责按自身协议推导 preferred 档位（Gemini 原生读 generationConfig.thinkingConfig，
// Claude/OpenAI 兼容层读 thinking.budget_tokens），此处只做映射查找与降级。
func resolveGeminiThinkingVariantForLevel(account *Account, requestedModel string, preferred string) (string, bool) {
	if account == nil {
		return "", false
	}
	model := strings.TrimSpace(strings.TrimPrefix(requestedModel, "models/"))
	if !strings.HasPrefix(model, "gemini-") || hasGeminiThinkingVariantSuffix(model) {
		return "", false
	}
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		return "", false
	}
	// 裸名本身若已映射到其他具体模型（如 gemini-3.8-flash → gemini-3.8-flash-tiered、
	// gemini-3.1-pro → gemini-pro-agent），尊重显式别名配置。
	// 若裸名映射目标是自己（如前端从 DefaultAntigravityModelMapping 拉取并持久化的
	// gemini-3.8-flash → gemini-3.8-flash，或 ensureAntigravityDefaultPassthroughs 注入的条目），
	// 当映射表中存在该模型的精确思考变体（-high/-medium/-low/-tiered）时继续解析变体，
	// 避免把上游不存在的裸名直接发出导致 404；而对已在映射表中的真实裸模型（如 gemini-2.5-flash），
	// 仅查找精确变体键以避免被通配符误匹配。
	// Carbon 的默认映射是“裸名 → 自身某一档后缀”（medium），它表达默认档位而不是显式别名，
	// 因此同样继续解析变体，并在客户端未指定档位时使用该默认档位。
	mapped, modelMatched := resolveRequestedModelInMapping(mapping, model)
	carbonDefaultLevel := ""
	if modelMatched && strings.TrimSpace(mapped) != model {
		level, ok := antigravityCarbonDefaultLevel(model, strings.TrimSpace(mapped))
		if !ok {
			return "", false
		}
		carbonDefaultLevel = level
	}

	if preferred == "" {
		switch {
		case carbonDefaultLevel != "":
			preferred = carbonDefaultLevel
		case model == antigravityCarbonModelBase:
			// 裸 Carbon 的默认档位是 medium（上游 defaultAgentModelId），
			// 与是否有显式 bare 映射无关。
			preferred = "medium"
		default:
			preferred = "high"
		}
	}
	order := []string{preferred}
	for _, level := range []string{"high", "medium", "low", "tiered"} {
		if level != preferred {
			order = append(order, level)
		}
	}
	for _, level := range order {
		candidate := model + "-" + level
		var (
			variantMapped  string
			variantMatched bool
		)
		if modelMatched {
			variantMapped, variantMatched = mapping[candidate]
		} else {
			variantMapped, variantMatched = resolveRequestedModelInMapping(mapping, candidate)
		}
		if variantMatched && strings.TrimSpace(variantMapped) != "" {
			return variantMapped, true
		}
	}
	return "", false
}
