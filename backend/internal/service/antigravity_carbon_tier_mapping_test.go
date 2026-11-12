package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Antigravity 的 fetchAvailableModels 对 Carbon 只登记三档后缀
// （gemini-4-carbon-low / -medium / -high），没有裸名；上游自己的
// defaultAgentModelId 就是 gemini-4-carbon-medium。默认映射沿用现有设计
// “裸名 → 自身 + 三档后缀 → 自身”，由 dynamic helper 在客户端未指定档位时
// 为裸 Carbon 选 medium，显式 low/high 时选对应后缀。
func TestAntigravityCarbonBareRequestDefaultsToMedium(t *testing.T) {
	svc := &AntigravityGatewayService{}

	t.Run("default catalog: bare request falls back to medium", func(t *testing.T) {
		account := newAntigravityAccountWithMapping(map[string]string{})
		require.Equal(t, domain.DefaultAntigravityModelMapping, account.GetModelMapping())
		require.Equal(t,
			"gemini-4-carbon-medium",
			svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", ""))
	})

	t.Run("default catalog: explicit levels pick their real suffix", func(t *testing.T) {
		account := newAntigravityAccountWithMapping(map[string]string{})
		for _, level := range []string{"low", "medium", "high"} {
			require.Equal(t,
				"gemini-4-carbon-"+level,
				svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", level))
		}
	})

	t.Run("persisted default mapping keeps bare self-mapping", func(t *testing.T) {
		account := newAntigravityAccountWithMapping(map[string]string{
			"gemini-4-carbon":        "gemini-4-carbon",
			"gemini-4-carbon-low":    "gemini-4-carbon-low",
			"gemini-4-carbon-medium": "gemini-4-carbon-medium",
			"gemini-4-carbon-high":   "gemini-4-carbon-high",
		})
		require.Equal(t,
			"gemini-4-carbon-medium",
			svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", ""))
		require.Equal(t,
			"gemini-4-carbon-high",
			svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", "high"))
		require.Equal(t,
			"gemini-4-carbon-low",
			svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", "low"))
	})

	t.Run("administrator pinned suffix alias is respected", func(t *testing.T) {
		account := newAntigravityAccountWithMapping(map[string]string{
			"gemini-4-carbon":        "gemini-4-carbon-high",
			"gemini-4-carbon-low":    "gemini-4-carbon-low",
			"gemini-4-carbon-medium": "gemini-4-carbon-medium",
			"gemini-4-carbon-high":   "gemini-4-carbon-high",
		})
		// 显式别名优先：即使客户端请求 low，也保持管理员固定的 high。
		for _, level := range []string{"", "low", "high"} {
			require.Equal(t,
				"gemini-4-carbon-high",
				svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", level))
		}
	})

	t.Run("other Gemini models still default to high", func(t *testing.T) {
		account := newAntigravityAccountWithMapping(map[string]string{})
		require.Equal(t,
			"gemini-3.8-flash-high",
			svc.getMappedModelForThinkingLevel(account, "gemini-3.8-flash", ""))
		require.Equal(t,
			"gemini-3.8-flash-low",
			svc.getMappedModelForThinkingLevel(account, "gemini-3.8-flash", "low"))
	})

	t.Run("suffix requests are already upstream identities", func(t *testing.T) {
		account := newAntigravityAccountWithMapping(map[string]string{})
		for _, level := range []string{"low", "medium", "high"} {
			model := "gemini-4-carbon-" + level
			require.Equal(t, model, svc.getMappedModelForThinkingLevel(account, model, "high"))
		}
	})
}

// 裸请求的档位来源：Gemini 原生 thinkingLevel/budget、Claude thinking.budget_tokens、
// OpenAI/Anthropic reasoning effort 都通过现有 dynamic helper 落到真实后缀。
func TestAntigravityCarbonBareRequestLevelSources(t *testing.T) {
	svc := &AntigravityGatewayService{}
	account := newAntigravityAccountWithMapping(map[string]string{})

	t.Run("native thinkingLevel", func(t *testing.T) {
		for level, want := range map[string]string{
			"low":    "gemini-4-carbon-low",
			"medium": "gemini-4-carbon-medium",
			"high":   "gemini-4-carbon-high",
		} {
			got, matched := resolveGeminiThinkingVariant(account, "gemini-4-carbon",
				[]byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"`+level+`"}}}`))
			require.True(t, matched)
			require.Equal(t, want, got)
		}
	})

	t.Run("native legacy budget is converted to a level", func(t *testing.T) {
		for body, want := range map[string]string{
			`{"generationConfig":{"thinkingConfig":{"thinkingBudget":1000}}}`:  "gemini-4-carbon-low",
			`{"generationConfig":{"thinkingConfig":{"thinkingBudget":4000}}}`:  "gemini-4-carbon-medium",
			`{"generationConfig":{"thinkingConfig":{"thinkingBudget":32768}}}`: "gemini-4-carbon-high",
			`{"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`:    "gemini-4-carbon-high",
		} {
			got, matched := resolveGeminiThinkingVariant(account, "gemini-4-carbon", []byte(body))
			require.True(t, matched, body)
			require.Equal(t, want, got, body)
		}
	})

	t.Run("native request without thinking config defaults to medium", func(t *testing.T) {
		level, specified := geminiThinkingLevelFromBodyIfSpecified([]byte(`{"contents":[]}`))
		require.False(t, specified)
		require.Empty(t, level)
		require.Equal(t,
			"gemini-4-carbon-medium",
			svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", level))
	})

	t.Run("claude effort and reasoning effort", func(t *testing.T) {
		for effort, want := range map[string]string{
			"low":    "gemini-4-carbon-low",
			"medium": "gemini-4-carbon-medium",
			"high":   "gemini-4-carbon-high",
		} {
			body := []byte(`{"output_config":{"effort":"` + effort + `"}}`)
			level, ok := geminiThinkingLevelFromClaudeRequestIfSpecified(body, nil)
			require.True(t, ok)
			require.Equal(t, want, svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", level))

			reasoning := effort
			level, ok = geminiThinkingLevelFromClaudeRequestIfSpecified([]byte(`{}`), &reasoning)
			require.True(t, ok)
			require.Equal(t, want, svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", level))
		}
	})

	t.Run("claude request without thinking config defaults to medium", func(t *testing.T) {
		level, ok := geminiThinkingLevelFromClaudeRequestIfSpecified([]byte(`{}`), nil)
		require.False(t, ok)
		require.Equal(t,
			"gemini-4-carbon-medium",
			svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", level))
	})
}

// 显式映射账号的运行时透传补齐只补三档后缀；裸名不是上游身份，
// 因此没有被管理员显式映射的账号不会因为裸 Carbon 请求被选中。
func TestAntigravityCarbonPassthroughAndUnmappedAccounts(t *testing.T) {
	svc := &AntigravityGatewayService{}
	account := newAntigravityAccountWithMapping(map[string]string{
		"gemini-3.8-flash": "gemini-3.8-flash",
	})

	mapping := account.GetModelMapping()
	for _, level := range []string{"low", "medium", "high"} {
		model := "gemini-4-carbon-" + level
		require.Equal(t, model, mapping[model], "passthrough must inject %s", model)
	}
	require.NotContains(t, mapping, "gemini-4-carbon")

	// 透传补齐的三档后缀让裸请求仍可解析，默认档位仍是上游的 medium。
	require.Equal(t,
		"gemini-4-carbon-medium",
		svc.getMappedModelForThinkingLevel(account, "gemini-4-carbon", ""))
	require.True(t, account.IsModelSupported("gemini-4-carbon-medium"))

	// 不含 Carbon 的账号（例如 Gemini API 平台账号没有 Antigravity 的默认透传补齐）
	// 不会被 Carbon 请求选中。
	plain := &Account{
		ID:       2,
		Name:     "gemini-api",
		Platform: PlatformGemini,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gemini-3.8-flash": "gemini-3.8-flash"},
		},
	}
	require.Empty(t, svc.getMappedModelForThinkingLevel(plain, "gemini-4-carbon", ""))
	require.Empty(t, svc.getMappedModelForThinkingLevel(plain, "gemini-4-carbon-medium", "medium"))
}

// Carbon 只接受 thinkingLevel：原生请求里的 legacy thinkingBudget 不能原样透传。
func gjsonString(t *testing.T, body []byte, path string) string {
	t.Helper()
	return gjson.GetBytes(body, path).String()
}

func TestNormalizeCarbonNativeThinking(t *testing.T) {
	// 裸名（防御路径，正常流程映射后已是后缀）：budget 换算成 level。
	body := []byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingBudget":4000}}}`)
	normalized := normalizeCarbonNativeThinking(body, "gemini-4-carbon")
	require.NotContains(t, string(normalized), "thinkingBudget")
	require.Equal(t, "medium", gjsonString(t, normalized, "generationConfig.thinkingConfig.thinkingLevel"))

	// 已带后缀：后缀决定档位，只删除 budget。
	body = []byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingBudget":32768}}}`)
	normalized = normalizeCarbonNativeThinking(body, "gemini-4-carbon-low")
	require.NotContains(t, string(normalized), "thinkingBudget")
	require.Empty(t, gjsonString(t, normalized, "generationConfig.thinkingConfig.thinkingLevel"))

	// 客户端已显式给 level：保留 level，只删除 budget。
	body = []byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingBudget":32768,"thinkingLevel":"low"}}}`)
	normalized = normalizeCarbonNativeThinking(body, "gemini-4-carbon-medium")
	require.NotContains(t, string(normalized), "thinkingBudget")
	require.Equal(t, "low", gjsonString(t, normalized, "generationConfig.thinkingConfig.thinkingLevel"))

	// 非 Carbon 模型保持既有透传行为。
	body = []byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingBudget":4000}}}`)
	require.Equal(t, body, normalizeCarbonNativeThinking(body, "gemini-3.8-flash"))
	// 没有 budget 的 Carbon 请求不动。
	body = []byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`)
	require.Equal(t, body, normalizeCarbonNativeThinking(body, "gemini-4-carbon-low"))
}

// 价格：后缀归一必须落到 Carbon 自己的价卡（带 priority 档），不能落到 Argon
// （同为促销价但没有 priority 档）或被当成免费模型。
func TestGeminiCarbonTierPricingNotArgonOrFree(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	tokens := UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000}

	carbon := svc.fallbackPrices["gemini-4-carbon"]
	require.NotNil(t, carbon)
	argon := svc.fallbackPrices["gemini-4-argon"]
	require.NotNil(t, argon)
	require.NotEqual(t, argon.InputPricePerTokenPriority, carbon.InputPricePerTokenPriority,
		"Carbon must not be priced through the Argon card")
	require.Zero(t, svc.CacheStoragePricePerTokenHour("gemini-4-carbon-high"))

	for _, model := range []string{"gemini-4-carbon-low", "gemini-4-carbon-medium", "gemini-4-carbon-high"} {
		require.Same(t, carbon, svc.getFallbackPricing(model), model)

		cost, err := svc.CalculateCost(model, tokens, 1)
		require.NoError(t, err)
		require.InDelta(t, 2, cost.InputCost, 1e-12)
		require.InDelta(t, 10, cost.OutputCost, 1e-12)
		require.InDelta(t, 0.1, cost.CacheReadCost, 1e-12)
		require.Greater(t, cost.TotalCost, 0.0, "Carbon must never be free")

		priority, err := svc.CalculateCostWithServiceTier(model, tokens, 1, "priority")
		require.NoError(t, err)
		require.InDelta(t, 12.1*1.8, priority.TotalCost, 1e-12)
	}
}
