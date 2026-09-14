//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const kiroInvalidModelTestBody = `{"message":"Invalid model ID or insufficient subscription level to use it.","reason":"INVALID_MODEL_ID"}`

type kiroInvalidModelRepo struct {
	modelNotFoundAccountRepoStub
	extraKeys []string
}

func (r *kiroInvalidModelRepo) UpdateExtra(_ context.Context, _ int64, extra map[string]any) error {
	for key := range extra {
		r.extraKeys = append(r.extraKeys, key)
	}
	return nil
}

func TestHandleUpstreamModelNotFound_KiroInvalidModelID_CooldownOnly_NoTierWrite(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		body     string
		handled  bool
	}{
		{"kiro invalid model", PlatformKiro, kiroInvalidModelTestBody, true},
		{"kiro phrase only", PlatformKiro, `{"message":"insufficient subscription level"}`, true},
		{"other platform", PlatformAnthropic, kiroInvalidModelTestBody, false},
		{"unrelated bad request", PlatformKiro, `{"message":"Invalid request format"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given
			repo := &kiroInvalidModelRepo{}
			svc := &RateLimitService{accountRepo: repo}
			account := &Account{
				ID: 501, Platform: tc.platform, Type: AccountTypeOAuth,
				Credentials: map[string]any{"model_mapping": map[string]any{"public-opus": "claude-opus-4-5"}},
				Extra:       map[string]any{"kiro_sched_tier": "PAID", "operator_note": "PAID"},
			}
			before := time.Now()

			// When
			handled := svc.HandleUpstreamModelNotFound(context.Background(), account, "public-opus", http.StatusBadRequest, []byte(tc.body))

			// Then
			require.Equal(t, map[string]any{"kiro_sched_tier": "PAID", "operator_note": "PAID"}, account.Extra)
			for _, key := range repo.extraKeys {
				require.NotContains(t, strings.ToLower(key), "tier", "UpdateExtra must never write tier keys")
			}
			require.Equal(t, tc.handled, handled)
			if !tc.handled {
				require.Empty(t, repo.modelRateLimitCalls)
				return
			}
			require.Len(t, repo.modelRateLimitCalls, 1)
			call := repo.modelRateLimitCalls[0]
			require.Equal(t, account.ID, call.accountID)
			require.Equal(t, account.GetMappedModel("public-opus"), call.scope)
			require.Equal(t, "upstream_400_kiro_invalid_model", call.reason)
			require.Equal(t, kiroInvalidModelReason, call.reason)
			require.False(t, call.resetAt.Before(before.Add(30*time.Minute)))
			require.False(t, call.resetAt.After(time.Now().Add(30*time.Minute)))
			require.Zero(t, repo.tempCalls)
		})
	}
}

func TestKiroCooldownKey_WriteMatchesSchedulerRead(t *testing.T) {
	for _, consumer := range []string{"A", "B"} {
		t.Run(consumer, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				client  string
				mapping map[string]any
				key     string
			}{
				{"no mapping", "claude-opus-4-5", map[string]any{}, "claude-opus-4-5"},
				{"single mapping", "claude-opus-4-5", map[string]any{"claude-opus-4-5": "claude-opus-4.5"}, "claude-opus-4.5"},
				{"chained mapping", "a", map[string]any{"a": "b", "b": "c"}, "b"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// Given
					ctx := context.Background()
					repo := &kiroInvalidModelRepo{}
					svc := &RateLimitService{accountRepo: repo}
					account := &Account{
						ID: 502, Platform: PlatformKiro, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
						Credentials: map[string]any{"model_mapping": tc.mapping,
							"access_token": "valid-access", "refresh_token": "rt-kiro", "region": "us-east-1",
							"expires_at":                 time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
							"temp_unschedulable_enabled": true,
							"temp_unschedulable_rules":   []any{map[string]any{"error_code": 400, "keywords": []string{"capacity unavailable"}, "duration_minutes": 5}},
						},
						Extra: map[string]any{"kiro_sched_tier": "PAID"},
					}
					require.True(t, account.IsSchedulableForModelWithContext(ctx, tc.client))

					// When: persist the recorded cooldown in the repository's JSON shape.
					switch consumer {
					case "A":
						upstream := &kiroForwardUpstream{response: &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(kiroInvalidModelTestBody))}}
						gateway := &GatewayService{httpUpstream: upstream, rateLimitService: svc}
						parsed := mustParseKiroSeedRequest(t, `{"model":"`+tc.client+`","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, nil)
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
						_, err := gateway.forwardKiro(ctx, c, account, parsed)
						var failover *UpstreamFailoverError
						require.ErrorAs(t, err, &failover)
					case "B":
						require.True(t, svc.HandleUpstreamError(ctx, account, 400, http.Header{}, []byte(`{"message":"capacity unavailable"}`), tc.client))
					}
					require.Len(t, repo.modelRateLimitCalls, 1)
					call := repo.modelRateLimitCalls[0]
					account.Extra["model_rate_limits"] = map[string]any{
						call.scope: map[string]any{
							"rate_limited_at":     time.Now().UTC().Format(time.RFC3339),
							"rate_limit_reset_at": call.resetAt.UTC().Format(time.RFC3339),
							"reason":              call.reason,
						},
					}

					// Then
					assert.Equal(t, tc.key, call.scope)
					assert.False(t, account.IsSchedulableForModelWithContext(ctx, tc.client))
					require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-sonnet-4-5"))
					require.Zero(t, repo.tempCalls)
				})
			}
		})
	}
}

