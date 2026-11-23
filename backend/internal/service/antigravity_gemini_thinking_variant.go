package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type antigravityThinkingLevelContextKey struct{}

// WithAntigravityThinkingLevelFromClaudeBody 从策略改写后的最终 Claude 请求体提取
// Antigravity 思考档位写入 ctx，确保混合分组调度冷却预检与实际转发命中同一变体。
func WithAntigravityThinkingLevelFromClaudeBody(ctx context.Context, body []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	// 未指定档位时不写入 ctx：由解析器按模型决定默认档位（Carbon 为 medium），
	// 与转发阶段保持一致。
	if level, ok := geminiThinkingLevelFromClaudeRequestIfSpecified(body, nil); ok {
		return context.WithValue(ctx, antigravityThinkingLevelContextKey{}, level)
	}
	return ctx
}

// WithAntigravityThinkingLevelFromGeminiBody 从 Gemini 原生请求体提取思考档位写入 ctx。
func WithAntigravityThinkingLevelFromGeminiBody(ctx context.Context, body []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if level, ok := geminiThinkingLevelFromBodyIfSpecified(body); ok {
		return context.WithValue(ctx, antigravityThinkingLevelContextKey{}, level)
	}
	return ctx
}

// AntigravityThinkingLevelFromContext 读取请求体解析出的有效 Antigravity 思考档位。
func AntigravityThinkingLevelFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	level, ok := ctx.Value(antigravityThinkingLevelContextKey{}).(string)
	return level, ok && level != ""
}

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

var geminiThinkingVariantSuffixes = []string{"-low", "-medium", "-high", "-xhigh", "-tiered"}

const (
	geminiThinkingBudgetLowMax    = 1024
	geminiThinkingBudgetMediumMax = 8192
)

type geminiThinkingConfigProbe struct {
	GenerationConfig      *geminiThinkingGenerationConfigProbe `json:"generationConfig"`
	GenerationConfigSnake *geminiThinkingGenerationConfigProbe `json:"generation_config"`
}

type geminiThinkingGenerationConfigProbe struct {
	ThinkingConfig      *geminiThinkingInnerProbe `json:"thinkingConfig"`
	ThinkingConfigSnake *geminiThinkingInnerProbe `json:"thinking_config"`
}

type geminiThinkingInnerProbe struct {
	ThinkingBudget      *json.Number `json:"thinkingBudget"`
	ThinkingBudgetSnake *json.Number `json:"thinking_budget"`
	ThinkingLevel       string       `json:"thinkingLevel"`
	ThinkingLevelSnake  string       `json:"thinking_level"`
}

// isAntigravityThinkingVariantFamily 判断模型是否属于 Antigravity 按思考深度拆分的模型族
// （gemini-* 以及 claude-opus-5-5* / claude-sonnet-5-5*）。
func isAntigravityThinkingVariantFamily(model string) bool {
	lower := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "models/")
	return strings.HasPrefix(lower, "gemini-") ||
		lower == "claude-opus-5-5" ||
		lower == "claude-sonnet-5-5" ||
		strings.HasPrefix(lower, "claude-opus-5-5-") ||
		strings.HasPrefix(lower, "claude-sonnet-5-5-")
}

