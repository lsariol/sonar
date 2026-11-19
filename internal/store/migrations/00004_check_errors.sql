-- Each check minute keeps its failed checks' last error, so an incident can say why.

-- +goose Up
ALTER TABLE sonar.check_minutes ADD COLUMN last_error text;
