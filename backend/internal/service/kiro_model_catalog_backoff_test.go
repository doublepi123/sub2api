//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKiroCatalogRefresher_UnknownWithRetryAfter_NotProbedBeforeNextAttempt(t *testing.T) {
	// Given
	now := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	a := catalogRefreshAccount(1)
	a.Extra[kiroDetectedModelCatalogKey] = map[string]any{"state": "unknown", "next_attempt_at": now.Add(time.Hour).Format(time.RFC3339Nano)}
	p := &catalogProbeFake{}
	r := NewKiroModelCatalogRefresher(p, nil)
	r.now = func() time.Time { return now }
	// When / Then
	r.Run(context.Background(), []*Account{a})
	require.Empty(t, p.calls, "unknown catalog must honor upstream Retry-After")
	now = now.Add(3601 * time.Second)
	r.Run(context.Background(), []*Account{a})
	require.Len(t, p.calls, 1)
}

func TestKiroCatalogRefresher_EarlyRefresh_RespectsBackoff(t *testing.T) {
	// Given
	now := time.Now()
	a := catalogReadyAccount(1, now)
	p := &catalogProbeFake{}
	r := NewKiroModelCatalogRefresher(p, nil)
	r.now = func() time.Time { return now }
	r.recordAttempt(a.ID, now, errors.New("persistence failed"))
	// When
	r.RequestEarlyRefresh(a.ID)
	r.Run(context.Background(), []*Account{a})
	// Then
	require.Empty(t, p.calls)
}

func TestKiroCatalogRefresher_PersistedBackoff_SurvivesNewInstance(t *testing.T) {
	// Given: no process-local history in either instance.
	now := time.Now()
	a := catalogReadyAccount(1, now.Add(-3*time.Hour))
	a.Extra[kiroDetectedModelCatalogKey].(map[string]any)["next_attempt_at"] = now.Add(time.Hour).Format(time.RFC3339Nano)
	for range 2 {
		p := &catalogProbeFake{}
		r := NewKiroModelCatalogRefresher(p, nil)
		r.now = func() time.Time { return now }
		// When
		r.Run(context.Background(), []*Account{a})
		// Then
		require.Empty(t, p.calls)
	}
}
