package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

func (s *AccountUsageService) SetKiroModelCatalogFetcher(fetcher KiroModelCatalogFetcher) {
	s.kiroCatalogFetcher = fetcher
}

func (s *AccountUsageService) RefreshKiroModelCatalog(ctx context.Context, account *Account, probeStartedAt time.Time) (kiro.ModelCatalog, error) {
	if account == nil || account.Platform != PlatformKiro {
		return kiro.ModelCatalog{}, errors.New("kiro account is required")
	}
	previous, _ := kiro.ParseModelCatalog(account.Extra[kiroDetectedModelCatalogKey])
	if s.kiroCatalogFetcher == nil || s.accountRepo == nil {
		return previous, errors.New("kiro model catalog fetcher and repository are required")
	}
	fingerprint := account.kiroCatalogScopeFingerprint()
	ids, probeErr := s.kiroCatalogFetcher.FetchKiroAvailableModels(ctx, account)
	if errors.Is(kiroCatalogLeaseCause(ctx), ErrKiroCatalogLeaseLost) {
		return previous, errors.Join(probeErr, ErrKiroCatalogLeaseLost)
	}
	persistCtx, cancel := kiroCatalogPersistenceContext(ctx)
	defer cancel()
	fresh, err := s.accountRepo.GetByID(persistCtx, account.ID)
	if errors.Is(kiroCatalogLeaseCause(ctx), ErrKiroCatalogLeaseLost) {
		return previous, errors.Join(probeErr, ErrKiroCatalogLeaseLost)
	}
	if err != nil {
		return previous, errors.Join(probeErr, fmt.Errorf("reload kiro model catalog: %w", err))
	}
	if fresh == nil {
		return previous, errors.Join(probeErr, errors.New("kiro account no longer exists"))
	}
	catalog, _ := kiro.ParseModelCatalog(fresh.Extra[kiroDetectedModelCatalogKey])
	lastSuccess, _ := time.Parse(time.RFC3339Nano, catalog.LastSuccessAt)
	// Permit migration of the original old-scope catalog, but reject a scope changed during the probe.
	scopeChanged := catalog.ScopeFingerprint != fingerprint && catalog.ScopeFingerprint != previous.ScopeFingerprint
	if lastSuccess.After(probeStartedAt) || fresh.kiroCatalogScopeFingerprint() != fingerprint || scopeChanged {
		slog.Warn("kiro_model_catalog_stale_write_skipped", "account_id", account.ID)
		return catalog, nil
	}
	now := time.Now().UTC()
	catalog.LastAttemptAt = now.Format(time.RFC3339Nano)
	if probeErr == nil {
		catalog.SchemaVersion = kiro.CatalogSchemaVersion
		catalog.Source = kiro.CatalogSource
		catalog.ModelIDs = append([]string{}, ids...)
		catalog.ScopeFingerprint = fingerprint
		catalog.LastSuccessAt = catalog.LastAttemptAt
		catalog.State = kiro.CatalogStateReady
		catalog.LastErrorCode = ""
		catalog.ConsecutiveFailures = 0
		catalog.NextAttemptAt = ""
	} else {
		catalog.ConsecutiveFailures++
		var retryAfter time.Duration
		var httpErr *kiro.CatalogHTTPError
		if errors.As(probeErr, &httpErr) && httpErr.StatusCode == 429 {
			retryAfter = httpErr.RetryAfter
		}
		next, capped := kiro.CatalogNextAttempt(now, catalog.ConsecutiveFailures, retryAfter)
		catalog.NextAttemptAt = next.Format(time.RFC3339Nano)
		if capped {
			slog.Warn("kiro_model_catalog_retry_after_capped", "account_id", account.ID, "retry_after_s", retryAfter.Seconds(), "cap_s", kiro.CatalogRetryAfterCap.Seconds())
		}
		catalog.LastErrorCode = kiroCatalogErrorCode(probeErr)
		if !lastSuccess.IsZero() && now.Sub(lastSuccess) > kiro.CatalogMaxAge {
			catalog.State = kiro.CatalogStateExpired
		} else if catalog.State == "" {
			catalog.State = kiro.CatalogStateUnknown
		}
	}
	body, err := json.Marshal(catalog)
	if err != nil {
		return previous, errors.Join(probeErr, fmt.Errorf("encode kiro model catalog: %w", err))
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return previous, errors.Join(probeErr, fmt.Errorf("encode kiro model catalog map: %w", err))
	}
	if errors.Is(kiroCatalogLeaseCause(ctx), ErrKiroCatalogLeaseLost) {
		return previous, errors.Join(probeErr, ErrKiroCatalogLeaseLost)
	}
	if err := s.persistSchedulerExtraUpdates(persistCtx, account, map[string]any{kiroDetectedModelCatalogKey: value}, "kiro_model_catalog_persist_failed"); err != nil {
		return catalog, errors.Join(probeErr, err)
	}
	return catalog, probeErr
}

func kiroCatalogErrorCode(err error) string {
	var httpErr *kiro.CatalogHTTPError
	var fetchErr *kiro.CatalogFetchError
	switch {
	case errors.As(err, &httpErr):
		switch {
		case httpErr.StatusCode == 401:
			return "http_401"
		case httpErr.StatusCode == 429:
			return "http_429"
		case httpErr.StatusCode >= 500 && httpErr.StatusCode < 600:
			return "http_5xx"
		default:
			return "network"
		}
	case errors.As(err, &fetchErr):
		return fetchErr.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "network"
	}
}
