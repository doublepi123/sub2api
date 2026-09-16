package kiro

import (
	"strings"
	"unicode"
)

// Tier is the Kiro account subscription tier. Detection is best-effort: real
// subscriptionInfo payloads are inconsistent, so TierUnknown means "we could
// not decide". Tier is display metadata; model capability comes from the catalog.
type Tier string

const (
	TierUnknown Tier = ""
	TierFree    Tier = "free"
	TierPaid    Tier = "paid"
)

// ParseTierString parses a persisted or admin-supplied tier value. Only the
// canonical values are accepted; anything else (including empty) is unknown.
func ParseTierString(s string) Tier {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(TierFree):
		return TierFree
	case string(TierPaid):
		return TierPaid
	default:
		return TierUnknown
	}
}

var freeTierTokens = map[string]struct{}{"FREE": {}}
var paidTierTokens = map[string]struct{}{
	"PRO": {}, "PLUS": {}, "MAX": {}, "POWER": {}, "ENTERPRISE": {},
}

// ParseTier infers the tier from the four subscriptionInfo fields by exact
// token matching (never substring matching, so "PROFESSIONAL" is not "PRO").
// Exactly one kind of signal decides the tier; conflicting or missing signals
// yield TierUnknown rather than a wrong guess.
func ParseTier(info *UsageSubscriptionInfo) Tier {
	if info == nil {
		return TierUnknown
	}
	free, paid := false, false
	for _, field := range []string{info.SubscriptionName, info.SubscriptionTitle, info.SubscriptionType, info.Type} {
		for _, token := range splitTierTokens(field) {
			if _, ok := freeTierTokens[token]; ok {
				free = true
			}
			if _, ok := paidTierTokens[token]; ok {
				paid = true
			}
		}
	}
	if free == paid {
		return TierUnknown
	}
	if free {
		return TierFree
	}
	return TierPaid
}

func splitTierTokens(s string) []string {
	return strings.FieldsFunc(strings.ToUpper(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// modelAliases is an explicit, closed map from Anthropic dashed/dated ids to
// Kiro dotted ids. No generic rewriting: every entry is verified by hand and
// pinned by TestNormalizeModelAlias_TargetsExistInModels.
var modelAliases = map[string]string{
	"claude-opus-4-5":            "claude-opus-4.5",
	"claude-opus-4-5-20251101":   "claude-opus-4.5",
	"claude-opus-4-6":            "claude-opus-4.6",
	"claude-opus-4-7":            "claude-opus-4.7",
	"claude-opus-4-8":            "claude-opus-4.8",
	"claude-sonnet-4-5":          "claude-sonnet-4.5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
	"claude-sonnet-4-6":          "claude-sonnet-4.6",
	"claude-sonnet-4-20250514":   "claude-sonnet-4",
	"claude-haiku-4-5":           "claude-haiku-4.5",
	"claude-haiku-4-5-20251001":  "claude-haiku-4.5",
}

// NormalizeModelAlias maps a known Anthropic dashed/dated id to its Kiro
// dotted equivalent. Unknown names are returned unchanged.
func NormalizeModelAlias(model string) string {
	if alias, ok := modelAliases[strings.ToLower(strings.TrimSpace(model))]; ok {
		return alias
	}
	return model
}
