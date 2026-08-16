package kiro

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func float64Pointer(value float64) *float64 {
	return &value
}

func TestNewUsageLimitsRequestMatchesKiroRuntimeContract(t *testing.T) {
	req, err := NewUsageLimitsRequest(
		context.Background(),
		"us-east-1",
		"arn:aws:codewhisperer:us-east-1:123456789012:profile/example",
		"access-token",
	)
	require.NoError(t, err)
	require.Equal(t, "https://codewhisperer.us-east-1.amazonaws.com/?isEmailRequired=false&profileArn=arn%3Aaws%3Acodewhisperer%3Aus-east-1%3A123456789012%3Aprofile%2Fexample", req.URL.String())
	require.Equal(t, UsageLimitsTarget, req.Header.Get("x-amz-target"))
	require.Equal(t, "Bearer access-token", req.Header.Get("Authorization"))
	require.Equal(t, "application/x-amz-json-1.0", req.Header.Get("Content-Type"))

	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))
	require.Equal(t, false, payload["isEmailRequired"])
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:123456789012:profile/example", payload["profileArn"])
}

func TestNewUsageLimitsRequestSkipsBuilderIDPlaceholderProfile(t *testing.T) {
	req, err := NewUsageLimitsRequest(context.Background(), "eu-central-1", DefaultProfileARN, "token")
	require.NoError(t, err)
	require.Equal(t, "q.eu-central-1.amazonaws.com", req.URL.Host)
	require.Equal(t, "false", req.URL.Query().Get("isEmailRequired"))
	require.Empty(t, req.URL.Query().Get("profileArn"))
}

func TestSummarizeUsageLimitsUsesPreciseCreditsAndActiveGrants(t *testing.T) {
	reset := float64(1788220800)
	response := &UsageLimitsResponse{
		NextDateReset: reset,
		SubscriptionInfo: &UsageSubscriptionInfo{
			SubscriptionTitle: "KIRO PRO",
			Type:              "Q_DEVELOPER_STANDALONE_PRO",
		},
		UsageBreakdownList: []UsageBreakdown{
			{
				ResourceType:              "CREDIT",
				Unit:                      "INVOCATIONS",
				CurrentUsage:              22,
				CurrentUsageWithPrecision: float64Pointer(22.97),
				UsageLimit:                1000,
				UsageLimitWithPrecision:   float64Pointer(1000),
				FreeTrialInfo: &UsageGrant{
					FreeTrialStatus:           "ACTIVE",
					CurrentUsageWithPrecision: float64Pointer(5.5),
					UsageLimitWithPrecision:   float64Pointer(100),
				},
				Bonuses: []UsageGrant{
					{Status: "ACTIVE", CurrentUsageWithPrecision: float64Pointer(2), UsageLimitWithPrecision: float64Pointer(50)},
					{Status: "EXPIRED", CurrentUsageWithPrecision: float64Pointer(99), UsageLimitWithPrecision: float64Pointer(500)},
				},
			},
		},
	}

	summary := SummarizeUsageLimits(response)
	require.NotNil(t, summary)
	require.Equal(t, "KIRO PRO", summary.SubscriptionTitle)
	require.Equal(t, "Q_DEVELOPER_STANDALONE_PRO", summary.SubscriptionType)
	require.InDelta(t, 30.47, summary.CurrentUsage, 0.0001)
	require.InDelta(t, 1150, summary.UsageLimit, 0.0001)
	require.InDelta(t, 1119.53, summary.Remaining, 0.0001)
	require.InDelta(t, 30.47/1150*100, summary.UsagePercent, 0.0001)
	require.NotNil(t, summary.NextResetAt)
	require.Equal(t, time.Unix(int64(reset), 0).UTC(), *summary.NextResetAt)
}