// hasGeminiThinkingVariantSuffix 判断模型名是否已经带了思考深度后缀。
func hasGeminiThinkingVariantSuffix(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, suffix := range geminiThinkingVariantSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// antigravityRateLimitModelKey 在裸 Gemini 模型按请求思考档位落到自映射变体时返回实际转发变体，
// 避免模型级冷却将低档位失败误记到默认高档位，同时对无自映射目标的显式别名保留原名以免二次映射置空。
func antigravityRateLimitModelKey(account *Account, originalModel, mappedModel string) string {
	if mappedModel != "" && mappedModel != originalModel && hasGeminiThinkingVariantSuffix(mappedModel) && mapAntigravityModel(account, mappedModel) == mappedModel {
		return mappedModel
	}
	return originalModel
}

// effectiveAntigravityGeminiReasoningEffort 提取转发到 Antigravity 分档模型族时实际生效的思考档位，
// 仅在客户端显式指定了 effort 且目标属于分档模型族时返回（不给未传 effort 的请求虚构计费档位）。
func effectiveAntigravityGeminiReasoningEffort(mappedModel string, wrappedGeminiBody []byte, hadExplicitEffort bool) *string {
	if !hadExplicitEffort {
		return nil
	}
	actualModel := strings.TrimSpace(gjson.GetBytes(wrappedGeminiBody, "model").String())
	if actualModel == "" {
		actualModel = mappedModel
	}
	lowerModel := strings.TrimPrefix(strings.ToLower(actualModel), "models/")
	if !isAntigravityThinkingVariantFamily(lowerModel) {
		return nil
	}
	requestRaw := gjson.GetBytes(wrappedGeminiBody, "request").Raw
	if requestRaw == "" {
		requestRaw = string(wrappedGeminiBody)
	}
	if effort := extractGeminiReasoningEffortFromBody([]byte(requestRaw)); effort != nil {
		return effort
	}
	for _, level := range []string{"xhigh", "high", "medium", "low"} {
		if strings.HasSuffix(lowerModel, "-"+level) {
			resolved := level
			return &resolved
		}
	}
	return nil
}

// geminiThinkingLevelFromBody 从 Gemini 原生请求体推导期望的思考档位（low/medium/high/xhigh）。
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
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false
	}
	gc := probe.GenerationConfig
	if gc == nil {
		gc = probe.GenerationConfigSnake
	}
	if gc == nil {
		return "", false
	}
	tc := gc.ThinkingConfig
	if tc == nil {
		tc = gc.ThinkingConfigSnake
	}
	if tc == nil {
		return "", false
	}
	rawLevel := strings.TrimSpace(tc.ThinkingLevel)
	if rawLevel == "" {
		rawLevel = strings.TrimSpace(tc.ThinkingLevelSnake)
	}
	switch strings.ToLower(rawLevel) {
	case "minimal", "low":
		return "low", true
	case "medium":
		return "medium", true
	case "high":
		return "high", true
	case "xhigh":
		return "xhigh", true
	}
	rawBudget := tc.ThinkingBudget
	if rawBudget == nil {
		rawBudget = tc.ThinkingBudgetSnake
	}
	if rawBudget == nil {
		return "", false
	}
	budget, err := rawBudget.Float64()
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
// OpenAI reasoning effort and Anthropic output_config.effort. xhigh/max select
// the -xhigh variant where the account has one (Gemini 4 Hydrogen and Neon); the
// resolver falls back to -high for families without it.
func geminiThinkingLevelFromReasoningEffort(effort *string) (string, bool) {
	if effort == nil {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(*effort)) {
	case "none", "minimal", "low":
		return "low", true
	case "medium":
		return "medium", true
	case "high":
		return "high", true
	case "xhigh", "max":
		return "xhigh", true
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

// antigravityHydrogenModelBase 是 Antigravity 上 Hydrogen 的模型族前缀：
// 上游只登记 -low/-medium/-high/-xhigh 四档后缀，裸名同样只是客户端别名。
const (
	antigravityHydrogenModelBase = "gemini-4-hydrogen"
	antigravityNeonModelBase     = "gemini-4-neon"
)

// isAntigravityLevelOnlyGeminiModel 判断模型名（含 models/ 前缀与档位后缀）是否属于
// 只接受 thinkingLevel 的 Carbon / Hydrogen / Neon 族。
func isAntigravityLevelOnlyGeminiModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(model, "models/")))
	switch {
	case strings.HasPrefix(name, antigravityHydrogenModelBase):
		rest := strings.TrimPrefix(name, antigravityHydrogenModelBase)
		return rest == "" || rest == "-low" || rest == "-medium" || rest == "-high" || rest == "-xhigh"
	case strings.HasPrefix(name, antigravityNeonModelBase):
		rest := strings.TrimPrefix(name, antigravityNeonModelBase)
		return rest == "" || rest == "-low" || rest == "-medium" || rest == "-high" || rest == "-xhigh"
	case strings.HasPrefix(name, antigravityCarbonModelBase):
		rest := strings.TrimPrefix(name, antigravityCarbonModelBase)
		return rest == "" || rest == "-low" || rest == "-medium" || rest == "-high"
	}
	return false
}

// normalizeCarbonNativeThinking 归一 Carbon / Hydrogen / Neon 原生请求里的 legacy thinkingBudget：
// 这些模型族只接受 thinkingLevel，带 thinkingBudget 的请求可能被上游拒绝，
// 因此删除 thinkingBudget，并在裸名（尚未落到后缀）时把它换算成 thinkingLevel。
// 已经带后缀的请求由后缀决定档位，只删除 budget，不再写入 level。
func normalizeCarbonNativeThinking(body []byte, mappedModel string) []byte {
	if len(body) == 0 || !isAntigravityLevelOnlyGeminiModel(mappedModel) {
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
	level, _ := geminiThinkingLevelFromBodyIfSpecified(body)
	return resolveGeminiThinkingVariantForLevel(account, requestedModel, level)
}

func resolveGeminiThinkingVariantForLevel(account *Account, requestedModel string, preferred string) (string, bool) {
	if account == nil {
		return "", false
	}
	model := strings.TrimPrefix(strings.TrimSpace(requestedModel), "models/")
	if !isAntigravityThinkingVariantFamily(model) || hasGeminiThinkingVariantSuffix(model) {
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
	// Carbon 的裸名按既有设计映射到自身，后缀才是上游身份；它的默认档位是
	// 上游的 medium（defaultAgentModelId=gemini-4-carbon-medium）。只在客户端
	// 没有指定档位、且模型是裸 Carbon 时改用 medium，其它 Gemini 仍按 high。
	mapped, modelMatched := resolveRequestedModelInMapping(mapping, model)
	if modelMatched && strings.TrimSpace(mapped) != model {
		return "", false
	}

	if preferred == "" {
		if model == antigravityCarbonModelBase {
			preferred = "medium"
		} else {
			preferred = "high"
		}
	}
	order := []string{preferred}
	for _, level := range []string{"high", "medium", "low", "xhigh", "tiered"} {
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
	// 当账号显式添加了裸动态分档自映射（如前端点击 Opus 5.5 / Sonnet 5.5 / 4 Hydrogen 预设
	// 产生 base -> base），但未单独逐条录入后缀变体时，回退到官方默认分档目录避免把裸名直接发往上游 404。
	if modelMatched && strings.TrimSpace(mapped) == model {
		for _, level := range order {
			if defaultTarget, ok := domain.DefaultAntigravityModelMapping[model+"-"+level]; ok && strings.TrimSpace(defaultTarget) != "" {
				return defaultTarget, true
			}
		}
	}
	return "", false
}
