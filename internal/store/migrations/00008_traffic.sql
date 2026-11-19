-- Traffic per site, as Cloudflare counts it (requests from visitors only):
-- each hostname's requests, 5xx answers and bytes sent, by hour. Read again
-- for a few hours, so each row ends up with Cloudflare's final count.

-- +goose Up
CREATE TABLE sonar.traffic_hours (
    host       text NOT NULL,
    hour       timestamptz NOT NULL,
    requests   bigint NOT NULL,
    errors_5xx bigint NOT NULL,
    bytes      bigint NOT NULL,
    PRIMARY KEY (host, hour)
);
CREATE INDEX traffic_hours_hour ON sonar.traffic_hours (hour);

-- sonar_reader reads it through the default privileges of 00002.
GRANT SELECT, INSERT, UPDATE, DELETE ON sonar.traffic_hours TO sonar_app;
