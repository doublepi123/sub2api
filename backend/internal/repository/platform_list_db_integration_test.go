//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func platformConstraintExists(t *testing.T, ctx context.Context, client *dbent.Client, table, constraint string) bool {
	t.Helper()
	rows, err := client.QueryContext(ctx, `
SELECT 1
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public' AND tbl.relname = $1 AND c.conname = $2`, table, constraint)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	exists := rows.Next()
	require.NoError(t, rows.Err())
	return exists
}

// 迁移 243 最终删除平台白名单 CHECK 约束：旧库（约束仍在）上可重复执行，执行后
// 新登记的平台无需再做数据库迁移即可写入。
func TestMigration243DropsPlatformCheckConstraints(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	// 还原迁移 241_add_typesafe_platform 之后、242 之前的约束状态。
	for _, stmt := range []string{
		`ALTER TABLE user_platform_quotas DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check`,
		`ALTER TABLE user_platform_quotas ADD CONSTRAINT user_platform_quotas_platform_check
			CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
			                    'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'typesafe'))`,
		`ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check`,
		`ALTER TABLE composite_model_routes ADD CONSTRAINT composite_model_routes_target_platform_check
			CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
			                           'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'typesafe'))`,
	} {
		_, err := client.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	require.True(t, platformConstraintExists(t, ctx, client, "user_platform_quotas", "user_platform_quotas_platform_check"))
	require.True(t, platformConstraintExists(t, ctx, client, "composite_model_routes", "composite_model_routes_target_platform_check"))

	migrationSQL, err := dbmigrations.FS.ReadFile("243_drop_platform_check_constraints.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = client.ExecContext(ctx, string(migrationSQL))
		require.NoError(t, err)
	}
	require.False(t, platformConstraintExists(t, ctx, client, "user_platform_quotas", "user_platform_quotas_platform_check"))
	require.False(t, platformConstraintExists(t, ctx, client, "composite_model_routes", "composite_model_routes_target_platform_check"))
	// 渠道监控的 provider 约束表示探测能力，保留。
	require.True(t, platformConstraintExists(t, ctx, client, "channel_monitors", "channel_monitors_provider_check"))
	require.True(t, platformConstraintExists(t, ctx, client, "channel_monitor_request_templates", "channel_monitor_request_templates_provider_check"))

	// 数据库层不再限制平台取值：模拟未来新登记的平台。
	userID := mustCreateUserForQuota(t, client)
	_, err = client.ExecContext(ctx, `
INSERT INTO user_platform_quotas (user_id, platform, daily_limit_usd, daily_usage_usd, weekly_usage_usd, monthly_usage_usd, created_at, updated_at)
VALUES ($1, 'future_platform', 1, 0, 0, 0, NOW(), NOW())`, userID)
	require.NoError(t, err)
	group := mustCreateGroup(t, client, &service.Group{Name: "platform-list-composite", Platform: service.PlatformComposite})
	_, err = client.ExecContext(ctx, `
INSERT INTO composite_model_routes (group_id, public_model, target_platform)
VALUES ($1, 'future-model', 'future_platform')`, group.ID)
	require.NoError(t, err)
}

// TestMain applies the complete migration set to a fresh PostgreSQL database.
// Here recreate the production ledger: historical 242_restore is already
// recorded, while upstream 242_drop and final 243_drop have not run yet.
func TestMigrationsRunner_ProductionKiroLedger(t *testing.T) {
	ctx := context.Background()
	const restore = "242_restore_kiro_platform_checks.sql"
	content, err := dbmigrations.FS.ReadFile(restore)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(content))))
	wantChecksum := hex.EncodeToString(sum[:])
	var before string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT checksum FROM schema_migrations WHERE filename = $1", restore).Scan(&before))
	require.Equal(t, wantChecksum, before)
	_, err = integrationDB.ExecContext(ctx, string(content))
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE filename IN
		('242_drop_platform_check_constraints.sql', '243_drop_platform_check_constraints.sql')`)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ApplyMigrations(ctx, integrationDB)) })
	require.NoError(t, ApplyMigrations(ctx, integrationDB))
	var after string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT checksum FROM schema_migrations WHERE filename = $1", restore).Scan(&after))
	require.Equal(t, before, after, "historical restore checksum must remain unchanged")
	for _, name := range []string{"242_drop_platform_check_constraints.sql", "243_drop_platform_check_constraints.sql"} {
		var count int
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			"SELECT count(*) FROM schema_migrations WHERE filename = $1", name).Scan(&count))
		require.Equal(t, 1, count, name)
	}
	for _, name := range []string{"user_platform_quotas_platform_check", "composite_model_routes_target_platform_check"} {
		var count int
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			"SELECT count(*) FROM pg_constraint WHERE conname = $1", name).Scan(&count))
		require.Zero(t, count, name)
	}
}

func TestPlatformRepositories_WriteRegisteredKiroAndNewProviders(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	userID := mustCreateUserForQuota(t, client)
	group := mustCreateGroup(t, client, &service.Group{Name: "registered-platforms", Platform: service.PlatformComposite})
	quotaRepo := NewUserPlatformQuotaRepository(client)
	routeRepo := NewCompositeModelRouteRepository(client)
	daily := 5.0
	var records []UserPlatformQuotaRecord
	for _, platform := range []string{service.PlatformKiro, service.PlatformCline, service.PlatformCommandCode} {
		record := UserPlatformQuotaRecord{UserID: userID, Platform: platform, DailyLimitUSD: &daily}
		records = append(records, record)
		require.NoError(t, quotaRepo.BulkInsertInitial(txCtx, []UserPlatformQuotaRecord{record}), platform)
		route := &service.CompositeModelRoute{
			GroupID: group.ID, PublicModel: platform + "-model", MatchType: "exact", TargetPlatform: platform,
			UpstreamModel: "model", Endpoint: "any", Priority: 100, Enabled: true,
		}
		require.NoError(t, routeRepo.Create(txCtx, route), platform)
		require.NoError(t, routeRepo.Update(txCtx, route), platform)
	}
	require.NoError(t, quotaRepo.UpsertForUser(txCtx, userID, records))
	quotas, err := quotaRepo.ListByUser(txCtx, userID)
	require.NoError(t, err)
	require.Len(t, quotas, 3)
	routes, err := routeRepo.ListByGroup(txCtx, group.ID, true)
	require.NoError(t, err)
	require.Len(t, routes, 3)
}

// 删除 CHECK 约束后，平台合法性由应用层保证：原生 SQL 写入前的 repository 校验。
func TestUserPlatformQuotaRepository_RejectsUnregisteredPlatform(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	userID := mustCreateUserForQuota(t, client)
	repo := NewUserPlatformQuotaRepository(client)
	daily := 5.0
	require.NoError(t, repo.UpsertForUser(txCtx, userID, []UserPlatformQuotaRecord{
		{UserID: userID, Platform: service.PlatformOpenCodeGo, DailyLimitUSD: &daily},
	}))

	for _, platform := range []string{"bogus", "moonshot", "Kimi", service.PlatformComposite} {
		err := repo.BulkInsertInitial(txCtx, []UserPlatformQuotaRecord{
			{UserID: userID, Platform: service.PlatformAnthropic, DailyLimitUSD: &daily},
			{UserID: userID, Platform: platform, DailyLimitUSD: &daily},
		})
		require.ErrorContains(t, err, "is not allowed", platform)
		err = repo.UpsertForUser(txCtx, userID, []UserPlatformQuotaRecord{
			{UserID: userID, Platform: platform, DailyLimitUSD: &daily},
		})
		require.ErrorContains(t, err, "is not allowed", platform)
	}

	// 被拒绝的写入不产生任何副作用：既有记录保留、未新增任何行。
	got, err := repo.ListByUser(txCtx, userID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, service.PlatformOpenCodeGo, got[0].Platform)

	// 三档全空的记录不写入，也不参与校验（与既有 configuredRecords 语义一致）。
	require.NoError(t, repo.BulkInsertInitial(txCtx, []UserPlatformQuotaRecord{{UserID: userID, Platform: "bogus"}}))
}

func TestCompositeModelRouteRepository_RejectsUnregisteredTargetPlatform(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	group := mustCreateGroup(t, client, &service.Group{Name: "platform-list-routes", Platform: service.PlatformComposite})
	repo := NewCompositeModelRouteRepository(client)

	route := &service.CompositeModelRoute{
		GroupID: group.ID, PublicModel: "glm-5", MatchType: "exact", TargetPlatform: service.PlatformZhipu,
		UpstreamModel: "glm-5", Endpoint: "any", Priority: 100, Enabled: true,
	}
	require.NoError(t, repo.Create(txCtx, route))

	bad := &service.CompositeModelRoute{
		GroupID: group.ID, PublicModel: "bogus-model", MatchType: "exact", TargetPlatform: "bogus",
		UpstreamModel: "bogus-model", Endpoint: "any", Priority: 100, Enabled: true,
	}
	require.Error(t, repo.Create(txCtx, bad))

	route.TargetPlatform = service.PlatformComposite
	require.Error(t, repo.Update(txCtx, route))

	routes, err := repo.ListByGroup(txCtx, group.ID, true)
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, service.PlatformZhipu, routes[0].TargetPlatform)
}
