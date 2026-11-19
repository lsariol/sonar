# Sonar

Spark's read-only monitoring service: it measures the server and every container on it, checks the public sites from outside, keeps the history in Postgres, and serves it to authenticated consumers such as the status page on [mobasity.com](https://mobasity.com/status).

Sonar observes and reports. It never starts, stops or changes anything; deploys are [Lighthouse](https://github.com/lsariol/lighthouse)'s job.

## Features

- Each project's state (operational, degraded, down, updating), worked out from its containers, their healthchecks and an outside check of its public address, with daily uptime kept for 380 days.
- Deploys recognised from the container list alone, with the versions before and after and whether the new version came up.
- The host's CPU, memory, swap, load, disk and network traffic, and each service's CPU, memory, network and disk I/O, minute by minute for 30 days.
- Spark's own conditions, such as a full disk, heavy swap or an infrastructure project down, each with since when.
- Each database's connections and size, each project's data folder, image and volume sizes, and each site's traffic from Cloudflare.
- The apps' own usage counters and feature checks, read from a small report each app serves.
- A public projection for the status page, and owner detail for a private dashboard, each behind its own token.
- A CLI in the API container: `docker exec -it sonar-api /sonar shell`, then `help`.

## Parts

One Go binary, run as three containers:

| Container | Does |
|---|---|
| `sonar-proxy` | The only part with the Docker socket and the projects' data folders. No network; answers four fixed read requests from the agent |
| `sonar-agent` | Samples the host, containers, databases, Cloudflare and the apps' reports, checks public addresses, writes the history to Postgres |
| `sonar-api` | Serves the data to authenticated consumers on `sonar-api:2500` (the `spark` network) |

## Requirements

- Go 1.27.4
- Docker Desktop, for the containers and the test stage
- Postgres 16, set up with [scripts/db/setup.sql](scripts/db/setup.sql)

## Getting started

1. Create the Docker network the containers join, if it doesn't exist: `docker network create spark`.
2. Create the database and its roles: run `scripts/db/setup.sql` with `psql` as a superuser, giving a password for each login role. It's safe to run again, and never changes the password of a role that exists.

   ```bash
   psql -U postgres -d postgres -v migrator_password=... -v app_password=... \
     -v reader_password=... -v monitor_password=... -f scripts/db/setup.sql
   ```

3. Copy `.env.example` to `.env` and fill it in: the four connection strings, as the containers reach Postgres, a Cloudflare API token, and a token of at least 32 random characters for each API consumer (`openssl rand -hex 32`).
4. Start it:

   ```bash
   docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build
   ```

`docker-compose.dev.yml` publishes the API on `127.0.0.1:2501` and keeps what Sonar writes in `.dev/`. All three containers turn `healthy` in `docker ps`, and this returns your local projects:

```bash
curl -H "Authorization: Bearer <dev token>" http://127.0.0.1:2501/v1/status
```

## Configuration

Secrets are read from the environment; every other setting is in the code.

| Variable | Read by | Meaning |
|---|---|---|
| `APP_ENV` | all | `prod` (the default) or `dev` |
| `SONAR_VERSION` | all | The version Sonar reports; `dev` when unset |
| `SONAR_DATABASE_URL` | agent | Postgres as `sonar_app`, which writes the history |
| `SONAR_DATABASE_MIGRATOR_URL` | agent | Postgres as `sonar_migrator`, which runs the migrations at startup |
| `SONAR_DATABASE_MONITOR_URL` | agent | Postgres as `sonar_monitor` (`pg_read_all_stats`), which reads every database's connections and size |
| `SONAR_CLOUDFLARE_TOKEN` | agent | A Cloudflare API token with Zone Read and Analytics Read, for traffic per site |
| `SONAR_DATABASE_READER_URL` | api | Postgres as `sonar_reader`, which reads the history |
| `SHARED_SONAR_<CONSUMER>_TOKEN` | api | One bearer token per consumer, 32 characters or more (`internal/api/consumers.go`) |

Labels Sonar reads on other projects' containers:

| Label | Meaning |
|---|---|
| `com.mobasity.public-url` | The service's public address, which Sonar checks from outside every 30 seconds |
| `com.mobasity.check-path` | A path to check instead of the public address's own, such as `/healthz` for a site behind a sign-in |
| `com.mobasity.version` | The version the container runs |
| `com.mobasity.group` | `infrastructure` for a project Spark itself depends on (one that's down is one of Spark's conditions), or `app` |
| `com.mobasity.report-port` | The port where the app serves its report (below) |

Host access, in `docker-compose.yml`:

| Container | Mount or capability | Why |
|---|---|---|
| `sonar-proxy` | `/var/run/docker.sock` | The container list, stats and Docker's disk use |
| `sonar-proxy` | `/srv/server/storage` (read-only) and `CAP_DAC_READ_SEARCH` | Sizing each project's data folder; the capability reads and lists files, it never writes |
| `sonar-agent` | `/proc/1/net/dev` (read-only, that one file) | The host's network counters; a container's own `/proc/net` shows only its network |

## API

Every `/v1/` request needs `Authorization: Bearer <token>`. Each consumer's scopes are set in `internal/api/consumers.go`. Times are UTC, percentages are 0 to 100, and anything unknown is `null`.

| Endpoint | Scope | Answers |
|---|---|---|
| `GET /healthz` | none | Whether the API can reach its database, and its version |
| `GET /v1/status?days=90` | `status:read` | Each public project's state, version, uptime and daily history |
| `GET /v1/status/history?range=24h` | `status:read` | The host's CPU and memory (average, extremes, and busiest and quietest minute), and the deploys, for a chart |
| `GET /v1/monitor/status?days=90` | `monitor:read` | `/v1/status` with every project |
| `GET /v1/monitor/projects` | `monitor:read` | Every project now: its group, since when it's in its state, its services, containers and checks |
| `GET /v1/monitor/conditions` | `monitor:read` | Spark's own conditions and each collector's state; answers without the database |
| `GET /v1/monitor/states?range=7d` | `monitor:read` | Each project's state changes, with the services and checks that weren't up, and where a slow check's time went |
| `GET /v1/monitor/host?range=24h` | `monitor:read` | The host now and over time: CPU, memory, swap, load, disk, network |
| `GET /v1/monitor/usage?range=24h&project=` | `monitor:read` | Each service's CPU, memory, network and disk I/O over time |
| `GET /v1/monitor/databases?range=24h` | `monitor:read` | Each database's connections and size |
| `GET /v1/monitor/disk?range=30d` | `monitor:read` | Each project's data folder, images, containers and volumes on disk |
| `GET /v1/monitor/traffic?range=7d` | `monitor:read` | Each site's requests, 5xx answers and bytes, from Cloudflare |
| `GET /v1/monitor/apps?days=30` | `monitor:read` | The apps' usage counters by day and their feature checks |

Chart ranges are `1h`, `24h`, `7d` and `30d`; disk takes `7d`, `30d`, `90d` and `380d`, and traffic `24h` to `380d`.

## App reports

An app that sets `com.mobasity.report-port=<port>` serves its report at `GET /report/v1` on that port, on the `spark` network, without a token. Sonar reads it once a minute.

```json
{
  "started_at": "2026-11-18T10:00:00Z",
  "counters": {"games_completed": 1234},
  "checks": [{"name": "twitch_chat", "ok": true, "message": "connected to 3 channels"}]
}
```

- **Counters** are running totals since `started_at`, normally when the process started. They only grow, and start over from zero with a new `started_at`. Sonar keeps how much each one grew per minute and per day.
- **Checks** say whether one of the app's own features works. Sonar keeps when each one changed.
- Names are lowercase `snake_case`, at most 50 counters and 20 checks, messages at most 200 bytes, the answer at most 64 KB. A report holds no secrets and nobody's data.

## Development

```bash
go test -race ./...                                        # the tests
docker build --target test .                               # gofmt, vet and the tests, as before a deploy
scripts/test-db.sh up && eval "$(scripts/test-db.sh env)"  # a throwaway Postgres: the database tests run too
scripts/test-db.sh down                                    # remove it
```

## Deployment

`docker-compose.yml` is written for [Lighthouse](https://github.com/lsariol/lighthouse), which deploys each version tag. It expects an external Docker network named `spark` and reads its secrets from the environment. Changes are listed in [CHANGELOG.md](CHANGELOG.md).

## License

See [LICENSE](LICENSE).
