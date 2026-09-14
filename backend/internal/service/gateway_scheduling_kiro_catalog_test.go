//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

var kiroFreeCatalogIDs = []string{
	"auto", "claude-haiku-4.5", "claude-sonnet-4", "claude-sonnet-4.5",
	"deepseek-3.2", "glm-5", "minimax-m2.1", "minimax-m2.5", "qwen3-coder-next",
}

var kiroPaidCatalogIDs = append(append([]string{}, kiroFreeCatalogIDs...),
	"claude-opus-4.5", "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8", "claude-opus-5",
	"claude-sonnet-4.6", "claude-sonnet-5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra")

func kiroCatalogAccount(id int64, ids []string, lastSuccess time.Time, mode string) Account {
	return Account{
		ID: id, Platform: PlatformKiro, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 5,
		Extra: map[string]any{
			"kiro_model_catalog_mode": mode,
			"detected_model_catalog": map[string]any{
				"schema_version": kiro.CatalogSchemaVersion, "source": kiro.CatalogSource,
				"state": "ready", "model_ids": ids,
				"last_success_at":   lastSuccess.UTC().Format(time.RFC3339),
				"scope_fingerprint": kiro.ScopeFingerprint(kiro.ScopeInputs{}),
			},
		},
	}
}

func newKiroCatalogSchedulingService(accounts []Account) *GatewayService {
	repo := &mockAccountRepoForPlatform{accounts: accounts, accountsByID: make(map[int64]*Account, len(accounts))}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	return &GatewayService{
		accountRepo: repo, cache: &mockGatewayCacheForPlatform{}, cfg: cfg,
		concurrencyService: NewConcurrencyService(&mockConcurrencyCache{}),
	}
}

func TestKiroCatalogGate_FreeCatalog_RejectsOpus_when_enforce(t *testing.T) {
	// Given
	a := kiroCatalogAccount(1, kiroFreeCatalogIDs, time.Now(), "enforce")
	// When
	allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-opus-4-5")
	// Then
	require.False(t, allowed)
}

func TestKiroCatalogGate_FreeCatalog_AcceptsSonnet45_when_enforce(t *testing.T) {
	// Given
	a := kiroCatalogAccount(1, kiroFreeCatalogIDs, time.Now(), "enforce")
	// When
	allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-sonnet-4-5")
	// Then
	require.True(t, allowed)
}

func TestKiroCatalogGate_FreeCatalog_AcceptsHaiku45_when_enforce(t *testing.T) {
	// Given
	a := kiroCatalogAccount(1, kiroFreeCatalogIDs, time.Now(), "enforce")
	a.Extra["kiro_sched_tier"] = "free"
	// When
	allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-haiku-4-5")
	// Then
	require.True(t, allowed)
}

func TestKiroCatalogGate_PaidCatalog_AcceptsOpus_when_enforce(t *testing.T) {
	// Given
	a := kiroCatalogAccount(1, kiroPaidCatalogIDs, time.Now(), "enforce")
	// When
	allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-opus-4-5-20251101")
	// Then
	require.True(t, allowed)
}

func TestKiroCatalogGate_EmptyCatalog_SuspendsAccount_when_enforce(t *testing.T) {
	for _, model := range append(append([]string{}, kiroPaidCatalogIDs...), "new-model") {
		t.Run(model, func(t *testing.T) {
			// Given
			a := kiroCatalogAccount(1, []string{}, time.Now(), "enforce")
			// When
			allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, model)
			// Then
			require.False(t, allowed)
		})
	}
}

func TestKiroCatalogGate_Expired_RejectsInEnforce_LogsInShadow(t *testing.T) {
	for _, mode := range []string{"enforce", "shadow"} {
		t.Run(mode, func(t *testing.T) {
			// Given
			a := kiroCatalogAccount(1, kiroPaidCatalogIDs, time.Now().Add(-25*time.Hour), mode)
			logs := captureKiroCatalogLogs(t)
			// When
			allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-opus-4-5")
			// Then
			require.Equal(t, mode == "shadow", allowed)
			if mode == "shadow" {
				require.Contains(t, logs.String(), `"reason":"catalog_expired"`)
			}
		})
	}
}

func TestKiroCatalogGate_Unknown_AllowsInShadow_RejectsInEnforce(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce", ""} {
		t.Run(mode, func(t *testing.T) {
			// Given
			a := kiroCatalogAccount(1, nil, time.Now(), mode)
			delete(a.Extra, "detected_model_catalog")
			// When
			allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-opus-4-5")
			// Then
			require.Equal(t, mode != "enforce", allowed)
		})
	}
}

func TestKiroCatalogGate_FingerprintMismatch_TreatedAsUnknown(t *testing.T) {
	// Given
	a := kiroCatalogAccount(1, kiroPaidCatalogIDs, time.Now(), "enforce")
	a.Credentials = map[string]any{"region": "eu-west-1"}
	// When
	d := (&GatewayService{}).diagnoseSelectionFailure(context.Background(), &a, "claude-opus-4-5", PlatformKiro, nil, false)
	// Then
	require.Equal(t, "model_unsupported", d.Category)
	require.Contains(t, d.Detail, "reason=catalog_unknown")
}

func TestKiroCatalogGate_ModeOff_AllowsAndSkipsRepo(t *testing.T) {
	// Given
	a := kiroCatalogAccount(1, nil, time.Now(), "off")
	parentID := int64(2)
	a.ParentAccountID = &parentID
	repo := &mockAccountRepoForPlatform{}
	svc := &GatewayService{accountRepo: repo}
	// When
	allowed := svc.isModelSupportedByAccountWithContext(context.Background(), &a, "claude-opus-4-5")
	// Then
	require.True(t, allowed)
	require.Zero(t, repo.getByIDCalls)
}

func captureKiroCatalogLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestKiroCatalogGate_Shadow_LogsWouldReject_AllowsRequest(t *testing.T) {
	// Given
	a := kiroCatalogAccount(1, kiroFreeCatalogIDs, time.Now().Add(-time.Hour), "shadow")
	logs := captureKiroCatalogLogs(t)
	// When
	allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-opus-4-5")
	// Then
	require.True(t, allowed)
	require.Equal(t, 1, strings.Count(logs.String(), "kiro_catalog_shadow_would_reject"))
	var record struct {
		Message   string  `json:"msg"`
		AccountID int64   `json:"account_id"`
		Model     string  `json:"resolved_model"`
		Reason    string  `json:"reason"`
		Age       float64 `json:"catalog_age_s"`
		Source    string  `json:"source"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record))
	require.Equal(t, "kiro_catalog_shadow_would_reject", record.Message)
	require.Equal(t, a.ID, record.AccountID)
	require.Equal(t, "claude-opus-4.5", record.Model)
	require.Equal(t, "catalog_missing_model", record.Reason)
	require.InDelta(t, 3600, record.Age, 5)
	require.Equal(t, kiro.CatalogSource, record.Source)
}

func TestKiroCatalogGate_ShadowAccountUsesParentCatalog(t *testing.T) {
	// Given
	parent := kiroCatalogAccount(2, kiroFreeCatalogIDs, time.Now(), "enforce")
	child := kiroCatalogAccount(1, nil, time.Now(), "enforce")
	delete(child.Extra, "detected_model_catalog")
	child.ParentAccountID = &parent.ID
	repo := &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{2: &parent}}
	// When
	allowed := (&GatewayService{accountRepo: repo}).isModelSupportedByAccountWithContext(context.Background(), &child, "claude-opus-4-5")
	// Then
	require.False(t, allowed)
	require.Equal(t, 1, repo.getByIDCalls)
}

func TestKiroCatalogGate_OperatorMappingStillFiltersFirst(t *testing.T) {
	// Given
	a := kiroCatalogAccount(1, kiroPaidCatalogIDs, time.Now(), "enforce")
	a.Credentials = map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5"}}
	repo := &mockAccountRepoForPlatform{}
	a.ParentAccountID = &a.ID
	// When
	allowed := (&GatewayService{accountRepo: repo}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-opus-4-5")
	// Then
	require.False(t, allowed)
	require.Zero(t, repo.getByIDCalls)
}

func TestKiroCatalogGate_IgnoredForNonKiroPlatform(t *testing.T) {
	// Given
	a := kiroCatalogAccount(1, kiroFreeCatalogIDs, time.Now(), "enforce")
	a.Platform = PlatformAnthropic
	// When
	allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-opus-4-5")
	// Then
	require.True(t, allowed)
}

func TestSelectAccount_KiroOpus_RoutesToPaidCatalogAccount(t *testing.T) {
	// Given: equal priorities force the scheduler to exercise shuffled ties.
	svc := newKiroCatalogSchedulingService([]Account{
		kiroCatalogAccount(1, kiroFreeCatalogIDs, time.Now(), "enforce"),
		kiroCatalogAccount(2, kiroPaidCatalogIDs, time.Now(), "enforce"),
	})
	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformKiro)
	for attempt := range 50 {
		// When
		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-opus-4-5", nil, "", 0)
		// Then
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.ReleaseFunc)
		result.ReleaseFunc()
		require.Equal(t, int64(2), result.Account.ID, "attempt %d: free catalog account selected for Opus", attempt)
	}
}

func TestSelectAccount_KiroOpus_AllFreeCatalog_NoCandidates(t *testing.T) {
	// Given
	accounts := []Account{kiroCatalogAccount(1, kiroFreeCatalogIDs, time.Now(), "enforce"), kiroCatalogAccount(2, kiroFreeCatalogIDs, time.Now(), "enforce")}
	svc := newKiroCatalogSchedulingService(accounts)
	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformKiro)
	// When
	result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-opus-4-5", nil, "", 0)
	// Then
	if result != nil && result.ReleaseFunc != nil {
		result.ReleaseFunc()
	}
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, result)
	for i := range accounts {
		d := svc.diagnoseSelectionFailure(ctx, &accounts[i], "claude-opus-4-5", PlatformKiro, nil, false)
		require.Equal(t, "model_unsupported", d.Category)
		require.Contains(t, d.Detail, "catalog_missing_model")
	}
}

func TestDiagnoseSelectionFailure_KiroCatalogSubReason(t *testing.T) {
	for _, reason := range []string{"catalog_missing_model", "catalog_unknown", "catalog_expired"} {
		t.Run(reason, func(t *testing.T) {
			// Given
			a := kiroCatalogAccount(1, kiroFreeCatalogIDs, time.Now(), "enforce")
			switch reason {
			case "catalog_unknown":
				delete(a.Extra, "detected_model_catalog")
			case "catalog_expired":
				a = kiroCatalogAccount(1, kiroFreeCatalogIDs, time.Now().Add(-25*time.Hour), "enforce")
			}
			// When
			d := (&GatewayService{}).diagnoseSelectionFailure(context.Background(), &a, "claude-opus-4-5", PlatformKiro, nil, false)
			// Then
			require.Equal(t, "model_unsupported", d.Category)
			require.Equal(t, "model=claude-opus-4.5 reason="+reason, d.Detail)
		})
	}
}
