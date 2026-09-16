//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNonKiroCooldownKey_TempUnschedPreservesMappedCallerModel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		secondKey string
		secondVal string
	}{
		{"chained_mapping", "claude-upstream", "claude-other"},
		{"wildcard_fallback", "*", "claude-fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given: non-Kiro gateways map the client model before error handling.
			ctx := context.Background()
			repo := &modelNotFoundAccountRepoStub{}
			svc := &RateLimitService{accountRepo: repo}
			account := &Account{
				ID: 506, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"claude-public": "claude-upstream",
						tc.secondKey:    tc.secondVal,
					},
					"temp_unschedulable_enabled": true,
					"temp_unschedulable_rules": []any{map[string]any{
						"error_code": 400, "keywords": []string{"capacity unavailable"}, "duration_minutes": 5,
					}},
				},
			}
			mappedModel := account.GetMappedModel("claude-public")
			require.Equal(t, "claude-upstream", mappedModel)
			require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-public"))
			require.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-unrelated"))

			// When
			handled := svc.HandleUpstreamError(ctx, account, http.StatusBadRequest, http.Header{}, []byte(`{"message":"capacity unavailable"}`), mappedModel)

			// Then: replay the persisted cooldown through the real scheduler read.
			require.True(t, handled)
			require.Len(t, repo.modelRateLimitCalls, 1)
			call := repo.modelRateLimitCalls[0]
			setAccountModelRateLimitSnapshot(account, call.scope, call.resetAt, call.reason, time.Now())
			assert.Equal(t, "claude-upstream", call.scope, "already-mapped caller key must not be mapped again")
			assert.False(t, account.IsSchedulableForModelWithContext(ctx, "claude-public"), "the failing account must be excluded")
			assert.True(t, account.IsSchedulableForModelWithContext(ctx, "claude-unrelated"), "unrelated models must remain schedulable")
			require.Zero(t, repo.tempCalls)
		})
	}
}
