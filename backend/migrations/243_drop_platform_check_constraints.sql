-- Finalize application-managed platform validation after the immutable fork
-- migration 242_restore_kiro_platform_checks.sql. Fresh databases execute
-- 242_drop, 242_restore, then this final drop; production databases execute
-- the new 242_drop, checksum-validate and skip the recorded 242_restore,
-- then execute this migration. Channel-monitor provider checks are unchanged.

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;
