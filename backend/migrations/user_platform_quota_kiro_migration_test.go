package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUserPlatformQuotasKiroMigration 校验 230 号迁移把 fork 独占的 kiro
// 加入 user_platform_quotas.platform 的 CHECK 约束（对照 157/224 号迁移）。
// 上游 224 的 CHECK 不含 kiro，注册预填充 9 平台默认配额会整条 INSERT 中止
// → 新用户零配额行（缺失配额行 = 无限额）。
func TestUserPlatformQuotasKiroMigration(t *testing.T) {
	content, err := FS.ReadFile("230_user_platform_quotas_add_kiro.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok', 'kimi', 'zhipu', 'deepseek'))")
}
