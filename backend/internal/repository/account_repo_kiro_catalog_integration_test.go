//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUpdateKiroModelCatalogIfCurrent_Conditions(t *testing.T) {
	for _, tc := range []struct {
		name                string
		extra               any
		version, generation int64
		applied             bool
	}{
		{"matching_number", `{"detected_model_catalog":{"write_version":7,"model_ids":["old"]},"kiro_credential_generation":3,"keep":true}`, 7, 3, true},
		{"matching_string", `{"detected_model_catalog":{"write_version":7},"kiro_credential_generation":"3"}`, 7, 3, true},
		{"stale_version", `{"detected_model_catalog":{"write_version":8,"model_ids":["newer"],"consecutive_failures":3},"kiro_credential_generation":3}`, 7, 3, false},
		{"changed_generation", `{"detected_model_catalog":{"write_version":7},"kiro_credential_generation":4}`, 7, 3, false},
		{"missing_keys", `{}`, 0, 0, true},
		{"null_extra", nil, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given: use PostgreSQL, including literal SQL NULL and both JSON scalar shapes.
			ctx := context.Background()
			tx := testEntTx(t)
			if tc.extra == nil {
				_, err := tx.ExecContext(ctx, "ALTER TABLE accounts ALTER COLUMN extra DROP NOT NULL")
				require.NoError(t, err)
			}
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			a := mustCreateAccount(t, tx.Client(), &service.Account{Name: tc.name})
			_, err := tx.ExecContext(ctx, "UPDATE accounts SET extra = $1::jsonb WHERE id = $2", tc.extra, a.ID)
			require.NoError(t, err)
			before, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			cache := &schedulerCacheRecorder{}
			repo.schedulerCache = cache
			catalog := map[string]any{"write_version": tc.version + 1, "model_ids": []string{"probe"}}
			// When
			applied, err := repo.UpdateKiroModelCatalogIfCurrent(ctx, a.ID, catalog, tc.version, tc.generation)
			// Then
			require.NoError(t, err)
			require.Equal(t, tc.applied, applied)
			got, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			var events int
			require.NoError(t, scanSingleRow(ctx, tx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", []any{a.ID}, &events))
			if tc.applied {
				require.Equal(t, map[string]any{"write_version": float64(tc.version + 1), "model_ids": []any{"probe"}}, got.Extra["detected_model_catalog"])
				for key, value := range before.Extra {
					if key != "detected_model_catalog" {
						require.Equal(t, value, got.Extra[key])
					}
				}
				require.Equal(t, 1, events)
				require.Len(t, cache.setAccounts, 1)
				require.Equal(t, got.Extra, cache.setAccounts[0].Extra)
			} else {
				require.Equal(t, before.Extra, got.Extra)
				require.Equal(t, before.UpdatedAt, got.UpdatedAt)
				require.Zero(t, events)
				require.Empty(t, cache.setAccounts)
			}
		})
	}
}
