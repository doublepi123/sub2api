//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestKiroCredentialGeneration_StoredIncrement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   any
		initial float64
	}{
		{"number", `{"kiro_credential_generation":7,"detected_model_catalog":{"write_version":4},"keep":true}`, 7},
		{"string", `{"kiro_credential_generation":"7","detected_model_catalog":{"write_version":4}}`, 7},
		{"missing", `{}`, 0},
		{"null_key", `{"kiro_credential_generation":null}`, 0},
		{"null_extra", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given: two writes derived from the same stale application snapshot.
			ctx := context.Background()
			tx := testEntTx(t)
			if tc.extra == nil {
				_, err := tx.ExecContext(ctx, "ALTER TABLE accounts ALTER COLUMN extra DROP NOT NULL")
				require.NoError(t, err)
			}
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			a := mustCreateAccount(t, tx.Client(), &service.Account{Name: tc.name, Platform: service.PlatformKiro})
			_, err := tx.ExecContext(ctx, "UPDATE accounts SET extra = $1::jsonb WHERE id = $2", tc.extra, a.ID)
			require.NoError(t, err)
			before, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			cache := &schedulerCacheRecorder{}
			repo.schedulerCache = cache
			// When / Then: bulk then single must each increment the stored value.
			_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Credentials: map[string]any{"access_token": "P"}, BumpKiroCredentialGeneration: true})
			require.NoError(t, err)
			first, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, tc.initial+1, first.Extra["kiro_credential_generation"])
			before.Credentials = map[string]any{"access_token": "F"}
			require.NoError(t, repo.UpdateWithKiroCredentialGeneration(ctx, before))
			second, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, tc.initial+2, second.Extra["kiro_credential_generation"])
			require.Equal(t, first.Extra["detected_model_catalog"], second.Extra["detected_model_catalog"])
			require.Equal(t, "F", second.Credentials["access_token"])
			require.Equal(t, second.Extra, before.Extra, "single update returns database generation")
			_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Credentials: map[string]any{"access_token": "Q"}, BumpKiroCredentialGeneration: true})
			require.NoError(t, err)
			third, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, tc.initial+3, third.Extra["kiro_credential_generation"])
			require.Equal(t, first.Extra["detected_model_catalog"], third.Extra["detected_model_catalog"])
			require.Len(t, cache.setAccounts, 3)
			require.Equal(t, third.Extra, cache.setAccounts[2].Extra)
			var events int
			require.NoError(t, scanSingleRow(ctx, tx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", []any{a.ID}, &events))
			require.Equal(t, 1, events)
		})
	}
}

func TestKiroCredentialGeneration_StaleOrdinaryUpdatePreservesManagedKeys(t *testing.T) {
	// Given: an edit reads before a concurrent credential and catalog write.
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	a := mustCreateAccount(t, tx.Client(), &service.Account{Name: "stale", Platform: service.PlatformKiro})
	stale, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra = '{"kiro_credential_generation":9,"detected_model_catalog":{"write_version":5}}'::jsonb WHERE id = $1`, a.ID)
	require.NoError(t, err)
	// When: a stale ordinary update submits older managed values.
	stale.Extra = map[string]any{"kiro_credential_generation": 1, "detected_model_catalog": map[string]any{"write_version": 1}, "custom": true}
	require.NoError(t, repo.Update(ctx, stale))
	// Then: the stored managed fields survive unchanged.
	require.Equal(t, float64(9), stale.Extra["kiro_credential_generation"])
	require.Equal(t, map[string]any{"write_version": float64(5)}, stale.Extra["detected_model_catalog"])
	require.Equal(t, true, stale.Extra["custom"])
}

func TestKiroCredentialGeneration_StoredPrincipalChangeWithoutSnapshotBump(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "bulk"}[bulk], func(t *testing.T) {
			// Given: the request looked unchanged against its snapshot, but P has since been installed.
			ctx := context.Background()
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			a := mustCreateAccount(t, tx.Client(), &service.Account{Name: "concurrent-noop", Platform: service.PlatformKiro, Credentials: map[string]any{"access_token": "P"}, Extra: map[string]any{"kiro_credential_generation": 7}})
			// When: the stale request writes F without having requested a bump.
			if bulk {
				_, err := repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Credentials: map[string]any{"access_token": "F"}})
				require.NoError(t, err)
			} else {
				a.Credentials["access_token"] = "F"
				require.NoError(t, repo.Update(ctx, a))
			}
			// Then: a stored principal change cannot reuse P's generation.
			got, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, float64(8), got.Extra["kiro_credential_generation"])
		})
	}
}
