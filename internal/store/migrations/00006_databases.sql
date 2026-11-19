-- Each database's connections and size on sparkdb, once a minute.

-- +goose Up
CREATE TABLE sonar.database_minutes (
    minute      timestamptz NOT NULL,
    database    text NOT NULL,
    connections smallint NOT NULL,
    active      smallint NOT NULL,
    size_bytes  bigint NOT NULL,
    PRIMARY KEY (database, minute)
);
CREATE INDEX database_minutes_minute ON sonar.database_minutes (minute);

-- sonar_reader reads it through the default privileges of 00002.
GRANT SELECT, INSERT, UPDATE, DELETE ON sonar.database_minutes TO sonar_app;
