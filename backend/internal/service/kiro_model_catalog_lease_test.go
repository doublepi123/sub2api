//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type catalogRenewalFake struct {
	renews   atomic.Int32
	releases atomic.Int32
	renew    func(context.Context) (bool, error)
}

func (l *catalogRenewalFake) TryAcquire(context.Context, string, time.Duration) (func(), bool, error) {
	return func() { l.releases.Add(1) }, true, nil
}

func (l *catalogRenewalFake) Renew(ctx context.Context, _ string, _ time.Duration) (bool, error) {
	n := l.renews.Add(1)
	if l.renew != nil {
		return l.renew(ctx)
	}
	return n < 2, nil
}

type catalogBlockingFetcher struct {
	causes chan error
}

func (f catalogBlockingFetcher) FetchKiroAvailableModels(ctx context.Context, _ *Account) ([]string, error) {
	<-ctx.Done()
	f.causes <- context.Cause(ctx)
	return nil, ctx.Err()
}

func TestKiroCatalogRefresher_LeaseLost_StopsProbesAndWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given: the real service persists through a repository spy.
		s, a, repo := catalogFixture(t)
		spy := &catalogLeaseRepoSpy{catalogRepoStub: repo}
		s.accountRepo = spy
		causes := make(chan error, 6)
		s.SetKiroModelCatalogFetcher(catalogBlockingFetcher{causes: causes})
		lease := &catalogRenewalFake{}
		r := NewKiroModelCatalogRefresher(s, lease)
		r.maxConcurrency = 1
		r.leaseTTL = 3 * time.Second
		r.renewInterval = time.Second
		accounts := []*Account{a}
		for id := int64(2); id <= 6; id++ {
			accounts = append(accounts, catalogRefreshAccount(id))
		}
		// When
		r.Run(context.Background(), accounts)
		// Then
		require.ErrorIs(t, <-causes, ErrKiroCatalogLeaseLost)
		require.Empty(t, causes, "queued accounts must never be probed")
		require.Nil(t, repo.updates, "lease loss must not persist")
		require.Zero(t, spy.reads.Load())
		require.Zero(t, spy.writes.Load())
		require.Equal(t, int32(2), lease.renews.Load())
		require.Equal(t, int32(1), lease.releases.Load())
	})
}

func TestKiroCatalogRefresher_NonRenewableLease_BatchDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given
		s, a, repo := catalogFixture(t)
		causes := make(chan error, 6)
		s.SetKiroModelCatalogFetcher(catalogBlockingFetcher{causes: causes})
		lease := &catalogLeaseFake{ok: true}
		r := NewKiroModelCatalogRefresher(s, lease)
		r.maxConcurrency = 1
		r.leaseTTL = time.Second
		// When
		r.Run(context.Background(), []*Account{a, catalogRefreshAccount(2)})
		// Then
		require.ErrorIs(t, <-causes, ErrKiroCatalogLeaseLost)
		require.Empty(t, causes)
		require.Nil(t, repo.updates)
		require.True(t, lease.released)
	})
}

func TestRefreshKiroModelCatalog_LeaseLost_DoesNotPersist(t *testing.T) {
	// Given
	s, a, repo := catalogFixture(t)
	spy := &catalogLeaseRepoSpy{catalogRepoStub: repo}
	s.accountRepo = spy
	previous, _ := a.kiroModelCatalog()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrKiroCatalogLeaseLost)
	// When
	got, err := s.RefreshKiroModelCatalog(ctx, a, time.Now())
	// Then
	require.ErrorIs(t, err, ErrKiroCatalogLeaseLost)
	require.Equal(t, previous, got)
	require.Nil(t, repo.updates)
	require.Zero(t, spy.reads.Load())
	require.Zero(t, spy.writes.Load())
}
