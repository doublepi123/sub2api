//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

type catalogProbeCall struct {
	id int64
	at time.Time
}
type catalogProbeFake struct {
	mu    sync.Mutex
	calls []catalogProbeCall
	err   error
	hook  func(context.Context, *Account)
}

func (p *catalogProbeFake) RefreshKiroModelCatalog(ctx context.Context, a *Account, at time.Time) (kiro.ModelCatalog, error) {
	p.mu.Lock()
	p.calls = append(p.calls, catalogProbeCall{a.ID, at})
	p.mu.Unlock()
	if p.hook != nil {
		p.hook(ctx, a)
	}
	c := kiro.ModelCatalog{State: kiro.CatalogStateReady, LastAttemptAt: at.Format(time.RFC3339Nano), LastSuccessAt: at.Format(time.RFC3339Nano), ScopeFingerprint: a.kiroCatalogScopeFingerprint()}
	if p.err != nil {
		c.LastErrorCode = "network"
	}
	a.Extra[kiroDetectedModelCatalogKey] = map[string]any{"schema_version": kiro.CatalogSchemaVersion, "source": kiro.CatalogSource, "state": string(c.State), "last_attempt_at": c.LastAttemptAt, "last_success_at": c.LastSuccessAt, "scope_fingerprint": c.ScopeFingerprint, "last_error_code": c.LastErrorCode}
	return c, p.err
}

func catalogRefreshAccount(id int64) *Account {
	return &Account{ID: id, Platform: PlatformKiro, Type: AccountTypeOAuth, Extra: map[string]any{}}
}

func catalogReadyAccount(id int64, at time.Time) *Account {
	a := catalogRefreshAccount(id)
	a.Extra[kiroDetectedModelCatalogKey] = map[string]any{"schema_version": kiro.CatalogSchemaVersion, "source": kiro.CatalogSource, "state": "ready", "scope_fingerprint": a.kiroCatalogScopeFingerprint(), "last_attempt_at": at.Format(time.RFC3339Nano), "last_success_at": at.Format(time.RFC3339Nano)}
	return a
}

func awaitCatalogSignal[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for signal")
		var zero T
		return zero
	}
}

func TestKiroCatalogRefresher_UnknownAndFingerprintMismatchGoFirst(t *testing.T) {
	// Given
	now := time.Now()
	a := catalogReadyAccount(1, now.Add(-3*time.Hour))
	b := catalogRefreshAccount(2)
	c := catalogReadyAccount(3, now)
	c.Credentials = map[string]any{"region": "changed"}
	p := &catalogProbeFake{}
	r := NewKiroModelCatalogRefresher(p, nil)
	r.now = func() time.Time { return now }
	r.maxConcurrency = 1
	// When
	r.Run(context.Background(), []*Account{a, b, c})
	// Then
	require.Equal(t, []catalogProbeCall{{2, now}, {3, now}, {1, now}}, p.calls)
}

func TestKiroCatalogRefresher_NotDueSkipped(t *testing.T) {
	// Given
	now := time.Now()
	p := &catalogProbeFake{}
	r := NewKiroModelCatalogRefresher(p, nil)
	r.now = func() time.Time { return now }
	// When
	r.Run(context.Background(), []*Account{catalogReadyAccount(1, now.Add(-10*time.Minute))})
	// Then
	require.Empty(t, p.calls)
}

func TestKiroCatalogRefresher_JitterWithinBounds(t *testing.T) {
	for _, jitter := range []float64{-1, -0.5, 0, 0.5, 1} {
		t.Run(fmt.Sprint(jitter), func(t *testing.T) {
			// Given
			at := time.Now()
			now := at
			p := &catalogProbeFake{}
			r := NewKiroModelCatalogRefresher(p, nil)
			r.now = func() time.Time { return now }
			r.jitter = func(int64) float64 { return jitter }
			a := catalogReadyAccount(1, at)
			interval := time.Duration(float64(2*time.Hour) * (1 + 0.2*jitter))
			// When / Then: test the due boundary rather than a private formula.
			now = at.Add(interval - time.Nanosecond)
			r.Run(context.Background(), []*Account{a})
			require.Empty(t, p.calls)
			now = at.Add(interval)
			r.Run(context.Background(), []*Account{a})
			require.Len(t, p.calls, 1)
			require.GreaterOrEqual(t, interval, 96*time.Minute)
			require.LessOrEqual(t, interval, 144*time.Minute)
		})
	}
}

func TestKiroCatalogRefresher_BackoffDoublesAndCaps(t *testing.T) {
	// Given
	now := time.Now()
	p := &catalogProbeFake{err: errors.New("probe failed")}
	r := NewKiroModelCatalogRefresher(p, nil)
	r.now = func() time.Time { return now }
	r.jitter = func(int64) float64 { return 0 }
	a := catalogRefreshAccount(1)
	r.Run(context.Background(), []*Account{a})
	for i, interval := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		// When / Then
		now = now.Add(interval - time.Nanosecond)
		r.Run(context.Background(), []*Account{a})
		require.Len(t, p.calls, i+1)
		now = now.Add(time.Nanosecond)
		r.Run(context.Background(), []*Account{a})
		require.Len(t, p.calls, i+2)
	}
	// When / Then: success resets the failure interval.
	p.err = nil
	now = now.Add(4 * time.Hour)
	r.Run(context.Background(), []*Account{a})
	count := len(p.calls)
	now = now.Add(2 * time.Hour)
	r.Run(context.Background(), []*Account{a})
	require.Len(t, p.calls, count+1)
}

func TestKiroCatalogRefresher_HonorsRetryAfter(t *testing.T) {
	for _, retry := range []time.Duration{600 * time.Second, 2 * time.Hour, 8 * time.Hour} {
		t.Run(retry.String(), func(t *testing.T) {
			// Given
			now := time.Now()
			p := &catalogProbeFake{err: fmt.Errorf("wrapped: %w", &kiro.CatalogHTTPError{StatusCode: 429, RetryAfter: retry})}
			r := NewKiroModelCatalogRefresher(p, nil)
			r.now = func() time.Time { return now }
			a := catalogRefreshAccount(1)
			r.Run(context.Background(), []*Account{a})
			interval := min(retry, 4*time.Hour)
			if interval < 15*time.Minute {
				interval = 15 * time.Minute
			}
			// When / Then
			now = now.Add(interval - time.Nanosecond)
			r.Run(context.Background(), []*Account{a})
			require.Len(t, p.calls, 1)
			now = now.Add(time.Nanosecond)
			r.Run(context.Background(), []*Account{a})
			require.Len(t, p.calls, 2)
		})
	}
}

func TestKiroCatalogRefresher_EarlyRefreshCoalesced(t *testing.T) {
	// Given
	now := time.Now()
	p := &catalogProbeFake{}
	r := NewKiroModelCatalogRefresher(p, nil)
	r.now = func() time.Time { return now }
	a := catalogReadyAccount(1, now)
	// When
	for range 100 {
		r.RequestEarlyRefresh(1)
	}
	r.Run(context.Background(), []*Account{a})
	for range 100 {
		r.RequestEarlyRefresh(1)
	}
	r.Run(context.Background(), []*Account{a})
	// Then
	require.Len(t, p.calls, 1)
	now = now.Add(10 * time.Minute)
	r.RequestEarlyRefresh(1)
	r.Run(context.Background(), []*Account{a})
	require.Len(t, p.calls, 2)
}
