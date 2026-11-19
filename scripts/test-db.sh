#!/bin/sh
# A throwaway Postgres on 127.0.0.1:55433 for the database tests, with prod's roles: up, env or down.
set -eu

NAME=sonar-testdb
PORT=55433
IMAGE=postgres:16.4

case "${1:-}" in
up)
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	docker run -d --name "$NAME" -p "127.0.0.1:$PORT:5432" \
		-e POSTGRES_USER=Admin -e POSTGRES_PASSWORD=admin "$IMAGE" >/dev/null
	i=0
	until docker exec "$NAME" pg_isready -U Admin -q 2>/dev/null; do
		i=$((i + 1))
		[ "$i" -lt 60 ] || { echo "Postgres didn't start; see: docker logs $NAME" >&2; exit 1; }
		sleep 1
	done
	# The image's init scripts restart the server once; wait for that too.
	sleep 2
	until docker exec "$NAME" pg_isready -U Admin -q 2>/dev/null; do sleep 1; done
	docker exec -i "$NAME" psql -U Admin -d postgres -q \
		-v migrator_password=migrator -v app_password=app -v reader_password=reader -v monitor_password=monitor \
		-f - <scripts/db/setup.sql >/dev/null
	echo "Test database ready on 127.0.0.1:$PORT. Next: eval \"\$(scripts/test-db.sh env)\"" >&2
	;;
env)
	echo "export SONAR_TEST_DATABASE_MIGRATOR_URL='postgres://sonar_migrator:migrator@127.0.0.1:$PORT/sonar_db?sslmode=disable'"
	echo "export SONAR_TEST_DATABASE_URL='postgres://sonar_app:app@127.0.0.1:$PORT/sonar_db?sslmode=disable'"
	echo "export SONAR_TEST_DATABASE_READER_URL='postgres://sonar_reader:reader@127.0.0.1:$PORT/sonar_db?sslmode=disable'"
	echo "export SONAR_TEST_DATABASE_MONITOR_URL='postgres://sonar_monitor:monitor@127.0.0.1:$PORT/sonar_db?sslmode=disable'"
	;;
down)
	docker rm -f "$NAME" >/dev/null
	echo "Test database removed." >&2
	;;
*)
	echo "Usage: scripts/test-db.sh up|env|down" >&2
	exit 2
	;;
esac
