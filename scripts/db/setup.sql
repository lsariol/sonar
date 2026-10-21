-- Sonar database: the one-time Admin setup on sparkdb.
--
-- Creates what's missing and fixes ownership and access:
--   roles     sonar_owner (NOLOGIN), sonar_migrator (member of the owner,
--             acting as it), sonar_app, sonar_reader
--   database  sonar_db, owned by sonar_owner; only the three login roles may
--             connect
--   schema    sonar, if it exists already: owned by sonar_owner
-- Everything inside the schema (tables, grants) comes from Sonar's goose
-- migrations, never from here.
--
-- Safe to run again. It never changes the password of a role that exists; a
-- password is only needed for a role that doesn't exist yet, and every missing
-- one is checked before anything changes.
--
-- Run as the Admin superuser (on Spark, over SSH):
--   docker exec -i sparkdb psql -U Admin -d postgres \
--     -v migrator_password="..." -v app_password="..." -v reader_password="..." \
--     -f - < scripts/db/setup.sql

\set ON_ERROR_STOP on
SET client_min_messages = warning;

SELECT NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sonar_owner')    AS need_owner,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sonar_migrator') AS need_migrator,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sonar_app')      AS need_app,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sonar_reader')   AS need_reader,
       NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'sonar_db')    AS need_db,
       current_setting('is_superuser') = 'on' AS is_superuser
\gset

-- 1. Preconditions, before anything changes.

\if :is_superuser
\else
DO $$ BEGIN RAISE EXCEPTION 'Run this as the Admin superuser (psql -U Admin).'; END $$;
\endif

\if :need_migrator
\if :{?migrator_password}
\else
DO $$ BEGIN RAISE EXCEPTION 'sonar_migrator doesn''t exist: pass its password with -v migrator_password=... Nothing was changed.'; END $$;
\endif
\endif

\if :need_app
\if :{?app_password}
\else
DO $$ BEGIN RAISE EXCEPTION 'sonar_app doesn''t exist: pass its password with -v app_password=... Nothing was changed.'; END $$;
\endif
\endif

\if :need_reader
\if :{?reader_password}
\else
DO $$ BEGIN RAISE EXCEPTION 'sonar_reader doesn''t exist: pass its password with -v reader_password=... Nothing was changed.'; END $$;
\endif
\endif

-- 2. Roles.

\if :need_owner
\echo 'Creating sonar_owner'
CREATE ROLE sonar_owner NOLOGIN;
\else
-- Nothing logs in as the owner; its old password (if any) goes too.
ALTER ROLE sonar_owner NOLOGIN PASSWORD NULL;
\endif

\if :need_migrator
\echo 'Creating sonar_migrator'
CREATE ROLE sonar_migrator LOGIN PASSWORD :'migrator_password';
\else
ALTER ROLE sonar_migrator LOGIN;
\endif

\if :need_app
\echo 'Creating sonar_app'
CREATE ROLE sonar_app LOGIN PASSWORD :'app_password';
\else
ALTER ROLE sonar_app LOGIN;
\endif

\if :need_reader
\echo 'Creating sonar_reader'
CREATE ROLE sonar_reader LOGIN PASSWORD :'reader_password';
\else
ALTER ROLE sonar_reader LOGIN;
\endif

-- The migrator acts as the owner, so everything the migrations create is
-- owned by sonar_owner, not by whoever ran them.
GRANT sonar_owner TO sonar_migrator;
ALTER ROLE sonar_migrator SET role = 'sonar_owner';

-- 3. The database.

\if :need_db
\echo 'Creating sonar_db'
CREATE DATABASE sonar_db OWNER sonar_owner;
\else
ALTER DATABASE sonar_db OWNER TO sonar_owner;
\endif

REVOKE ALL ON DATABASE sonar_db FROM PUBLIC;
GRANT CONNECT ON DATABASE sonar_db TO sonar_migrator, sonar_app, sonar_reader;

-- 4. Inside the database: the sonar schema (if an earlier setup made it)
-- belongs to the owner, and nobody else may create objects in public.

\connect sonar_db
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'sonar') THEN
    ALTER SCHEMA sonar OWNER TO sonar_owner;
  END IF;
END $$;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

\echo
\echo 'Done. Check: every row below matches its "want".'
\echo
\connect postgres
SELECT r.rolname AS role, r.rolcanlogin AS can_login,
       coalesce((SELECT string_agg(b.rolname, ', ') FROM pg_auth_members m JOIN pg_roles b ON b.oid = m.roleid
                  WHERE m.member = r.oid), '') AS member_of,
       coalesce(array_to_string(r.rolconfig, '; '), '') AS settings,
       CASE r.rolname
         WHEN 'sonar_owner'    THEN 'NOLOGIN'
         WHEN 'sonar_migrator' THEN 'LOGIN, member of sonar_owner, role=sonar_owner'
         ELSE 'LOGIN'
       END AS want
  FROM pg_roles r
 WHERE r.rolname IN ('sonar_owner', 'sonar_migrator', 'sonar_app', 'sonar_reader')
 ORDER BY r.rolname;
SELECT datname AS database, pg_get_userbyid(datdba) AS owner, array_to_string(datacl, ' ') AS access,
       'owner sonar_owner; c (connect) for migrator, app, reader' AS want
  FROM pg_database WHERE datname = 'sonar_db';
