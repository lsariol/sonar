-- What each project keeps on disk, measured by sonar-proxy every 30 minutes:
-- its data folder in /srv/server/storage, the image layers only it uses, its
-- containers' writable layers and its volumes. Kept 380 days, to show growth.

-- +goose Up
CREATE TABLE sonar.project_disk (
    project         text NOT NULL,
    measured_at     timestamptz NOT NULL,
    data_bytes      bigint,            -- NULL without a data folder
    data_unreadable integer NOT NULL,  -- entries that couldn't be read
    image_bytes     bigint NOT NULL,
    container_bytes bigint NOT NULL,
    volume_bytes    bigint NOT NULL,
    PRIMARY KEY (project, measured_at)
);
CREATE INDEX project_disk_measured_at ON sonar.project_disk (measured_at);

-- Everything at once, shared image layers counted once.
CREATE TABLE sonar.disk_totals (
    measured_at       timestamptz PRIMARY KEY,
    data_bytes        bigint,
    image_bytes       bigint NOT NULL,
    container_bytes   bigint NOT NULL,
    volume_bytes      bigint NOT NULL,
    build_cache_bytes bigint NOT NULL
);

-- sonar_reader reads them through the default privileges of 00002.
GRANT SELECT, INSERT, UPDATE, DELETE ON sonar.project_disk, sonar.disk_totals TO sonar_app;
