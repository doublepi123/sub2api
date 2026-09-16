//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type catalogLeaseRepoSpy struct {
	*catalogRepoStub
	reads  atomic.Int32
	writes atomic.Int32
	read   func(context.Context) (*Account, error)
}

func (r *catalogLeaseRepoSpy) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.reads.Add(1)
	if r.read != nil {
		return r.read(ctx)
	}
	return r.catalogRepoStub.GetByID(ctx, id)
}

func (r *catalogLeaseRepoSpy) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.writes.Add(1)
	return r.catalogRepoStub.UpdateExtra(ctx, id, updates)
}

func (r *catalogLeaseRepoSpy) UpdateKiroModelCatalogIfCurrent(ctx context.Context, id int64, catalog map[string]any, version, generation int64) (bool, error) {
	r.writes.Add(1)
	return r.catalogRepoStub.UpdateKiroModelCatalogIfCurrent(ctx, id, catalog, version, generation)
}

func TestRefreshKiroModelCatalog_LeaseLostDuringDetachedReload_DoesNotPersist(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given: probe timeout precedes lease expiry by two seconds.
		s, a, repo := catalogFixture(t)
		previous, _ := a.kiroModelCatalog()
		probeCauses, readCauses := make(chan error, 1), make(chan error, 1)
		s.SetKiroModelCatalogFetcher(catalogBlockingFetcher{causes: probeCauses})
		spy := &catalogLeaseRepoSpy{catalogRepoStub: repo, read: func(ctx context.Context) (*Account, error) {
			<-ctx.Done()
			readCauses <- context.Cause(ctx)
			return nil, ctx.Err()
		}}
		s.accountRepo = spy
		r := NewKiroModelCatalogRefresher(s, &catalogLeaseFake{ok: true})
		r.leaseTTL = 27 * time.Second
		leaseCtx, stop := r.keepCatalogLease(context.Background(), r.lease, time.Now())
		defer stop(nil)
		ctx, cancel := context.WithTimeout(leaseCtx, 25*time.Second)
		defer cancel()
		// When
		got, err := s.RefreshKiroModelCatalog(ctx, a, time.Now())
		// Then
		require.ErrorIs(t, <-probeCauses, context.DeadlineExceeded)
		require.ErrorIs(t, <-readCauses, ErrKiroCatalogLeaseLost)
		require.ErrorIs(t, err, ErrKiroCatalogLeaseLost)
		require.Equal(t, previous, got)
		require.Equal(t, int32(1), spy.reads.Load())
		require.Zero(t, spy.writes.Load())
	})
}

func TestKiroCatalogRefresher_LeaseKeepalive_FailsClosed(t *testing.T) {
	for _, mode := range []string{"renew_error", "renew_timeout", "watchdog"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given
				lease := &catalogRenewalFake{renew: func(ctx context.Context) (bool, error) {
					if mode == "renew_timeout" {
						<-ctx.Done()
						return false, ctx.Err()
					}
					return true, errors.New("redis unavailable")
				}}
				causes := make(chan error, 2)
				p := &catalogProbeFake{hook: func(ctx context.Context, _ *Account) {
					<-ctx.Done()
					causes <- context.Cause(ctx)
				}}
				r := NewKiroModelCatalogRefresher(p, lease)
				r.leaseTTL, r.renewInterval, r.maxConcurrency = 3*time.Second, time.Second, 1
				if mode == "watchdog" {
					r.renewInterval = 10 * time.Second
				}
				// When
				r.Run(context.Background(), []*Account{catalogRefreshAccount(1), catalogRefreshAccount(2)})
				// Then
				require.ErrorIs(t, <-causes, ErrKiroCatalogLeaseLost)
				require.Len(t, p.calls, 1)
				require.Equal(t, int32(1), lease.releases.Load())
			})
		})
	}
}

func TestKiroCatalogRefresher_LeaseRenewal_CoversLongBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given: the batch lasts more than two original lease lifetimes.
		lease := &catalogRenewalFake{renew: func(context.Context) (bool, error) { return true, nil }}
		p := &catalogProbeFake{hook: func(ctx context.Context, _ *Account) {
			<-ctx.Done()
			require.ErrorIs(t, context.Cause(ctx), context.DeadlineExceeded)
		}}
		r := NewKiroModelCatalogRefresher(p, lease)
		r.leaseTTL = 3 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		// When
		r.Run(ctx, []*Account{catalogRefreshAccount(1)})
		// Then
		require.GreaterOrEqual(t, lease.renews.Load(), int32(6))
		require.Equal(t, int32(1), lease.releases.Load())
	})
}
