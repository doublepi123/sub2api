package kiro

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTier(t *testing.T) {
	tests := []struct {
		name string
		info *UsageSubscriptionInfo
		want Tier
	}{
		{name: "kiro free name", info: &UsageSubscriptionInfo{SubscriptionName: "Kiro Free"}, want: TierFree},
		{name: "free bare", info: &UsageSubscriptionInfo{Type: "FREE"}, want: TierFree},
		{name: "kiro free underscore", info: &UsageSubscriptionInfo{Type: "KIRO_FREE"}, want: TierFree},
		{name: "kiro pro name", info: &UsageSubscriptionInfo{SubscriptionName: "Kiro Pro"}, want: TierPaid},
		{name: "pro bare", info: &UsageSubscriptionInfo{Type: "PRO"}, want: TierPaid},
		{name: "kiro pro underscore", info: &UsageSubscriptionInfo{Type: "KIRO_PRO"}, want: TierPaid},
		{name: "q developer standalone pro", info: &UsageSubscriptionInfo{Type: "Q_DEVELOPER_STANDALONE_PRO"}, want: TierPaid},
		{name: "pro plus", info: &UsageSubscriptionInfo{SubscriptionType: "PRO_PLUS"}, want: TierPaid},
		{name: "pro max", info: &UsageSubscriptionInfo{SubscriptionType: "PRO_MAX"}, want: TierPaid},
		{name: "power", info: &UsageSubscriptionInfo{SubscriptionType: "POWER"}, want: TierPaid},
		{name: "enterprise", info: &UsageSubscriptionInfo{SubscriptionType: "ENTERPRISE"}, want: TierPaid},
		{name: "nil info", info: nil, want: TierUnknown},
		{name: "all empty", info: &UsageSubscriptionInfo{}, want: TierUnknown},
		{
			name: "conflicting signals",
			info: &UsageSubscriptionInfo{SubscriptionName: "Kiro Free", SubscriptionTitle: "Kiro Pro"},
			want: TierUnknown,
		},
		{name: "professional is not pro", info: &UsageSubscriptionInfo{SubscriptionTitle: "PROFESSIONAL"}, want: TierUnknown},
		{name: "lowercase free", info: &UsageSubscriptionInfo{Type: "free"}, want: TierFree},
		{name: "spaced title", info: &UsageSubscriptionInfo{SubscriptionTitle: "  Kiro Pro  "}, want: TierPaid},
		{
			name: "real free account payload",
			info: &UsageSubscriptionInfo{SubscriptionTitle: "KIRO FREE", Type: "Q_DEVELOPER_STANDALONE_FREE"},
			want: TierFree,
		},
		{
			name: "real free type alone",
			info: &UsageSubscriptionInfo{Type: "Q_DEVELOPER_STANDALONE_FREE"},
			want: TierFree,
		},
		{
			name: "real free title alone",
			info: &UsageSubscriptionInfo{SubscriptionTitle: "KIRO FREE"},
			want: TierFree,
		},
		{
			name: "real paid account payload",
			info: &UsageSubscriptionInfo{SubscriptionTitle: "KIRO PRO+", Type: "Q_DEVELOPER_STANDALONE_PRO_PLUS"},
			want: TierPaid,
		},
		{
			name: "real paid type alone",
			info: &UsageSubscriptionInfo{Type: "Q_DEVELOPER_STANDALONE_PRO_PLUS"},
			want: TierPaid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ParseTier(tt.info))
		})
	}
}

func TestParseTierString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Tier
	}{
		{name: "free", input: "free", want: TierFree},
		{name: "paid", input: "paid", want: TierPaid},
		{name: "uppercase free", input: "FREE", want: TierFree},
		{name: "spaced paid", input: "  Paid  ", want: TierPaid},
		{name: "empty", input: "", want: TierUnknown},
		{name: "blank", input: "   ", want: TierUnknown},
		{name: "garbage", input: "whatever", want: TierUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ParseTierString(tt.input))
		})
	}
}

func TestNormalizeModelAlias(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "opus 4.5 dashed", input: "claude-opus-4-5", want: "claude-opus-4.5"},
		{name: "opus 4.5 dated", input: "claude-opus-4-5-20251101", want: "claude-opus-4.5"},
		{name: "sonnet 4.5 dated", input: "claude-sonnet-4-5-20250929", want: "claude-sonnet-4.5"},
		{name: "sonnet 4 dated", input: "claude-sonnet-4-20250514", want: "claude-sonnet-4"},
		{name: "haiku 4.5 dated", input: "claude-haiku-4-5-20251001", want: "claude-haiku-4.5"},
		{name: "opus 4.6 dashed", input: "claude-opus-4-6", want: "claude-opus-4.6"},
		{name: "dotted passes through", input: "claude-opus-4.5", want: "claude-opus-4.5"},
		{name: "gpt passes through", input: "gpt-5.6-sol", want: "gpt-5.6-sol"},
		{name: "no generic rewrite", input: "random-model-3-1", want: "random-model-3-1"},
		{name: "case and space normalized", input: " Claude-Opus-4-5 ", want: "claude-opus-4.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, NormalizeModelAlias(tt.input))
		})
	}
}

func TestNormalizeModelAlias_TargetsExistInModels(t *testing.T) {
	known := make(map[string]struct{}, len(Models))
	for _, model := range Models {
		known[model] = struct{}{}
	}
	for source, target := range modelAliases {
		require.Contains(t, known, target, "alias %s -> %s must target a model in kiro.Models", source, target)
	}
}
