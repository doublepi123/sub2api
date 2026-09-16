//go:build unit

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type kiroSnapshotAccountRepo struct {
	service.AccountRepository
	account service.Account
	lists   int
}

func (r *kiroSnapshotAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, groupID int64, platform string) ([]service.Account, error) {
	if groupID != 10 || platform != service.PlatformKiro {
		return nil, fmt.Errorf("unexpected scheduling bucket: %d/%s", groupID, platform)
	}
	r.lists++
	return []service.Account{r.account}, nil
}

func (r *kiroSnapshotAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	if id != r.account.ID {
		return nil, fmt.Errorf("unexpected account: %d", id)
	}
	return &r.account, nil
}

func kiroSnapshotFixture(mode string) service.Account {
	now := time.Now().UTC().Truncate(time.Second)
	fp := kiro.ScopeFingerprint(kiro.ScopeInputs{
		Region: "eu-west-1", ProfileARN: "arn:aws:codewhisperer:eu-west-1:123456789012:profile/test",
		AuthMethod: "idc", ClientID: "test-client", BaseURL: "https://example.invalid", PrincipalGeneration: "7",
	})
	return service.Account{
		ID: 10, Platform: service.PlatformKiro, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{10},
		Credentials: map[string]any{
			"region": "eu-west-1", "profile_arn": "arn:aws:codewhisperer:eu-west-1:123456789012:profile/test",
			"auth_method": "idc", "client_id": "test-client", "base_url": "https://example.invalid",
			"access_token": "test-access-token", "refresh_token": "test-refresh-token", "client_secret": "test-secret",
		},
		Extra: map[string]any{
			"kiro_model_catalog_mode": mode, "kiro_credential_generation": 7,
			"kiro_sched_tier": "free", "kiro_sched_tier_updated_at": now.Format(time.RFC3339),
			"kiro_sched_reset_at":         now.Add(time.Hour).Format(time.RFC3339),
			"kiro_sched_usage_updated_at": now.Format(time.RFC3339), "kiro_sched_utilization": 0.1,
			"unrelated_large_payload": strings.Repeat("x", 4096),
			"detected_model_catalog": map[string]any{
				"schema_version": kiro.CatalogSchemaVersion, "source": kiro.CatalogSource, "state": "ready",
				"model_ids": []string{"claude-sonnet-4.5"}, "scope_fingerprint": fp,
				"last_success_at": now.Format(time.RFC3339),
			},
		},
	}
}

func TestSelectAccount_KiroCatalogSnapshotData(t *testing.T) {
	for _, warm := range []bool{false, true} {
		for _, batch := range []bool{false, true} {
			for _, mode := range []string{"enforce", "shadow"} {
				t.Run(fmt.Sprintf("cache_hit=%t/load_batch=%t/%s", warm, batch, mode), func(t *testing.T) {
					// Given: a full repository account and the production Redis cache codec.
					ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, service.PlatformKiro)
					groupID := int64(10)
					repo := &kiroSnapshotAccountRepo{account: kiroSnapshotFixture(mode)}
					cache := newSchedulerCacheUnit(t)
					cfg := &config.Config{}
					cfg.Gateway.Scheduling.LoadBatchEnabled = batch
					cfg.Gateway.Scheduling.DbFallbackEnabled = true
					snapshot := service.NewSchedulerSnapshotService(cache, nil, repo, nil, cfg)
					if warm {
						_, _, err := snapshot.ListSchedulableAccounts(ctx, &groupID, service.PlatformKiro, true)
						require.NoError(t, err)
					}
					concurrency := service.NewConcurrencyService(NewConcurrencyCache(cache.rdb, 1, 1))
					gateway := service.NewGatewayService(
						repo, nil, nil, nil, nil, nil, nil, nil, cfg, snapshot, concurrency,
						nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
					)
					var logs bytes.Buffer
					previous := slog.Default()
					slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
					t.Cleanup(func() { slog.SetDefault(previous) })

					// When: real selection invokes the catalog gate before load acquisition/hydration.
					result, err := gateway.SelectAccountWithLoadAwareness(ctx, &groupID, "", "claude-opus-5", nil, "", 0)
					if result != nil && result.ReleaseFunc != nil {
						result.ReleaseFunc()
					}

					// Then: inspect the existing production diagnostic emitted AT the gate.
					var detail struct {
						Message    string   `json:"msg"`
						Reason     string   `json:"reason"`
						ExtraKeys  []string `json:"extra_keys"`
						ComputedFP string   `json:"computed_fp"`
						StoredFP   string   `json:"stored_fp"`
						ParseOK    bool     `json:"parse_ok"`
					}
					found := false
					for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
						require.NoError(t, json.Unmarshal([]byte(line), &detail))
						if detail.Message == "kiro_catalog_decision_detail" || detail.Message == "kiro_catalog_shadow_would_reject" {
							found = true
							t.Log(line)
							break
						}
					}
					require.True(t, found, "missing gate diagnostic: %s", logs.String())
					require.Contains(t, detail.ExtraKeys, "detected_model_catalog", "catalog stripped before the gate")
					require.True(t, detail.ParseOK)
					require.Equal(t, repo.account.Extra["detected_model_catalog"].(map[string]any)["scope_fingerprint"], detail.ComputedFP)
					require.Equal(t, detail.StoredFP, detail.ComputedFP)
					require.Equal(t, "catalog_missing_model", detail.Reason)
					require.Equal(t, 1, repo.lists, "warm selection must use the cached projection")
					if mode == "enforce" {
						require.ErrorIs(t, err, service.ErrNoAvailableAccounts)
						require.Nil(t, result)
					} else {
						require.NoError(t, err)
						require.NotNil(t, result)
					}
				})
			}
		}
	}
}

func TestSchedulerCacheKiroAccountMetadata(t *testing.T) {
	// Given: full account data including secrets and a large unrelated payload.
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	account := kiroSnapshotFixture("enforce")
	bucket := service.SchedulerBucket{GroupID: 10, Platform: service.PlatformKiro, Mode: service.SchedulerModeSingle}
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{account}))

	// When: account updates refresh metadata without rebuilding membership.
	account.Credentials["profile_arn"] = "updated-profile"
	account.Extra["kiro_credential_generation"] = 8
	require.NoError(t, cache.SetAccount(ctx, &account))
	candidates, hit, err := cache.GetSnapshot(ctx, bucket)

	// Then: all capability inputs survive, while heavy/secret data stays out.
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, candidates, 1)
	for _, key := range []string{"region", "profile_arn", "auth_method", "client_id", "base_url"} {
		require.Equal(t, account.Credentials[key], candidates[0].Credentials[key], key)
	}
	for key, value := range account.Extra {
		if key == "unrelated_large_payload" {
			continue
		}
		want, err := json.Marshal(value)
		require.NoError(t, err)
		got, err := json.Marshal(candidates[0].Extra[key])
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got), key)
	}
	for _, key := range []string{"access_token", "refresh_token", "client_secret"} {
		require.NotContains(t, candidates[0].Credentials, key)
	}
	require.NotContains(t, candidates[0].Extra, "unrelated_large_payload")
}
