//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type catalogLeaseFake struct {
	ok       bool
	err      error
	key      string
	ttl      time.Duration
	released bool
}

func (l *catalogLeaseFake) TryAcquire(_ context.Context, key string, ttl time.Duration) (func(), bool, error) {
	l.key, l.ttl = key, ttl
	return func() { l.released = true }, l.ok, l.err
}

func TestKiroCatalogRefresher_LeaseNotAcquired_NoProbes(t *testing.T) {
	// Given
	p := &catalogProbeFake{}
	lease := &catalogLeaseFake{}
	r := NewKiroModelCatalogRefresher(p, lease)
	// When
	r.Run(context.Background(), []*Account{catalogRefreshAccount(1)})
	// Then
	require.Empty(t, p.calls)
	require.False(t, r.running.Load())
	require.False(t, lease.released)
	require.Equal(t, "kiro:model_catalog:refresher:leader", lease.key)
	require.Equal(t, 10*time.Minute, lease.ttl)
}

func TestKiroCatalogRefresher_LeaseErrorFailsClosed(t *testing.T) {
	// Given
	p := &catalogProbeFake{}
	lease := &catalogLeaseFake{ok: true, err: errors.New("redis unavailable")}
	r := NewKiroModelCatalogRefresher(p, lease)
	// When
	r.Run(context.Background(), []*Account{catalogRefreshAccount(1)})
	// Then
	require.Empty(t, p.calls)
	require.False(t, r.running.Load())
	require.False(t, lease.released)
}

func TestKiroCatalogRefresher_BoundedConcurrency(t *testing.T) {
	// Given
	started, release, done := make(chan struct{}, 10), make(chan struct{}), make(chan struct{})
	var active, high atomic.Int32
	p := &catalogProbeFake{hook: func(ctx context.Context, _ *Account) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := high.Load(); n > old; old = high.Load() {
			if high.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	r := NewKiroModelCatalogRefresher(p, nil)
	accounts := make([]*Account, 10)
	for i := range accounts {
		accounts[i] = catalogRefreshAccount(int64(i + 1))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// When
	go func() { defer close(done); r.Run(ctx, accounts) }()
	for range 3 {
		awaitCatalogSignal(t, started)
	}
	close(release)
	awaitCatalogSignal(t, done)
	// Then
	require.Equal(t, int32(3), high.Load())
	require.Len(t, p.calls, 10)
}

func TestKiroCatalogRefresher_NoOverlap(t *testing.T) {
	// Given
	started, release, done, second := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}), make(chan struct{})
	p := &catalogProbeFake{hook: func(ctx context.Context, _ *Account) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	r := NewKiroModelCatalogRefresher(p, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { defer close(done); r.Run(ctx, []*Account{catalogRefreshAccount(1)}) }()
	awaitCatalogSignal(t, started)
	// When
	go func() { defer close(second); r.Run(ctx, []*Account{catalogRefreshAccount(2)}) }()
	// Then: the second call returns while the first probe remains blocked.
	awaitCatalogSignal(t, second)
	require.True(t, r.running.Load())
	require.Empty(t, started)
	close(release)
	awaitCatalogSignal(t, done)
	require.Len(t, p.calls, 1)
}

func TestKiroCatalogRefresher_PanicSafety(t *testing.T) {
	// Given
	p := &catalogProbeFake{hook: func(_ context.Context, a *Account) {
		if a.ID == 1 {
			panic("probe panic")
		}
	}}
	lease := &catalogLeaseFake{ok: true}
	r := NewKiroModelCatalogRefresher(p, lease)
	// When
	r.Run(context.Background(), []*Account{catalogRefreshAccount(1), catalogRefreshAccount(2)})
	r.Run(context.Background(), []*Account{catalogRefreshAccount(3)})
	// Then
	require.Len(t, p.calls, 3)
	require.False(t, r.running.Load())
	require.True(t, lease.released)
}

func TestKiroCatalogRefresher_SkipsShadowAndNonOAuth(t *testing.T) {
	// Given
	a, b, c := catalogRefreshAccount(1), catalogRefreshAccount(2), catalogRefreshAccount(3)
	parent := int64(4)
	a.ParentAccountID = &parent
	b.Type = AccountTypeAPIKey
	c.Platform = PlatformOpenAI
	p := &catalogProbeFake{}
	r := NewKiroModelCatalogRefresher(p, nil)
	// When
	r.Run(context.Background(), []*Account{nil, a, b, c, catalogRefreshAccount(4)})
	// Then
	require.Len(t, p.calls, 1)
	require.Equal(t, int64(4), p.calls[0].id)
}
