-- 把 kiro 平台加入 composite_model_routes.target_platform 的 CHECK 约束。
--
-- 背景：fork 独占的 kiro 已进入 handler/service 白名单
-- （group_handler TargetPlatform oneof、isConcreteRequestPlatform），
-- 但上游 227 号迁移把 CHECK 扩到 CN 三家时漏了 kiro。
-- 不直接改 227 以保持其 checksum 稳定。
--
-- 修复：把约束与代码平台列表对齐。DROP ... IF EXISTS 保证可重入；
-- 新约束是旧约束的超集，存量行瞬时校验通过。
ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
                               'kimi', 'zhipu', 'deepseek'));
