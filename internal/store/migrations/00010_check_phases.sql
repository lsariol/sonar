-- Each check minute keeps where its slowest answer's time went, so a slow site can be told from a slow network.

-- +goose Up
ALTER TABLE sonar.check_minutes
    ADD COLUMN dns_ms     integer,
    ADD COLUMN connect_ms integer,
    ADD COLUMN tls_ms     integer,
    ADD COLUMN wait_ms    integer,
    ADD COLUMN read_ms    integer;
