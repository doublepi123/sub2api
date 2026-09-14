package service

import (
	"context"
	"errors"
	"time"
)

var ErrKiroCatalogLeaseLost = errors.New("kiro model catalog: leader lease lost")

const kiroCatalogLeaseKey = "kiro:model_catalog:refresher:leader"

type kiroCatalogLeaseContextKey struct{}

func (r *KiroModelCatalogRefresher) keepCatalogLease(ctx context.Context, lease LeaderLease, acquiredAt time.Time) (context.Context, context.CancelCauseFunc) {
	leaseCtx, cancel := context.WithCancelCause(ctx)
	renewable, ok := lease.(RenewableLeaderLease)
	if !ok {
		deadlineCtx, stop := context.WithDeadlineCause(leaseCtx, acquiredAt.Add(r.leaseTTL), ErrKiroCatalogLeaseLost)
		return context.WithValue(deadlineCtx, kiroCatalogLeaseContextKey{}, deadlineCtx), func(cause error) { cancel(cause); stop() }
	}
	interval := r.renewInterval
	if interval <= 0 {
		interval = r.leaseTTL / 3
	}
	watchdog := time.AfterFunc(time.Until(acquiredAt.Add(r.leaseTTL)), func() { cancel(ErrKiroCatalogLeaseLost) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer watchdog.Stop()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				started := time.Now()
				renewCtx, stop := context.WithTimeout(leaseCtx, min(5*time.Second, interval))
				owned, err := renewable.Renew(renewCtx, kiroCatalogLeaseKey, r.leaseTTL)
				stop()
				if err != nil || !owned {
					cancel(ErrKiroCatalogLeaseLost)
					return
				}
				// A late renewal must not resurrect an already expired local lease.
				if !watchdog.Reset(time.Until(started.Add(r.leaseTTL))) {
					cancel(ErrKiroCatalogLeaseLost)
					return
				}
			}
		}
	}()
	return context.WithValue(leaseCtx, kiroCatalogLeaseContextKey{}, leaseCtx), func(cause error) {
		cancel(cause)
		<-done
	}
}

func kiroCatalogLeaseCause(ctx context.Context) error {
	if leaseCtx, ok := ctx.Value(kiroCatalogLeaseContextKey{}).(context.Context); ok {
		if cause := context.Cause(leaseCtx); cause != nil {
			return cause
		}
	}
	return context.Cause(ctx)
}

func kiroCatalogPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := ctx
	stopTimeout := func() {}
	if ctx.Err() != nil {
		base, stopTimeout = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	}
	persistCtx, cancel := context.WithCancelCause(base)
	stopLease := func() bool { return false }
	if leaseCtx, ok := ctx.Value(kiroCatalogLeaseContextKey{}).(context.Context); ok {
		stopLease = context.AfterFunc(leaseCtx, func() { cancel(context.Cause(leaseCtx)) })
		if cause := context.Cause(leaseCtx); cause != nil {
			cancel(cause)
		}
	}
	return persistCtx, func() {
		stopLease()
		cancel(nil)
		stopTimeout()
	}
}
