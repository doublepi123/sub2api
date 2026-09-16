package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

type kiroUsageFetcherStub struct {
	response *kiro.UsageLimitsResponse
	err      error
	calls    int
}

func (s *kiroUsageFetcherStub) FetchKiroUsageLimits(context.Context, *Account) (*kiro.UsageLimitsResponse, error) {
	s.calls++
	return s.response, s.err
}

func serviceFloat64Pointer(value float64) *float64 {
	return &value
}

func TestAccountUsageServiceReturnsAndCachesKiroCredits(t *testing.T) {
	fetcher := &kiroUsageFetcherStub{response: &kiro.UsageLimitsResponse{
		NextDateReset: 1788220800,
		SubscriptionInfo: &kiro.UsageSubscriptionInfo{
			SubscriptionTitle: "KIRO PRO",
			Type:              "Q_DEVELOPER_STANDALONE_PRO",
		},
		UsageBreakdownList: []kiro.UsageBreakdown{
			{
				ResourceType:              "CREDIT",
				Unit:                      "INVOCATIONS",
				CurrentUsageWithPrecision: serviceFloat64Pointer(22.97),
				UsageLimitWithPrecision:   serviceFloat64Pointer(1000),
			},
		},
	}}
	svc := &AccountUsageService{cache: NewUsageCache(), kiroUsageFetcher: fetcher}
	account := &Account{ID: 88, Platform: PlatformKiro, Type: AccountTypeOAuth}

	first, err := svc.getUsageForAccount(context.Background(), account, false)
	require.NoError(t, err)
	require.NotNil(t, first.KiroSubscription)
	require.Equal(t, "KIRO PRO", first.KiroSubscription.SubscriptionTitle)
	require.InDelta(t, 22.97, first.KiroSubscription.CurrentUsage, 0.0001)
	require.InDelta(t, 1000, first.KiroSubscription.UsageLimit, 0.0001)
	require.InDelta(t, 977.03, first.KiroSubscription.Remaining, 0.0001)
	require.Equal(t, 1, fetcher.calls)

	second, err := svc.getUsageForAccount(context.Background(), account, false)
	require.NoError(t, err)
	require.Same(t, first, second)
	require.Equal(t, 1, fetcher.calls)

	_, err = svc.getUsageForAccount(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, 2, fetcher.calls)
}

func TestAccountUsageServiceMapsKiroUnauthorizedToReauthState(t *testing.T) {
	fetcher := &kiroUsageFetcherStub{err: &kiro.UsageLimitsHTTPError{StatusCode: 401}}
	svc := &AccountUsageService{cache: NewUsageCache(), kiroUsageFetcher: fetcher}
	account := &Account{ID: 89, Platform: PlatformKiro, Type: AccountTypeOAuth}

	usage, err := svc.getUsageForAccount(context.Background(), account, false)
	require.NoError(t, err)
	require.True(t, usage.NeedsReauth)
	require.Equal(t, "unauthenticated", usage.ErrorCode)
	require.NotEmpty(t, usage.Error)
}

func TestGetKiroUsage_CacheHitDoesNotPersist(t *testing.T) {
	// Given: a successful probe has populated the cache and persisted once.
	repo := &accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 2)}
	fetcher := &kiroUsageFetcherStub{response: &kiro.UsageLimitsResponse{
		SubscriptionInfo:   &kiro.UsageSubscriptionInfo{SubscriptionTitle: "KIRO FREE"},
		UsageBreakdownList: []kiro.UsageBreakdown{{ResourceType: "CREDIT", UsageLimit: 50}},
	}}
	svc := &AccountUsageService{accountRepo: repo, cache: NewUsageCache(), kiroUsageFetcher: fetcher}
	a := &Account{ID: 1, Platform: PlatformKiro, Type: AccountTypeOAuth, Extra: map[string]any{}}
	first, err := svc.getKiroUsage(context.Background(), a, false)
	require.NoError(t, err)
	require.Len(t, repo.updateExtraCh, 1)
	writesBefore := len(repo.updateExtraCh)
	// When
	second, err := svc.getKiroUsage(context.Background(), a, false)
	// Then
	require.NoError(t, err)
	require.Same(t, first, second)
	require.Equal(t, 1, fetcher.calls)
	require.Equal(t, 0, len(repo.updateExtraCh)-writesBefore, "cache hit must perform ZERO additional UpdateExtra calls")
}

func TestBuildKiroSchedulerExtraUpdatesPersistsCreditsSnapshot(t *testing.T) {
	resetAt := time.Now().UTC().Add(6 * time.Hour)
	updates := buildKiroSchedulerExtraUpdates(&KiroSubscriptionQuota{
		UsagePercent: 97.4,
		NextResetAt:  &resetAt,
	})

	require.InDelta(t, 97.4, updates[kiroSchedUtilizationKey], 0.0001)
	require.Equal(t, resetAt.UTC().Format(time.RFC3339), updates[kiroSchedResetAtKey])
	require.NotEmpty(t, updates[kiroSchedUpdatedAtKey])
}

