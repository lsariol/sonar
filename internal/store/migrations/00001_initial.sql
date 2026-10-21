-- Sonar's history. States are small integers: 0 unknown,
-- 1 up, 2 degraded, 3 down. Every time is UTC; days are UTC days.

-- +goose Up

-- Compose services ever seen: the stable unit history is kept for.
CREATE TABLE sonar.services (
    id         integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project    text NOT NULL,
    service    text NOT NULL,
    first_seen timestamptz NOT NULL,
    last_seen  timestamptz NOT NULL,
    UNIQUE (project, service)
);

-- Container instances: what ran when, and which version.
CREATE TABLE sonar.containers (
    id          text PRIMARY KEY,
    service_id  integer NOT NULL REFERENCES sonar.services (id),
    name        text NOT NULL,
    image       text NOT NULL,
    version     text NOT NULL DEFAULT '',
    created_at  timestamptz,
    started_at  timestamptz,
    finished_at timestamptz,
    exit_code   integer NOT NULL DEFAULT 0,
    first_seen  timestamptz NOT NULL,
    last_seen   timestamptz NOT NULL
);
CREATE INDEX containers_last_seen ON sonar.containers (last_seen);

-- The host, one row per minute. CPU needs two readings, so it has its own
-- sample count and can be missing; the disk is its last reading.
CREATE TABLE sonar.host_minutes (
    minute              timestamptz PRIMARY KEY,
    samples             smallint NOT NULL,
    expected            smallint NOT NULL,
    cpu_samples         smallint NOT NULL,
    cpu_pct_avg         real,
    cpu_pct_min         real,
    cpu_pct_max         real,
    mem_used_pct_avg    real NOT NULL,
    mem_used_pct_min    real NOT NULL,
    mem_used_pct_max    real NOT NULL,
    mem_used_bytes_avg  bigint NOT NULL,
    mem_total_bytes     bigint NOT NULL,
    swap_used_bytes_avg bigint NOT NULL,
    load1_avg           real NOT NULL,
    load1_max           real NOT NULL,
    disk_used_bytes     bigint,
    disk_total_bytes    bigint
);

-- Each service, one row per minute: its worst state, its containers' usage
-- added up, and how much their counters grew during the minute.
CREATE TABLE sonar.service_minutes (
    service_id      integer NOT NULL REFERENCES sonar.services (id),
    minute          timestamptz NOT NULL,
    samples         smallint NOT NULL,
    state           smallint NOT NULL,
    counted         boolean NOT NULL,
    containers      smallint NOT NULL,
    cpu_cores_avg   real,
    cpu_cores_max   real,
    mem_bytes_avg   bigint,
    mem_bytes_max   bigint,
    net_rx_bytes    bigint NOT NULL DEFAULT 0,
    net_tx_bytes    bigint NOT NULL DEFAULT 0,
    blk_read_bytes  bigint NOT NULL DEFAULT 0,
    blk_write_bytes bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (service_id, minute)
);
CREATE INDEX service_minutes_minute ON sonar.service_minutes (minute);

-- Outside checks, one row per checked service per minute. Latency covers the
-- checks that answered (ok or slow).
CREATE TABLE sonar.check_minutes (
    service_id     integer NOT NULL REFERENCES sonar.services (id),
    minute         timestamptz NOT NULL,
    url            text NOT NULL,
    attempts       smallint NOT NULL,
    failures       smallint NOT NULL,
    slow           smallint NOT NULL,
    latency_ms_avg integer,
    latency_ms_max integer,
    state          smallint NOT NULL,
    PRIMARY KEY (service_id, minute)
);
CREATE INDEX check_minutes_minute ON sonar.check_minutes (minute);

-- Each project, one row per minute it had a known state.
CREATE TABLE sonar.project_minutes (
    project  text NOT NULL,
    minute   timestamptz NOT NULL,
    state    smallint NOT NULL,
    evidence text NOT NULL,
    PRIMARY KEY (project, minute)
);
CREATE INDEX project_minutes_minute ON sonar.project_minutes (minute);

-- Daily availability, kept long after the minutes are gone. A day's state
-- and coverage are worked out from these counts.
CREATE TABLE sonar.project_days (
    project          text NOT NULL,
    day              date NOT NULL,
    minutes_up       integer NOT NULL,
    minutes_degraded integer NOT NULL,
    minutes_down     integer NOT NULL,
    latency_ms_avg   integer,
    PRIMARY KEY (project, day)
);

CREATE TABLE sonar.service_days (
    service_id       integer NOT NULL REFERENCES sonar.services (id),
    day              date NOT NULL,
    minutes_up       integer NOT NULL,
    minutes_degraded integer NOT NULL,
    minutes_down     integer NOT NULL,
    PRIMARY KEY (service_id, day)
);
