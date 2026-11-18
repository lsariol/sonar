-- Each check minute keeps the last error of the minute's failed checks, so
-- an incident can say why. NULL when every check that minute answered.

-- +goose Up
ALTER TABLE sonar.check_minutes ADD COLUMN last_error text;
