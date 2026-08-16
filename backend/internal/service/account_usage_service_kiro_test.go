package service

import (
	"context"
	"testing"

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
