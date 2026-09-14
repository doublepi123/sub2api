package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

const kiroDetectedModelCatalogKey = "detected_model_catalog"

type kiroCatalogDecision string

const (
	kiroCatalogAllowed       kiroCatalogDecision = "allowed"
	kiroCatalogMissingModel  kiroCatalogDecision = "catalog_missing_model"
	kiroCatalogUnknown       kiroCatalogDecision = "catalog_unknown"
	kiroCatalogExpired       kiroCatalogDecision = "catalog_expired"
	kiroCatalogModeOffResult kiroCatalogDecision = "mode_off"
)

func (a *Account) kiroModelCatalog() (kiro.ModelCatalog, bool) {
	if a == nil {
		return kiro.ModelCatalog{}, false
	}
	return kiro.ParseModelCatalog(a.Extra[kiroDetectedModelCatalogKey])
}

func (a *Account) kiroCatalogScopeFingerprint() string {
	if a == nil {
		return ""
	}
	return kiro.ScopeFingerprint(kiro.ScopeInputs{
		Region: a.GetCredential("region"), ProfileARN: a.GetCredential("profile_arn"),
		AuthMethod: a.GetCredential("auth_method"), ClientID: a.GetCredential("client_id"),
		BaseURL: a.GetCredential("base_url"),
	})
}

func kiroUpstreamModel(account *Account, requested string) string {
	if account == nil {
		return kiro.NormalizeModelAlias(requested)
	}
	return kiro.NormalizeModelAlias(account.GetMappedModel(requested))
}

func (s *GatewayService) kiroCatalogSubject(ctx context.Context, account *Account) *Account {
	if !account.IsShadow() {
		return account
	}
	if s == nil || s.accountRepo == nil {
		return nil
	}
	parent, err := s.accountRepo.GetByID(ctx, *account.ParentAccountID)
	if err != nil {
		return nil
	}
	return parent
}

func (s *GatewayService) kiroCatalogDecide(ctx context.Context, account *Account, requestedModel string) (kiroCatalogDecision, bool) {
	decision, allowed, _ := s.kiroCatalogEvaluate(ctx, account, requestedModel)
	return decision, allowed
}

func (s *GatewayService) kiroCatalogEvaluate(ctx context.Context, account *Account, requestedModel string) (decision kiroCatalogDecision, allowed bool, ageSeconds int64) {
	var settings *SettingService
	if s != nil {
		settings = s.settingService
	}
	mode := resolveKiroCatalogMode(settings.GetKiroModelCatalogRuntime(ctx), account)
	if mode == kiroCatalogModeOff {
		return kiroCatalogModeOffResult, true, -1
	}
	allowed = mode == kiroCatalogModeShadow
	subject := s.kiroCatalogSubject(ctx, account)
	catalog, ok := subject.kiroModelCatalog()
	if !ok {
		return kiroCatalogUnknown, allowed, -1
	}
	now := time.Now()
	ageSeconds = -1
	if lastSuccess, err := time.Parse(time.RFC3339, catalog.LastSuccessAt); err == nil {
		ageSeconds = int64(now.Sub(lastSuccess).Seconds())
	}
	if fingerprint := subject.kiroCatalogScopeFingerprint(); catalog.ScopeFingerprint != "" && fingerprint != "" && catalog.ScopeFingerprint != fingerprint {
		return kiroCatalogUnknown, allowed, ageSeconds
	}
	switch catalog.EffectiveState(now) {
	case kiro.CatalogStateUnknown:
		return kiroCatalogUnknown, allowed, ageSeconds
	case kiro.CatalogStateExpired:
		return kiroCatalogExpired, allowed, ageSeconds
	case kiro.CatalogStateReady:
		if !catalog.Contains(kiroUpstreamModel(account, requestedModel)) {
			return kiroCatalogMissingModel, allowed, ageSeconds
		}
		return kiroCatalogAllowed, true, ageSeconds
	default:
		return kiroCatalogUnknown, allowed, ageSeconds
	}
}
