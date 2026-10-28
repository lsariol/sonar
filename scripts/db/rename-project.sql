-- Rename a project in Sonar's history: everything recorded under the old
-- compose project name moves to the new one, so a renamed project keeps its
-- uptime and its strip of days. One transaction; it only previews (and rolls
-- back) unless apply=1 is given.
--
--   docker exec -i sparkdb psql -U Admin -d sonar_db -v old=botsuite -v new=lys -f - < rename-project.sql
--   docker exec -i sparkdb psql -U Admin -d sonar_db -v old=botsuite -v new=lys -v apply=1 -f - < rename-project.sql
--
-- Services merge by name. When both projects have exactly one service (a
-- plain rename), those two merge even if their names differ; any other
-- service moves across as it is. A minute recorded under both names (the
-- swap) keeps the new project's row; a day recorded under both adds up.
-- Run it once the old project's containers are gone, or Sonar records the
-- old name again.

\set ON_ERROR_STOP on
\if :{?old}
\else
  \warn 'Usage: psql -d sonar_db -v old=<old project> -v new=<new project> [-v apply=1] -f rename-project.sql'
  \quit
\endif
\if :{?new}
\else
  \warn 'Usage: psql -d sonar_db -v old=<old project> -v new=<new project> [-v apply=1] -f rename-project.sql'
  \quit
\endif

BEGIN;
SET LOCAL ROLE sonar_owner;
SELECT set_config('sonar_rename.old', :'old', true) AS rename_old, set_config('sonar_rename.new', :'new', true) AS rename_new \gset

DO $$
DECLARE
    old_p     text := current_setting('sonar_rename.old');
    new_p     text := current_setting('sonar_rename.new');
    old_count integer;
    new_count integer;
    o         record;
    target    integer;
    n         bigint;
BEGIN
    IF old_p = '' OR new_p = '' OR old_p = new_p THEN
        RAISE EXCEPTION 'rename: give two different project names';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM sonar.services WHERE project = old_p)
       AND NOT EXISTS (SELECT 1 FROM sonar.project_days WHERE project = old_p)
       AND NOT EXISTS (SELECT 1 FROM sonar.project_minutes WHERE project = old_p) THEN
        RAISE EXCEPTION 'rename: Sonar has no history for "%": check the name with: sonar days', old_p;
    END IF;
    SELECT count(*) INTO old_count FROM sonar.services WHERE project = old_p;
    SELECT count(*) INTO new_count FROM sonar.services WHERE project = new_p;

    FOR o IN SELECT id, service FROM sonar.services WHERE project = old_p ORDER BY service LOOP
        target := NULL;
        SELECT id INTO target FROM sonar.services WHERE project = new_p AND service = o.service;
        IF target IS NULL AND old_count = 1 AND new_count = 1 THEN
            SELECT id INTO target FROM sonar.services WHERE project = new_p;
        END IF;
        IF target IS NULL THEN
            UPDATE sonar.services SET project = new_p WHERE id = o.id;
            RAISE NOTICE 'service "%/%" moves to "%/%"', old_p, o.service, new_p, o.service;
            CONTINUE;
        END IF;
        RAISE NOTICE 'service "%/%" merges into "%/%"', old_p, o.service, new_p,
            (SELECT service FROM sonar.services WHERE id = target);

        UPDATE sonar.containers SET service_id = target WHERE service_id = o.id;

        UPDATE sonar.service_minutes m SET service_id = target WHERE m.service_id = o.id
           AND NOT EXISTS (SELECT 1 FROM sonar.service_minutes x WHERE x.service_id = target AND x.minute = m.minute);
        DELETE FROM sonar.service_minutes WHERE service_id = o.id;

        UPDATE sonar.check_minutes m SET service_id = target WHERE m.service_id = o.id
           AND NOT EXISTS (SELECT 1 FROM sonar.check_minutes x WHERE x.service_id = target AND x.minute = m.minute);
        DELETE FROM sonar.check_minutes WHERE service_id = o.id;

        UPDATE sonar.updates u SET service_id = target WHERE u.service_id = o.id
           AND NOT EXISTS (SELECT 1 FROM sonar.updates x WHERE x.service_id = target AND x.started_at = u.started_at);
        DELETE FROM sonar.updates WHERE service_id = o.id;

        UPDATE sonar.service_days t SET
               minutes_up = t.minutes_up + d.minutes_up, minutes_degraded = t.minutes_degraded + d.minutes_degraded,
               minutes_down = t.minutes_down + d.minutes_down, minutes_updating = t.minutes_updating + d.minutes_updating
          FROM sonar.service_days d
         WHERE t.service_id = target AND d.service_id = o.id AND d.day = t.day;
        DELETE FROM sonar.service_days d WHERE d.service_id = o.id
           AND EXISTS (SELECT 1 FROM sonar.service_days x WHERE x.service_id = target AND x.day = d.day);
        UPDATE sonar.service_days SET service_id = target WHERE service_id = o.id;

        UPDATE sonar.services t SET first_seen = least(t.first_seen, s.first_seen), last_seen = greatest(t.last_seen, s.last_seen)
          FROM sonar.services s WHERE t.id = target AND s.id = o.id;
        DELETE FROM sonar.services WHERE id = o.id;
    END LOOP;

    UPDATE sonar.project_minutes m SET project = new_p WHERE m.project = old_p
       AND NOT EXISTS (SELECT 1 FROM sonar.project_minutes x WHERE x.project = new_p AND x.minute = m.minute);
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'minutes moved: %', n;
    DELETE FROM sonar.project_minutes WHERE project = old_p;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'minutes recorded under both names (the new name''s kept): %', n;

    UPDATE sonar.project_days t SET
           minutes_up = t.minutes_up + d.minutes_up, minutes_degraded = t.minutes_degraded + d.minutes_degraded,
           minutes_down = t.minutes_down + d.minutes_down, minutes_updating = t.minutes_updating + d.minutes_updating,
           latency_ms_avg = coalesce(t.latency_ms_avg, d.latency_ms_avg)
      FROM sonar.project_days d
     WHERE t.project = new_p AND d.project = old_p AND d.day = t.day;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'days recorded under both names (added up): %', n;
    DELETE FROM sonar.project_days d WHERE d.project = old_p
       AND EXISTS (SELECT 1 FROM sonar.project_days x WHERE x.project = new_p AND x.day = d.day);
    UPDATE sonar.project_days SET project = new_p WHERE project = old_p;
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'days moved: %', n;
END
$$;

SELECT project, min(day) AS first_day, max(day) AS last_day, count(*) AS days
  FROM sonar.project_days WHERE project IN (:'old', :'new')
 GROUP BY project ORDER BY 1;

SELECT s.project, s.service, min(d.day) AS first_day, max(d.day) AS last_day, count(d.day) AS days
  FROM sonar.services s LEFT JOIN sonar.service_days d ON d.service_id = s.id
 WHERE s.project IN (:'old', :'new')
 GROUP BY s.project, s.service
 ORDER BY 1, 2;

\if :{?apply}
COMMIT;
\echo 'Renamed. The status page shows the full history within about a minute.'
\else
ROLLBACK;
\echo 'Preview only: nothing changed. Add -v apply=1 to rename.'
\endif
