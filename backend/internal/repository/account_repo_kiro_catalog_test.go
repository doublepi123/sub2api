package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func TestUpdateKiroModelCatalogIfCurrent_OutboxFailureRollsBack(t *testing.T) {
	// Given
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET.*write_version.*kiro_credential_generation`).
		WithArgs(`{"detected_model_catalog":{"write_version":8}}`, int64(27), int64(7), int64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()
	mock.ExpectClose()
	repo := newAccountRepositoryWithSQL(client, db, nil)
	// When
	applied, err := repo.UpdateKiroModelCatalogIfCurrent(context.Background(), 27, map[string]any{"write_version": 8}, 7, 3)
	// Then
	require.EqualError(t, err, "outbox failed")
	require.False(t, applied)
}
