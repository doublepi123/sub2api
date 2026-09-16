//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type kiroCatalogListRepo struct {
	AccountRepository
	listStarted chan string
	listRelease chan struct{}
	account     Account
}

func (r *kiroCatalogListRepo) ListOAuthRefreshCandidatePage(context.Context, OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error) {
	return &OAuthRefreshCandidatePage{}, nil
}

func (r *kiroCatalogListRepo) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	r.listStarted <- platform
	select {
	case <-r.listRelease:
		return []Account{r.account}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestKiroCatalogRefresher_TokenPassSourcesHealthyAccountsWithoutBlocking(t *testing.T) {
	// Given: the token page is empty while the separate platform query blocks.
	repo := &kiroCatalogListRepo{listStarted: make(chan string, 2), listRelease: make(chan struct{}), account: *catalogRefreshAccount(9)}
	svc := NewTokenRefreshService(repo, nil, nil, nil, nil, nil, nil, &config.Config{}, nil)
	defer svc.Stop()
	started, done := make(chan int64, 1), make(chan struct{})
	p := &catalogProbeFake{hook: func(ctx context.Context, a *Account) { started <- a.ID; <-ctx.Done() }}
	svc.kiroModelCatalogRefresher = NewKiroModelCatalogRefresher(p, nil)
	// When
	go func() { defer close(done); svc.processRefreshContext(svc.runCtx) }()
	// Then
	awaitCatalogSignal(t, done)
	require.Equal(t, PlatformKiro, awaitCatalogSignal(t, repo.listStarted))
	svc.kiroModelCatalogRefresher.runFromRepository(svc.runCtx, repo)
	require.Empty(t, repo.listStarted)
	close(repo.listRelease)
	require.Equal(t, int64(9), awaitCatalogSignal(t, started))
	require.Same(t, svc.kiroModelCatalogRefresher, svc.KiroCatalogRefresher())
}

func TestKiroCatalogRefresher_CancellationStopsQueuedProbes(t *testing.T) {
	// Given
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, done := make(chan struct{}, 10), make(chan struct{})
	var calls atomic.Int32
	p := &catalogProbeFake{hook: func(ctx context.Context, _ *Account) { calls.Add(1); started <- struct{}{}; <-ctx.Done() }}
	r := NewKiroModelCatalogRefresher(p, nil)
	r.maxConcurrency = 1
	accounts := make([]*Account, 10)
	for i := range accounts {
		accounts[i] = catalogRefreshAccount(int64(i + 1))
	}
	go func() { defer close(done); r.Run(ctx, accounts) }()
	awaitCatalogSignal(t, started)
	// When
	cancel()
	// Then
	awaitCatalogSignal(t, done)
	require.Equal(t, int32(1), calls.Load())
	require.False(t, r.running.Load())
}

func TestKiroCatalogRefresher_MissingTimestampAndExpiredBackoff(t *testing.T) {
	for _, stamp := range []string{"", time.Now().Add(-15 * time.Minute).Format(time.RFC3339Nano)} {
		t.Run(stamp, func(t *testing.T) {
			// Given
			now := time.Now()
			a := catalogRefreshAccount(1)
			a.Extra[kiroDetectedModelCatalogKey] = map[string]any{"state": "ready", "scope_fingerprint": a.kiroCatalogScopeFingerprint(), "last_attempt_at": stamp, "last_error_code": "network"}
			deadline := make(chan time.Duration, 1)
			p := &catalogProbeFake{hook: func(ctx context.Context, _ *Account) { end, _ := ctx.Deadline(); deadline <- time.Until(end) }}
			r := NewKiroModelCatalogRefresher(p, nil)
			r.now = func() time.Time { return now }
			// When
			r.Run(context.Background(), []*Account{a, a})
			// Then
			require.Len(t, p.calls, 1)
			require.InDelta(t, float64(25*time.Second), float64(awaitCatalogSignal(t, deadline)), float64(time.Second))
		})
	}
}
