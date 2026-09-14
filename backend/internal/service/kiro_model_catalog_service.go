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

func kiroCatalogScope(account *Account) string {
	return kiro.ScopeFingerprint(kiro.ScopeInputs{
		Region: account.GetCredential("region"), ProfileARN: account.GetCredential("profile_arn"),
		AuthMethod: account.GetCredential("auth_method"), ClientID: account.GetCredential("client_id"),
		BaseURL: account.GetCredential("base_url"),
	})
}

func (s *AccountUsageService) RefreshKiroModelCatalog(ctx context.Context, account *Account, probeStartedAt time.Time) (kiro.ModelCatalog, error) {
	if account == nil || account.Platform != PlatformKiro {
		return kiro.ModelCatalog{}, errors.New("kiro account is required")
	}
	previous, _ := kiro.ParseModelCatalog(account.Extra[kiroDetectedModelCatalogKey])
	if s.kiroCatalogFetcher == nil || s.accountRepo == nil {
		return previous, errors.New("kiro model catalog fetcher and repository are required")
	}
	fingerprint := kiroCatalogScope(account)
	ids, probeErr := s.kiroCatalogFetcher.FetchKiroAvailableModels(ctx, account)
	persistCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		persistCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	fresh, err := s.accountRepo.GetByID(persistCtx, account.ID)
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
	if lastSuccess.After(probeStartedAt) || kiroCatalogScope(fresh) != fingerprint || scopeChanged {
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
	} else {
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
