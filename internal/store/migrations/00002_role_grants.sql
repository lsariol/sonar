-- Grants for the login roles. Requires sonar_app and sonar_reader to exist
-- (scripts/db/setup.sql, since roles are server-wide).
--
-- sonar_app:    the agent: reads and writes the history, deletes what's past
--               its retention; reads the migrations table.
-- sonar_reader: the API and pgAdmin: reads everything, including tables
--               added later.
--
-- Privileges are reset first, so this ends in the same state whatever was
-- granted by hand before.

-- +goose Up
REVOKE ALL ON SCHEMA sonar FROM PUBLIC;
GRANT USAGE ON SCHEMA sonar TO sonar_app, sonar_reader;

REVOKE ALL ON ALL TABLES IN SCHEMA sonar FROM sonar_app, sonar_reader;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA sonar FROM sonar_app, sonar_reader;

GRANT SELECT, INSERT, UPDATE, DELETE ON
    sonar.services, sonar.containers, sonar.host_minutes, sonar.service_minutes,
    sonar.check_minutes, sonar.project_minutes, sonar.project_days, sonar.service_days
    TO sonar_app;
GRANT SELECT ON sonar.goose_db_version TO sonar_app;

GRANT SELECT ON ALL TABLES IN SCHEMA sonar TO sonar_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE sonar_owner IN SCHEMA sonar
    GRANT SELECT ON TABLES TO sonar_reader;
