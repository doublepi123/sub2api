package service

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

type kiroCatalogProber interface {
	RefreshKiroModelCatalog(ctx context.Context, account *Account, probeStartedAt time.Time) (kiro.ModelCatalog, error)
}

type KiroModelCatalogRefresher struct {
	prober             kiroCatalogProber
	lease              LeaderLease
	running            atomic.Bool
	now                func() time.Time
	jitter             func(accountID int64) float64
	maxConcurrency     int
	leaseTTL           time.Duration
	renewInterval      time.Duration
	baseInterval       time.Duration
	jitterFraction     float64
	failureBackoffBase time.Duration
	failureBackoffMax  time.Duration
	mu                 sync.Mutex
	failures           map[int64]int
	retryAfter         map[int64]time.Duration
	attempts           map[int64]time.Time
	earlyRequests      map[int64]time.Time
	earlyPending       map[int64]bool
}

func NewKiroModelCatalogRefresher(prober kiroCatalogProber, lease LeaderLease) *KiroModelCatalogRefresher {
	return &KiroModelCatalogRefresher{
		prober: prober, lease: lease, now: time.Now, jitter: func(int64) float64 { return rand.Float64()*2 - 1 },
		maxConcurrency: 3, baseInterval: 2 * time.Hour, jitterFraction: 0.2,
		leaseTTL:           10 * time.Minute,
		failureBackoffBase: kiro.CatalogFailureBackoffBase, failureBackoffMax: kiro.CatalogFailureBackoffMax,
		failures: make(map[int64]int), retryAfter: make(map[int64]time.Duration), attempts: make(map[int64]time.Time),
		earlyRequests: make(map[int64]time.Time), earlyPending: make(map[int64]bool),
	}
}

func (r *KiroModelCatalogRefresher) RequestEarlyRefresh(accountID int64) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.earlyRequests[accountID]; ok && now.Before(last.Add(10*time.Minute)) {
		return
	}
	r.earlyRequests[accountID] = now
	r.earlyPending[accountID] = true
}

func (r *KiroModelCatalogRefresher) Run(ctx context.Context, accounts []*Account) {
	r.run(ctx, func(context.Context) ([]*Account, error) { return accounts, nil })
}

func (r *KiroModelCatalogRefresher) run(ctx context.Context, load func(context.Context) ([]*Account, error)) {
	if !r.running.CompareAndSwap(false, true) {
		return
	}
	defer r.running.Store(false)
	defer func() {
		if recover() != nil {
			slog.Warn("kiro_model_catalog_refresh_panic")
		}
	}()
	if ctx.Err() != nil {
		return
	}
	lease := r.lease
	if lease == nil {
		lease = NoopLeaderLease()
	}
	acquiredAt := time.Now()
	release, ok, err := lease.TryAcquire(ctx, kiroCatalogLeaseKey, r.leaseTTL)
	if err != nil {
		slog.Warn("kiro_model_catalog_lease_failed", "error", err)
		return
	}
	if !ok {
		return
	}
	defer release()
	leaseCtx, cancel := r.keepCatalogLease(ctx, lease, acquiredAt)
	defer cancel(nil)
	accounts, err := load(leaseCtx)
	if err != nil {
		slog.Warn("kiro_model_catalog_list_accounts_failed", "error", err)
		return
	}
	now := r.now()
	var priority, regular []*Account
	seen := make(map[int64]bool, len(accounts))
	for _, a := range accounts {
		if leaseCtx.Err() != nil {
			return
		}
		if a == nil || a.Platform != PlatformKiro || a.Type != AccountTypeOAuth || a.IsShadow() || seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		catalog, exists := a.kiroModelCatalog()
		if !r.due(a, catalog, now) {
			continue
		}
		state := catalog.EffectiveState(now)
		if !exists || state == kiro.CatalogStateUnknown || catalog.ScopeFingerprint != a.kiroCatalogScopeFingerprint() {
			priority = append(priority, a)
		} else {
			regular = append(regular, a)
		}
	}
	r.probeBatch(leaseCtx, priority)
	r.probeBatch(leaseCtx, regular)
}

func (r *KiroModelCatalogRefresher) due(account *Account, catalog kiro.ModelCatalog, now time.Time) bool {
	next, nextErr := time.Parse(time.RFC3339Nano, catalog.NextAttemptAt)
	if nextErr == nil && now.Before(next) {
		return false
	}
	id := account.ID
	r.mu.Lock()
	defer r.mu.Unlock()
	at, _ := time.Parse(time.RFC3339Nano, catalog.LastAttemptAt)
	if r.attempts[id].After(at) {
		at = r.attempts[id]
	}
	if catalog.LastErrorCode != "" || catalog.ConsecutiveFailures > 0 || r.failures[id] > 0 {
		interval := r.failureBackoffBase
		for n := 1; n < max(catalog.ConsecutiveFailures, r.failures[id]) && interval < r.failureBackoffMax; n++ {
			interval = min(interval*2, r.failureBackoffMax)
		}
		interval = min(interval, r.failureBackoffMax)
		if retry := r.retryAfter[id]; retry > interval {
			interval = retry
		}
		return !now.Before(at.Add(interval))
	}
	if nextErr == nil || r.earlyPending[id] || catalog.EffectiveState(now) == kiro.CatalogStateUnknown || catalog.ScopeFingerprint != account.kiroCatalogScopeFingerprint() {
		return true
	}
	interval := time.Duration(float64(r.baseInterval) * (1 + r.jitterFraction*r.jitter(id)))
	return !now.Before(at.Add(interval))
}

func (r *KiroModelCatalogRefresher) probeBatch(ctx context.Context, accounts []*Account) {
	slots := make(chan struct{}, max(1, r.maxConcurrency))
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			if probeCtx.Err() != nil {
				return
			}
			started := r.now()
			r.mu.Lock()
			delete(r.earlyPending, a.ID)
			r.mu.Unlock()
			defer func() {
				if recover() != nil {
					r.recordAttempt(a.ID, started, errors.New("kiro catalog probe panic"))
					slog.Warn("kiro_model_catalog_probe_panic", "account_id", a.ID)
				}
			}()
			_, err := r.prober.RefreshKiroModelCatalog(probeCtx, a, started)
			r.recordAttempt(a.ID, started, err)
			if err != nil {
				slog.Warn("kiro_model_catalog_probe_failed", "account_id", a.ID, "error", err)
			}
		}()
	}
}

func (r *KiroModelCatalogRefresher) recordAttempt(id int64, started time.Time, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts[id] = started
	delete(r.retryAfter, id)
	if err == nil {
		delete(r.failures, id)
		return
	}
	r.failures[id]++
	var httpErr *kiro.CatalogHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == 429 {
		r.retryAfter[id] = min(httpErr.RetryAfter, kiro.CatalogRetryAfterCap)
	}
}