func TestBuildKiroSchedulerExtraUpdatesOmitsPastReset(t *testing.T) {
	resetAt := time.Now().UTC().Add(-time.Hour)
	updates := buildKiroSchedulerExtraUpdates(&KiroSubscriptionQuota{
		UsagePercent: 12,
		NextResetAt:  &resetAt,
	})

	require.InDelta(t, 12.0, updates[kiroSchedUtilizationKey], 0.0001)
	_, hasReset := updates[kiroSchedResetAtKey]
	require.False(t, hasReset)
}

func TestBuildAntigravitySchedulerExtraUpdatesUsesMostConstrainedGeminiModel(t *testing.T) {
	resetAt := time.Now().UTC().Add(3 * time.Hour).Format(time.RFC3339)
	updates := buildAntigravitySchedulerExtraUpdates(map[string]*AntigravityModelQuota{
		"claude-sonnet-4":       {Utilization: 99, ResetTime: resetAt},
		"gemini-2.5-flash":      {Utilization: 40, ResetTime: resetAt},
		"models/gemini-2.5-pro": {Utilization: 93, ResetTime: resetAt},
	})

	require.InDelta(t, 93.0, updates[antigravitySchedUtilizationKey], 0.0001)
	require.Equal(t, "models/gemini-2.5-pro", updates[antigravitySchedScopeKey])
	require.Equal(t, resetAt, updates[antigravitySchedResetAtKey])
}

func TestBuildAntigravitySchedulerExtraUpdatesIgnoresClaudeOnlyQuota(t *testing.T) {
	require.Nil(t, buildAntigravitySchedulerExtraUpdates(map[string]*AntigravityModelQuota{
		"claude-sonnet-4": {Utilization: 99, ResetTime: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)},
	}))
}

func TestAccountUsageServicePersistsKiroSchedulerExtras(t *testing.T) {
	fetcher := &kiroUsageFetcherStub{response: &kiro.UsageLimitsResponse{
		NextDateReset: float64(time.Now().UTC().Add(8 * time.Hour).Unix()),
		UsageBreakdownList: []kiro.UsageBreakdown{
			{
				ResourceType:              "CREDIT",
				CurrentUsageWithPrecision: serviceFloat64Pointer(900),
				UsageLimitWithPrecision:   serviceFloat64Pointer(1000),
			},
		},
	}}
	repo := &accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 1)}
	svc := &AccountUsageService{cache: NewUsageCache(), kiroUsageFetcher: fetcher, accountRepo: repo}
	account := &Account{ID: 91, Platform: PlatformKiro, Type: AccountTypeOAuth}

	usage, err := svc.getUsageForAccount(context.Background(), account, false)
	require.NoError(t, err)
	require.NotNil(t, usage.KiroSubscription)
	require.InDelta(t, 90.0, account.Extra[kiroSchedUtilizationKey], 0.0001)
	require.NotEmpty(t, account.Extra[kiroSchedResetAtKey])
	select {
	case updates := <-repo.updateExtraCh:
		require.Contains(t, updates, kiroSchedUtilizationKey)
		require.InDelta(t, 90.0, updates[kiroSchedUtilizationKey], 0.0001)
	default:
		t.Fatal("expected UpdateExtra to persist kiro scheduler extras")
	}
}

func TestBuildKiroSchedulerExtraUpdates_Tier(t *testing.T) {
	futureReset := time.Now().UTC().Add(4 * time.Hour)
	cases := []struct {
		name         string
		tier         kiro.Tier
		expectTier   bool
		expectedTier string
	}{
		{
			name:         "free tier writes tier and updated_at",
			tier:         kiro.TierFree,
			expectTier:   true,
			expectedTier: "free",
		},
		{
			name:         "paid tier writes tier and updated_at",
			tier:         kiro.TierPaid,
			expectTier:   true,
			expectedTier: "paid",
		},
		{
			name:       "unknown tier omits tier and updated_at",
			tier:       kiro.TierUnknown,
			expectTier: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quota := &KiroSubscriptionQuota{
				UsagePercent: 42.5,
				NextResetAt:  &futureReset,
				Tier:         tc.tier,
			}
			updates := buildKiroSchedulerExtraUpdates(quota)
			require.NotNil(t, updates)

			require.InDelta(t, 42.5, updates[kiroSchedUtilizationKey], 0.0001)
			require.Equal(t, futureReset.UTC().Format(time.RFC3339), updates[kiroSchedResetAtKey])
			require.NotEmpty(t, updates[kiroSchedUpdatedAtKey])
			updatedAt, ok := updates[kiroSchedUpdatedAtKey].(string)
			require.True(t, ok)
			_, err := time.Parse(time.RFC3339, updatedAt)
			require.NoError(t, err)

			if tc.expectTier {
				require.Contains(t, updates, kiroSchedTierKey)
				require.Equal(t, tc.expectedTier, updates[kiroSchedTierKey])
				require.Contains(t, updates, kiroSchedTierUpdatedAtKey)
				require.NotEmpty(t, updates[kiroSchedTierUpdatedAtKey])
				tierUpdatedAt, ok := updates[kiroSchedTierUpdatedAtKey].(string)
				require.True(t, ok)
				_, err = time.Parse(time.RFC3339, tierUpdatedAt)
				require.NoError(t, err)
			} else {
				require.NotContains(t, updates, kiroSchedTierKey)
				require.NotContains(t, updates, kiroSchedTierUpdatedAtKey)
			}
		})
	}
}
