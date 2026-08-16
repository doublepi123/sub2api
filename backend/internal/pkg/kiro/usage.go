package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const UsageLimitsTarget = "AmazonCodeWhispererService.GetUsageLimits"

// UsageLimitsHTTPError is intentionally body-free so upstream responses cannot
// leak account details through admin API error messages.
type UsageLimitsHTTPError struct {
	StatusCode int
}

func (e *UsageLimitsHTTPError) Error() string {
	return fmt.Sprintf("kiro usage limits returned HTTP %d", e.StatusCode)
}

type UsageLimitsResponse struct {
	UsageBreakdownList []UsageBreakdown       `json:"usageBreakdownList"`
	NextDateReset      float64                `json:"nextDateReset"`
	SubscriptionInfo   *UsageSubscriptionInfo `json:"subscriptionInfo"`
}

type UsageSubscriptionInfo struct {
	SubscriptionName  string `json:"subscriptionName"`
	SubscriptionTitle string `json:"subscriptionTitle"`
	SubscriptionType  string `json:"subscriptionType"`
	Type              string `json:"type"`
}

type UsageBreakdown struct {
	ResourceType                 string       `json:"resourceType"`
	Unit                         string       `json:"unit"`
	DisplayName                  string       `json:"displayName"`
	CurrentUsage                 float64      `json:"currentUsage"`
	CurrentUsageWithPrecision    *float64     `json:"currentUsageWithPrecision"`
	UsageLimit                   float64      `json:"usageLimit"`
	UsageLimitWithPrecision      *float64     `json:"usageLimitWithPrecision"`
	CurrentOverages              float64      `json:"currentOverages"`
	CurrentOveragesWithPrecision *float64     `json:"currentOveragesWithPrecision"`
	NextDateReset                float64      `json:"nextDateReset"`
	FreeTrialInfo                *UsageGrant  `json:"freeTrialInfo"`
	Bonuses                      []UsageGrant `json:"bonuses"`
}

type UsageGrant struct {
	Status                    string   `json:"status"`
	FreeTrialStatus           string   `json:"freeTrialStatus"`
	CurrentUsage              float64  `json:"currentUsage"`
	CurrentUsageWithPrecision *float64 `json:"currentUsageWithPrecision"`
	UsageLimit                float64  `json:"usageLimit"`
	UsageLimitWithPrecision   *float64 `json:"usageLimitWithPrecision"`
	ExpiresAt                 float64  `json:"expiresAt"`
	FreeTrialExpiry           float64  `json:"freeTrialExpiry"`
}

type UsageCreditSummary struct {
	SubscriptionType  string
	SubscriptionTitle string
	ResourceType      string
	Unit              string
	CurrentUsage      float64
	UsageLimit        float64
	Remaining         float64
	UsagePercent      float64
	OverageUsage      float64
	NextResetAt       *time.Time
}

func UsageLimitsURL(region, profileARN string) string {
	host := UsageLimitsHost(region)
	query := url.Values{"isEmailRequired": []string{"false"}}
	profileARN = strings.TrimSpace(profileARN)
	if profileARN != "" && profileARN != DefaultProfileARN {
		query.Set("profileArn", profileARN)
	}
	return "https://" + host + "/?" + query.Encode()
}

func UsageLimitsHost(region string) string {
	region = normalizedRegion(region)
	if region == DefaultRegion {
		return "codewhisperer." + region + ".amazonaws.com"
	}
	return "q." + region + ".amazonaws.com"
}

