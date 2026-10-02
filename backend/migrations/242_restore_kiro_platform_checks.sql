-- Restore 'kiro' in the platform CHECK constraints.
--
-- 230_user_platform_quotas_add_kiro.sql and 231_composite_routes_add_kiro.sql
-- added kiro, but later upstream migrations rebuild the same constraints from
-- their own platform lists, which do not include this fork-only platform:
-- 237_add_minimax_platform.sql, 238_opencode_go_platform.sql and
-- 241_add_typesafe_platform.sql. Each of them silently dropped kiro again, so
-- saving a kiro user quota or composite route violated the constraint.
--
-- This runs after 241 (files are applied in filename order) and rebuilds both
-- constraints as the union of every supported platform, keeping them aligned
-- with AllowedQuotaPlatforms in backend/internal/service/domain_constants.go.
-- DROP ... IF EXISTS keeps it re-entrant; the new set is a superset of 241, so
-- existing rows validate immediately.
--
-- Any future upstream migration that rebuilds these constraints needs a matching
-- fork migration after it that adds kiro back.

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'typesafe'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
                               'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'typesafe'));
