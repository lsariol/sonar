# Sonar

Spark's read-only monitoring service. It measures the host and every container, checks public addresses from outside, keeps the history in sparkdb, and serves an authenticated API. Its first consumer is the status page on [mobasity.com](https://mobasity.com/status).

Sonar observes and reports. It never starts, stops or changes anything; deploys are [Lighthouse](https://github.com/lsariol/lighthouse)'s job.

- **Changes:** [CHANGELOG.md](CHANGELOG.md).

## Parts

One Go binary, run as three containers:

| Container | Does |
|---|---|
| `sonar-proxy` | The only part with the Docker socket. No network; answers two fixed read requests from the agent |
| `sonar-agent` | Samples the host and containers, checks public URLs, writes history to sparkdb |
| `sonar-api` | Serves the data to authenticated consumers on `sonar-api:2500` (the `spark` network) |

## Running it locally

Needs Go 1.27.3 and Docker Desktop.

One-time setup, in the repo folder:

1. `docker network create spark`, if it doesn't exist yet.
2. On `sparkdb-dev` (`localhost:5000`), run `scripts/db/setup.sql` as the admin user, with dev passwords.
3. Copy `.env.example` to `.env` (gitignored) and fill in the dev values: the three database URLs pointing at `host.docker.internal:5000`, and a dev API token.

`docker-compose.dev.yml` publishes the API on `127.0.0.1:2501` and keeps what Sonar writes in `.dev/`. All three containers turn `healthy` in `docker ps`, and `curl -H "Authorization: Bearer <dev token>" http://127.0.0.1:2501/v1/status` returns your local projects.

```bash
go test ./...                                              # the tests
docker build --target test .                               # as Lighthouse runs them, with -race
scripts/test-db.sh up && eval "$(scripts/test-db.sh env)"  # a throwaway Postgres: the database tests run too
scripts/test-db.sh down                                    # remove it
docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build
```

The CLI: `docker exec -it sonar-api /sonar shell`, then `help`.
