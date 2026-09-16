package repository

import (
	"context"
	"encoding/json"
	"errors"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.KiroModelCatalogRepository = (*accountRepository)(nil)

// UpdateKiroModelCatalogIfCurrent atomically replaces the detected catalog only
// when the stored write version and credential generation still match what the
// prober observed. Returns false when another writer won the race.
func (r *accountRepository) UpdateKiroModelCatalogIfCurrent(ctx context.Context, id int64, catalog map[string]any, expectedWriteVersion int64, expectedCredentialGeneration int64) (bool, error) {
	payload, err := json.Marshal(map[string]any{"detected_model_catalog": catalog})
	if err != nil {
		return false, err
	}
	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if contextTx == nil {
		tx, err = r.client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return false, err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}
	result, err := client.ExecContext(ctx, `UPDATE accounts SET
		extra = COALESCE(extra, '{}'::jsonb) || $1::jsonb, updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
		AND COALESCE((extra->'detected_model_catalog'->>'write_version')::bigint, 0) = $3
		AND COALESCE((extra->>'kiro_credential_generation')::bigint, 0) = $4`,
		string(payload), id, expectedWriteVersion, expectedCredentialGeneration)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		return false, err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return false, err
		}
	}
	if contextTx == nil {
		r.syncSchedulerAccountSnapshot(baseCtx, id)
	}
	return true, nil
}
