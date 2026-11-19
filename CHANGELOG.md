# Changelog

All notable changes to Sonar. Versions follow [semantic versioning](https://semver.org).

## v0.3.2 (unreleased)

### Added
- Each outside check records where its time went: looking up the address, connecting, the TLS handshake, waiting for the site and reading the answer. A degraded stretch in `/v1/monitor/states` gives its slowest answer split that way, so a slow site can be told from a slow network, and `sonar states` says which phase took longest.

### Fixed
- A single answer slower than 2 seconds no longer marks a project degraded: like failures, slow answers need two in a row. One slow answer made every site flap for a minute or two.
- Every degraded stretch names its cause. A check that turned slow at the end of one minute kept the project degraded into the next, but that minute recorded no cause.

## v0.3.1 (2026-11-19)

### Added
- Each point in the host history gives `minute_min` and `minute_max` for CPU and memory: the quietest and busiest one-minute average in the bucket. A chart can shade the range a minute actually reached, so a few seconds of a build no longer show as 100% on every range. `min` and `max` are unchanged. The owner's host chart has them too.

## v0.3.0 (2026-11-19)

Owner detail for a private dashboard, and new things Sonar watches: Spark's own health, the network, the databases, disk use, traffic from Cloudflare and what the apps report about themselves.

### Added
- A second API consumer, Vista, with its own token, and owner endpoints under `monitor:read`: every project's status (the ones the status page leaves out too), each project now with its containers, restarts and checks, Spark's conditions, state changes with what wasn't up, host and per-service history, databases, disk use, traffic and the apps' reports. The README lists them.
- Spark's own conditions, such as a full disk, heavy swap, sparkdb near its connection limit or an infrastructure project down, each with a level and since when. `sonar status` lists them.
- The host's network traffic per interface, read from the host's network counters alone.
- Each database's size and connections on sparkdb, read by a new role, `sonar_monitor`, that sees statistics only.
- What each project keeps on disk (its data folder, its own image layers, its containers and volumes), measured every 30 minutes and kept for 380 days.
- Each site's requests, 5xx answers and bytes from Cloudflare, by hour, kept for 380 days.
- App reports: an app labelled `com.mobasity.report-port` serves its usage counters and feature checks, and Sonar keeps how each counter grows and when each check changes.
- The `com.mobasity.check-path` label checks a site at another path, for a site behind a sign-in; `com.mobasity.group: infrastructure` marks a project Spark depends on.
- Each outside check's last error is kept with its minute, so an incident can say why.
- CLI commands `databases`, `disk`, `traffic`, `apps` and `states`.

### Changed
- `sonar-agent` needs `SONAR_DATABASE_MONITOR_URL` and `SONAR_CLOUDFLARE_TOKEN`, and `sonar-api` needs `SHARED_SONAR_VISTA_TOKEN`; each refuses to start without its keys.
- The image runs as user 10001; only `sonar-proxy`, which reads the Docker socket and sizes the data folders, runs as root.
- Built with Go 1.27.4, which fixes a crash in Go's HTTP server.
- An MIT license, and a README that covers setup, settings, labels, the API and app reports.

## v0.2.5 (2026-11-11)

### Added
- Each day in the status gives its minute counts: `observed_minutes`, `degraded_minutes` and `down_minutes`, so a page can show exactly how long a project was degraded or down. They are 0 on a day with nothing observed, never `null`.

## v0.2.4 (2026-11-03)

### Changed
- lucassariol-web is left off the public status, like the database and the Cloudflare tunnel. Sonar still monitors it.
- Logs follow Spark's logging standard: one JSON line per record, with errors under `err`, so Blackbox can search and filter them. A part of Sonar that can't start says why in its log before it exits.

## v0.2.3 (2026-11-02)

### Fixed
- A project's first deploy is recorded as an update, so it gets a marker in the history and counts in the day's deploys. It has a version after it but none before.

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
