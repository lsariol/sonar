-- Updates: deploys, as Sonar sees them, a service's containers replaced by
-- new ones. Updating minutes are state 4 in the minute tables.

-- +goose Up
ALTER TABLE sonar.project_days ADD COLUMN minutes_updating integer NOT NULL DEFAULT 0;
ALTER TABLE sonar.service_days ADD COLUMN minutes_updating integer NOT NULL DEFAULT 0;

-- One row per update of a service. outcome is ok (healthy within 2 minutes),
-- failed (a new container crashed or turned unhealthy) or timed out.
CREATE TABLE sonar.updates (
    service_id   integer NOT NULL REFERENCES sonar.services (id),
    started_at   timestamptz NOT NULL,
    ended_at     timestamptz NOT NULL,
    from_version text NOT NULL DEFAULT '',
    to_version   text NOT NULL DEFAULT '',
    outcome      text NOT NULL,
    PRIMARY KEY (service_id, started_at)
);
CREATE INDEX updates_started_at ON sonar.updates (started_at);

-- sonar_reader reads it through the default privileges of 00002.
GRANT SELECT, INSERT, UPDATE, DELETE ON sonar.updates TO sonar_app;
