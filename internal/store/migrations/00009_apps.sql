-- The apps' own reports (internal/report): how much each usage counter grew,
-- per minute and per day, and when each feature check changed.

-- +goose Up
CREATE TABLE sonar.app_counter_minutes (
    project  text NOT NULL,
    name     text NOT NULL,
    minute   timestamptz NOT NULL,
    increase bigint NOT NULL,
    PRIMARY KEY (project, name, minute)
);
CREATE INDEX app_counter_minutes_minute ON sonar.app_counter_minutes (minute);

CREATE TABLE sonar.app_counter_days (
    project text NOT NULL,
    name    text NOT NULL,
    day     date NOT NULL,
    total   bigint NOT NULL,
    PRIMARY KEY (project, name, day)
);

CREATE TABLE sonar.app_check_changes (
    project text NOT NULL,
    service text NOT NULL,
    name    text NOT NULL,
    at      timestamptz NOT NULL,
    ok      boolean NOT NULL,
    message text NOT NULL,
    PRIMARY KEY (project, service, name, at)
);
CREATE INDEX app_check_changes_at ON sonar.app_check_changes (at);

-- sonar_reader reads them through the default privileges of 00002.
GRANT SELECT, INSERT, UPDATE, DELETE ON sonar.app_counter_minutes, sonar.app_counter_days, sonar.app_check_changes
    TO sonar_app;
