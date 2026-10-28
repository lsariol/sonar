# Changelog

All notable changes to Sonar. Versions follow [semantic versioning](https://semver.org).

## v0.2.2 (2026-10-28)

### Added
- `scripts/db/rename-project.sql` moves a renamed project's history to its new name, so it keeps its uptime and days. It previews first and changes nothing without `apply=1`.

## v0.2.1 (2026-10-26)

### Fixed
- Sonar's own deploys are recorded as updates, with their versions, so they show as markers in the history like every other project's. A deploy of another project while Sonar was restarting is caught too.

## v0.2.0 (2026-10-26)

### Added
- The status shows each project's running version, such as `v1.3.0`, so a problem can be traced to the version that brought it.
- Deploy markers in the history show the versions before and after, such as `v1.2.0` → `v1.3.0`, including for a deploy that failed.

### Changed
- `sonar-api` declares its port, 2500, so `docker ps` shows it. Nothing new can connect.

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