func NewUsageLimitsRequest(ctx context.Context, region, profileARN, accessToken string) (*http.Request, error) {
	body := map[string]any{"isEmailRequired": false}
	profileARN = strings.TrimSpace(profileARN)
	if profileARN != "" && profileARN != DefaultProfileARN {
		body["profileArn"] = profileARN
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode kiro usage request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, UsageLimitsURL(region, profileARN), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-amz-target", UsageLimitsTarget)
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("amz-sdk-request", "attempt=1; max=1")
	return req, nil
}

func SummarizeUsageLimits(response *UsageLimitsResponse) *UsageCreditSummary {
	if response == nil || len(response.UsageBreakdownList) == 0 {
		return nil
	}

	selected := response.UsageBreakdownList[0]
	selectedLimit := effectiveUsageLimit(selected)
	selectedIsCredit := strings.EqualFold(selected.ResourceType, "CREDIT")
	for _, candidate := range response.UsageBreakdownList[1:] {
		candidateIsCredit := strings.EqualFold(candidate.ResourceType, "CREDIT")
		candidateLimit := effectiveUsageLimit(candidate)
		if (candidateIsCredit && !selectedIsCredit) || (candidateIsCredit == selectedIsCredit && candidateLimit > selectedLimit) {
			selected = candidate
			selectedLimit = candidateLimit
			selectedIsCredit = candidateIsCredit
		}
	}

	used := preferredAmount(selected.CurrentUsageWithPrecision, selected.CurrentUsage)
	limit := preferredAmount(selected.UsageLimitWithPrecision, selected.UsageLimit)
	for _, grant := range appendActiveGrants(selected) {
		used += preferredAmount(grant.CurrentUsageWithPrecision, grant.CurrentUsage)
		limit += preferredAmount(grant.UsageLimitWithPrecision, grant.UsageLimit)
	}
	used = finiteNonNegative(used)
	limit = finiteNonNegative(limit)
	overage := finiteNonNegative(preferredAmount(selected.CurrentOveragesWithPrecision, selected.CurrentOverages))
	remaining := math.Max(limit-used, 0)
	percent := 0.0
	if limit > 0 {
		percent = used / limit * 100
	}

	reset := selected.NextDateReset
	if reset <= 0 {
		reset = response.NextDateReset
	}
	var resetAt *time.Time
	if parsed, ok := usageResetTime(reset); ok {
		resetAt = &parsed
	}

	summary := &UsageCreditSummary{
		ResourceType: selected.ResourceType,
		Unit:         selected.Unit,
		CurrentUsage: used,
		UsageLimit:   limit,
		Remaining:    remaining,
		UsagePercent: percent,
		OverageUsage: overage,
		NextResetAt:  resetAt,
	}
	if response.SubscriptionInfo != nil {
		summary.SubscriptionTitle = firstNonEmpty(
			response.SubscriptionInfo.SubscriptionTitle,
			response.SubscriptionInfo.SubscriptionName,
			response.SubscriptionInfo.SubscriptionType,
			response.SubscriptionInfo.Type,
		)
		summary.SubscriptionType = firstNonEmpty(
			response.SubscriptionInfo.SubscriptionType,
			response.SubscriptionInfo.Type,
		)
	}
	return summary
}

func appendActiveGrants(breakdown UsageBreakdown) []UsageGrant {
	grants := make([]UsageGrant, 0, len(breakdown.Bonuses)+1)
	if breakdown.FreeTrialInfo != nil && usageGrantActive(*breakdown.FreeTrialInfo) {
		grants = append(grants, *breakdown.FreeTrialInfo)
	}
	for _, bonus := range breakdown.Bonuses {
		if usageGrantActive(bonus) {
			grants = append(grants, bonus)
		}
	}
	return grants
}

func usageGrantActive(grant UsageGrant) bool {
	status := strings.ToUpper(strings.TrimSpace(firstNonEmpty(grant.Status, grant.FreeTrialStatus)))
	return status == "" || status == "ACTIVE" || status == "ENABLED"
}

func effectiveUsageLimit(breakdown UsageBreakdown) float64 {
	limit := preferredAmount(breakdown.UsageLimitWithPrecision, breakdown.UsageLimit)
	for _, grant := range appendActiveGrants(breakdown) {
		limit += preferredAmount(grant.UsageLimitWithPrecision, grant.UsageLimit)
	}
	return finiteNonNegative(limit)
}

func preferredAmount(precise *float64, fallback float64) float64 {
	if precise != nil {
		return *precise
	}
	return fallback
}

func finiteNonNegative(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return value
}

func usageResetTime(value float64) (time.Time, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return time.Time{}, false
	}
	if value > 1e12 {
		value /= 1000
	}
	seconds, fraction := math.Modf(value)
	return time.Unix(int64(seconds), int64(fraction*float64(time.Second))).UTC(), true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
