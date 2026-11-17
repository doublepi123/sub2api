-- Fork follow-up to 251_group_usage_rollup_nonblocking_build.sql.
-- 251 recreates the usage rollup invalidation triggers with the session
-- timezone, which reverts 228_group_usage_rollup_trigger_timezone.sql.
-- Database sessions may use a different timezone (CI and maintenance jobs
-- commonly use UTC), so invalidation must keep using the timezone that owns
-- the published daily buckets. The bodies below are 251's non-blocking
-- versions with only the timezone source changed.

CREATE OR REPLACE FUNCTION invalidate_group_usage_rollup_state()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    configured_timezone TEXT;
    old_date DATE;
    new_date DATE;
    affected_date DATE;
    state_closed_before DATE;
    state_building_until DATE;
    state_build_dirty_from DATE;
    state_expired_before TIMESTAMPTZ;
BEGIN
    SELECT closed_before, building_until, build_dirty_from, expired_before, timezone_name
    INTO state_closed_before, state_building_until, state_build_dirty_from, state_expired_before, configured_timezone
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR KEY SHARE;

    IF FOUND THEN
        configured_timezone := COALESCE(NULLIF(configured_timezone, ''), current_setting('TimeZone'));
        IF OLD.group_id IS NOT NULL AND OLD.created_at >= state_expired_before THEN
            old_date := (OLD.created_at AT TIME ZONE configured_timezone)::date;
        END IF;
        IF TG_OP = 'UPDATE' THEN
            IF NEW.group_id IS NOT NULL AND NEW.created_at >= state_expired_before THEN
                new_date := (NEW.created_at AT TIME ZONE configured_timezone)::date;
            END IF;
        END IF;

        affected_date := LEAST(old_date, new_date);
        IF state_closed_before > affected_date
            OR (
                state_building_until > affected_date
                AND (state_build_dirty_from IS NULL OR state_build_dirty_from > affected_date)
            ) THEN
            PERFORM apply_group_usage_rollup_invalidation(affected_date);
        END IF;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION invalidate_group_usage_rollup_state_after_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    configured_timezone TEXT;
    affected_date DATE;
    state_closed_before DATE;
    state_building_until DATE;
    state_build_dirty_from DATE;
    state_expired_before TIMESTAMPTZ;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM inserted_usage_logs WHERE group_id IS NOT NULL) THEN
        RETURN NULL;
    END IF;

    SELECT closed_before, building_until, build_dirty_from, expired_before, timezone_name
    INTO state_closed_before, state_building_until, state_build_dirty_from, state_expired_before, configured_timezone
    FROM usage_group_rollup_state
    WHERE id = 1
    FOR KEY SHARE;

    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    configured_timezone := COALESCE(NULLIF(configured_timezone, ''), current_setting('TimeZone'));

    SELECT MIN((created_at AT TIME ZONE configured_timezone)::date)
    INTO affected_date
    FROM inserted_usage_logs
    WHERE group_id IS NOT NULL
        AND created_at >= state_expired_before;

    IF state_closed_before > affected_date
        OR (
            state_building_until > affected_date
            AND (state_build_dirty_from IS NULL OR state_build_dirty_from > affected_date)
        ) THEN
        PERFORM apply_group_usage_rollup_invalidation(affected_date);
    END IF;

    RETURN NULL;
END;
$$;
