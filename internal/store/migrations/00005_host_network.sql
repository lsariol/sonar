-- Bytes each host network interface moved per minute.

-- +goose Up
CREATE TABLE sonar.host_net_minutes (
    minute    timestamptz NOT NULL,
    interface text NOT NULL,
    rx_bytes  bigint NOT NULL,
    tx_bytes  bigint NOT NULL,
    PRIMARY KEY (minute, interface)
);
CREATE INDEX host_net_minutes_minute ON sonar.host_net_minutes (minute);

-- sonar_reader reads it through the default privileges of 00002.
GRANT SELECT, INSERT, UPDATE, DELETE ON sonar.host_net_minutes TO sonar_app;
