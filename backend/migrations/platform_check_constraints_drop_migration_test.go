package migrations

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 242_drop 与 243_drop 内容相同：242_drop 在全新库上先移除约束，
// 本 fork 的 242_restore_kiro_platform_checks.sql 随后按文件名顺序重建
// 包含 kiro 的约束（修复上游迁移反复丢掉 kiro 的历史问题），243_drop
// 作为最终收尾在两条迁移路径上都保证约束被移除，回到应用层校验。
func TestDropPlatformCheckConstraintsMigration(t *testing.T) {
	for _, name := range []string{
		"242_drop_platform_check_constraints.sql",
		"243_drop_platform_check_constraints.sql",
	} {
		t.Run(name, func(t *testing.T) {
			content, err := FS.ReadFile(name)
			require.NoError(t, err)

			sql := strings.Join(strings.Fields(string(content)), " ")
			require.Contains(t, sql,
				"ALTER TABLE user_platform_quotas DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;")
			require.Contains(t, sql,
				"ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;")
			require.NotContains(t, sql, "ADD CONSTRAINT")
			require.NotContains(t, sql, "DROP CONSTRAINT IF EXISTS channel_monitors_provider_check")
			require.NotContains(t, sql, "DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check")
		})
	}
}

// Platform membership is validated against the application catalog after the
// final drop migration. A later migration must not restore a database
// whitelist that can drift from it.
//
// Anchor on the final 243_drop_platform_check_constraints.sql and scan every
// migration that runs AFTER it (filename > 243...). Migrations ordered before
// the anchor are out of scope: this fork's 242_restore_kiro_platform_checks.sql
// (which runs between 242_drop and 243_drop on a fresh database) legitimately
// rebuilds the constraints including kiro; 243_drop removes them again, so the
// final schema state never carries a database platform whitelist. Guarding the
// pre-anchor range would force deleting immutable, already-applied history,
// which checksum integrity forbids.
func TestLaterMigrationsDoNotRestorePlatformCheckConstraints(t *testing.T) {
	const droppedAt = "243_drop_platform_check_constraints.sql"
	entries, err := fs.ReadDir(FS, ".")
	require.NoError(t, err)
	comments := regexp.MustCompile(`(?ms)/\*.*?\*/|--[^\n]*`)
	addConstraint := regexp.MustCompile(`(?i)\bADD\s+CONSTRAINT\s+"?(user_platform_quotas_platform_check|composite_model_routes_target_platform_check)\b`)
	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		// Only migrations that run after the final drop are forbidden from
		// re-adding the platform CHECK constraints.
		if entry.Name() <= droppedAt {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			content, err := FS.ReadFile(entry.Name())
			require.NoError(t, err)
			sql := comments.ReplaceAll(content, nil)
			require.Nil(t, addConstraint.Find(sql), "platform CHECK constraints removed in 243 must remain managed by application validation")
		})
	}
	// The anchor itself must exist and be scanned past; otherwise the guard
	// silently degrades (e.g. if the final drop migration gets renamed or
	// a future 244+ migration re-adds the constraints).
	for _, entry := range entries {
		if entry.Name() == droppedAt {
			found = true
		}
	}
	require.True(t, found, "final drop migration %s must exist in the embedded filesystem", droppedAt)
}