func TestKiroCooldownKey_TempUnschedSharedPaths(t *testing.T) {
	for _, tc := range []struct {
		name, platform, client, mapped, key, unrelated string
		thinking                                       bool
	}{
		{"Anthropic gateway", PlatformAnthropic, "public-opus", "claude-opus-4.5", "claude-opus-4.5", "claude-sonnet-4-5", false},
		{"OpenAI image family", PlatformOpenAI, "public-image", "gpt-image-1", "gpt-image-1", "gpt-image-1.5", false},
		{"Anthropic fable family", PlatformAnthropic, "public-fable", "claude-fable-5[1m]", "claude-fable-5[1m]", "claude-fable-5", false},
		{"Antigravity thinking", PlatformAntigravity, "public-sonnet", "claude-sonnet-4-5", "claude-sonnet-4-5-thinking", "gemini-2.5-flash", true},
		{"Antigravity gemini family", PlatformAntigravity, "public-gemini", "gemini-2.5-pro", "gemini-2.5-pro", "gemini-2.5-flash", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given
			ctx := WithThinkingEnabled(context.Background(), tc.thinking, false)
			repo := &kiroInvalidModelRepo{}
			account := &Account{ID: 504, Platform: tc.platform, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{
					"model_mapping":              map[string]any{tc.client: tc.mapped, tc.unrelated: tc.unrelated},
					"temp_unschedulable_enabled": true,
					"temp_unschedulable_rules":   []any{map[string]any{"error_code": 400, "keywords": []string{"capacity unavailable"}, "duration_minutes": 5}},
				}}
			svc := &GatewayService{rateLimitService: &RateLimitService{accountRepo: repo}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"message":"capacity unavailable"}`))}
			defer resp.Body.Close()
			require.True(t, account.IsSchedulableForModelWithContext(ctx, tc.client))
			require.True(t, account.IsSchedulableForModelWithContext(ctx, tc.unrelated))

			// When: non-Kiro callers supply the final mapped key, including thinking resolution.
			_, err := svc.handleErrorResponse(ctx, resp, c, account, tc.key)

			// Then: only the primary key is persisted, even when reads include family keys.
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Len(t, repo.modelRateLimitCalls, 1)
			call := repo.modelRateLimitCalls[0]
			setAccountModelRateLimitSnapshot(account, call.scope, call.resetAt, call.reason, time.Now())
			assert.Equal(t, tc.key, call.scope)
			assert.False(t, account.IsSchedulableForModelWithContext(ctx, tc.client))
			require.True(t, account.IsSchedulableForModelWithContext(ctx, tc.unrelated))
			require.Zero(t, repo.tempCalls)
		})
	}
}

func TestKiroCooldownKey_TempUnschedBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, platform, client, keyword string
		status, accountWrites           int
		handled                         bool
	}{
		{"empty model", PlatformAntigravity, "", "capacity", 400, 1, true},
		{"401", PlatformAntigravity, "public-model", "capacity", 401, 1, true},
		{"unmatched rule", PlatformAntigravity, "public-model", "different", 400, 0, false},
		{"empty derived key", PlatformKiro, "unsupported-model", "capacity", 400, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given: Antigravity permits first-401 rule handling without DB fallback.
			repo := &kiroInvalidModelRepo{}
			svc := &RateLimitService{accountRepo: repo}
			account := &Account{ID: 505, Platform: tc.platform, Credentials: map[string]any{
				"model_mapping":              map[string]any{"public-model": "gemini-2.5-pro", "unsupported-model": " "},
				"temp_unschedulable_enabled": true,
				"temp_unschedulable_rules":   []any{map[string]any{"error_code": tc.status, "keywords": []string{tc.keyword}, "duration_minutes": 5}},
			}}
			// When
			handled := svc.HandleTempUnschedulable(context.Background(), account, tc.status, []byte(`{"message":"capacity unavailable"}`), tc.client)
			// Then
			require.Equal(t, tc.handled, handled)
			require.Equal(t, tc.accountWrites, repo.tempCalls)
			require.Empty(t, repo.modelRateLimitCalls)
		})
	}
}

func TestHandleUpstreamModelNotFound_KiroInvalidModelID_TriggersEarlyRefreshHint(t *testing.T) {
	// Given
	repo := &kiroInvalidModelRepo{}
	svc := &RateLimitService{accountRepo: repo}
	account := &Account{
		ID: 501, Platform: PlatformKiro, Type: AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{"public-opus": "claude-opus-4-5"}},
	}
	var hinted []int64
	svc.SetKiroCatalogEarlyRefresh(func(accountID int64) {
		hinted = append(hinted, accountID)
	})
	before := time.Now()

	// When
	handled := svc.HandleUpstreamModelNotFound(context.Background(), account, "public-opus", http.StatusBadRequest, []byte(kiroInvalidModelTestBody))

	// Then
	require.True(t, handled)
	require.Equal(t, []int64{account.ID}, hinted)
	require.Len(t, repo.modelRateLimitCalls, 1)
	call := repo.modelRateLimitCalls[0]
	require.Equal(t, account.ID, call.accountID)
	require.Equal(t, account.GetMappedModel("public-opus"), call.scope)
	require.Equal(t, kiroInvalidModelReason, call.reason)
	require.False(t, call.resetAt.Before(before.Add(30*time.Minute)))
	require.False(t, call.resetAt.After(time.Now().Add(30*time.Minute)))
}

func TestHandleUpstreamModelNotFound_KiroInvalidModelID_NeverMutatesCatalog(t *testing.T) {
	// Given
	repo := &kiroInvalidModelRepo{}
	svc := &RateLimitService{accountRepo: repo}
	catalog := map[string]any{
		"schema_version": 1,
		"model_ids":      []any{"claude-sonnet-4-5", "claude-opus-4-5"},
		"state":          "ready",
	}
	account := &Account{
		ID: 501, Platform: PlatformKiro, Type: AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{"public-opus": "claude-opus-4-5"}},
		Extra:       map[string]any{kiroDetectedModelCatalogKey: catalog},
	}
	before, err := json.Marshal(account.Extra[kiroDetectedModelCatalogKey])
	require.NoError(t, err)
	svc.SetKiroCatalogEarlyRefresh(func(int64) {})

	// When
	handled := svc.HandleUpstreamModelNotFound(context.Background(), account, "public-opus", http.StatusBadRequest, []byte(kiroInvalidModelTestBody))

	// Then
	require.True(t, handled)
	for _, key := range repo.extraKeys {
		require.NotEqual(t, kiroDetectedModelCatalogKey, key)
	}
	after, err := json.Marshal(account.Extra[kiroDetectedModelCatalogKey])
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestHandleUpstreamModelNotFound_NilHookIsSafe(t *testing.T) {
	// Given
	repo := &kiroInvalidModelRepo{}
	svc := &RateLimitService{accountRepo: repo}
	account := &Account{
		ID: 501, Platform: PlatformKiro, Type: AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{"public-opus": "claude-opus-4-5"}},
	}
	before := time.Now()

	// When / Then: nil hook must not panic.
	require.NotPanics(t, func() {
		require.True(t, svc.HandleUpstreamModelNotFound(context.Background(), account, "public-opus", http.StatusBadRequest, []byte(kiroInvalidModelTestBody)))
	})
	require.Len(t, repo.modelRateLimitCalls, 1)
	call := repo.modelRateLimitCalls[0]
	require.Equal(t, account.ID, call.accountID)
	require.Equal(t, account.GetMappedModel("public-opus"), call.scope)
	require.Equal(t, kiroInvalidModelReason, call.reason)
	require.False(t, call.resetAt.Before(before.Add(30*time.Minute)))
	require.False(t, call.resetAt.After(time.Now().Add(30*time.Minute)))
}

func TestHandleUpstreamModelNotFound_NonKiro_DoesNotTriggerHint(t *testing.T) {
	// Given
	repo := &kiroInvalidModelRepo{}
	svc := &RateLimitService{accountRepo: repo}
	account := openAICodexPlanGatedOAuthAccount()
	var hinted []int64
	svc.SetKiroCatalogEarlyRefresh(func(accountID int64) {
		hinted = append(hinted, accountID)
	})

	// When
	handled := svc.HandleUpstreamModelNotFound(
		context.Background(),
		account,
		"gpt-5.6-sol",
		http.StatusBadRequest,
		[]byte(`{"detail":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}`),
	)

	// Then
	require.True(t, handled)
	require.Empty(t, hinted)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, upstreamCodexPlanGatedModelReason, repo.modelRateLimitCalls[0].reason)
}
