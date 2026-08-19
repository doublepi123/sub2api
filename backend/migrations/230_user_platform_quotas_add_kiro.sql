-- 把 kiro 平台加入 user_platform_quotas.platform 的 CHECK 约束。
--
-- 背景：kiro 为 fork 独占平台，已进入 AllowedQuotaPlatforms
-- （internal/service/domain_constants.go），注册时 GetDefaultPlatformQuotas
-- 会预填充 9 平台默认配额行，但上游 224 号迁移的 CHECK 仍只允许 8 平台。
-- BulkInsertInitial 是单条多行 INSERT，任一违约行会中止整条语句 → 注册路径
-- fail-open 吞错 → 新用户拿到零条配额记录（缺失配额行 = 无限额）。
-- 不直接改上游 224 文件以保持其 checksum 稳定。
--
-- 修复：把约束与代码平台列表（PlatformKiro）对齐。
-- DROP ... IF EXISTS 保证可重入；新约束是旧约束的超集，存量行瞬时校验通过。
ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro',
                        'grok', 'kimi', 'zhipu', 'deepseek'));
