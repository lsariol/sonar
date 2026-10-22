# Changelog

All notable changes to Sonar. Versions follow [semantic versioning](https://semver.org).

## v0.1.0 (2026-10-22)

The first release: Sonar watches the server and every project on it, and keeps the record for the status page.

### Added
- Live status of every project: up, degraded, down, updating or unknown, from Docker and from a check of each public address every 30 seconds.
- Server figures: CPU, memory, swap, load and disk.
- Each service's CPU, memory, network and disk use.
- History by the minute (kept 30 days) and by the day (kept 380 days), with uptime percentages.
- Deploys recorded as updates: they count as uptime and show as markers in the history.
- A private API for the mobasity.com status page, protected by a token.
- A command-line tool for checking status, projects, the server, checks, days and updates.
- Read-only access to Docker through a small proxy that passes on only what Sonar needs.
