#!/usr/bin/env python3
"""Synthetic-only real CLI/INSERT tests in a disposable local PostgreSQL cluster.

No external DSN, existing server, business data, or credentials are consumed.
Raw subprocess output remains local and is deliberately not printed on failure.
"""
import json
import os
from pathlib import Path
import socket
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
BASE = Path(os.environ.get("OPENCODE_TEST_TMP", tempfile.gettempdir()))


def run(args, *, env=None, input=None, success=True):
    result = subprocess.run(args, text=True, input=input, capture_output=True, env=env)
    if (result.returncode == 0) != success:
        raise AssertionError(f"unexpected result for {Path(args[0]).name}; output suppressed")
    return result.stdout


with tempfile.TemporaryDirectory(prefix="accounts-rebuild-fixture-", dir=BASE) as tmp:
    tmp = Path(tmp)
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    data = tmp / "data"
    init_args = ["initdb", "-D", str(data), "-A", "trust", "-U", "fixture", "--no-locale", "--encoding=UTF8"]
    share = Path(shutil.which("initdb")).resolve().parents[1] / "share/postgresql"
    if share.exists(): init_args += ["-L", str(share)]
    run(init_args)
    started = False
    try:
        run(["pg_ctl", "-D", str(data), "-l", str(tmp / "postgres.log"), "-o",
             f"-h 127.0.0.1 -p {port} -k ''", "-w", "start"])
        started = True
        binary = tmp / "migratectl"
        run(["go", "build", "-o", str(binary), "./cmd/migratectl"], env=os.environ.copy())

        def sql(db, query, success=True):
            return run(["psql", "-X", "-h", "127.0.0.1", "-p", str(port), "-U", "fixture",
                        "-d", db, "-v", "ON_ERROR_STOP=1", "-At", "-c", query], success=success).strip()

        for name in ("legacy_fixture", "accounts_rebuild_fixture", "accounts_rebuild_rollback",
                     "accounts_rebuild_empty", "accounts_rebuild_null", "accounts_rebuild_same"):
            sql("postgres", f"CREATE DATABASE {name}")
        schema = (ROOT / "sql/schema.sql").read_text()
        for name in ("accounts_rebuild_fixture", "accounts_rebuild_rollback", "accounts_rebuild_empty", "accounts_rebuild_null", "accounts_rebuild_same"):
            sql(name, schema)
        # Old three-table-only source: no MFA/replication/lifecycle columns,
        # no identity/session timestamps, no new business tables.
        sql("legacy_fixture", """
CREATE TABLE users(uuid uuid PRIMARY KEY,username text NOT NULL,password text NOT NULL,email text,
 role text NOT NULL,level integer NOT NULL,groups jsonb NOT NULL,permissions jsonb NOT NULL,
 active boolean NOT NULL,proxy_uuid uuid NOT NULL,created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL);
CREATE TABLE identities(uuid uuid PRIMARY KEY,provider text NOT NULL,external_id text NOT NULL,user_uuid uuid REFERENCES users(uuid));
CREATE TABLE sessions(uuid uuid PRIMARY KEY,token text NOT NULL,expires_at timestamptz NOT NULL,user_uuid uuid REFERENCES users(uuid));
INSERT INTO users VALUES('11111111-1111-4111-8111-111111111111','fixture','synthetic-not-a-real-password-hash','fixture@example.invalid',
 'user',20,'["fixture"]','["read"]',false,'11111111-1111-4111-8111-111111111111','2020-01-01T01:02:03.123456+08','2020-01-02T00:00:00Z');
INSERT INTO identities VALUES('22222222-2222-4222-8222-222222222222','fixture','external-fixture','11111111-1111-4111-8111-111111111111');
INSERT INTO sessions VALUES('33333333-3333-4333-8333-333333333333','synthetic-expired-token','2000-01-01Z','11111111-1111-4111-8111-111111111111'),
 ('44444444-4444-4444-8444-444444444444','synthetic-active-token','2099-01-01Z','11111111-1111-4111-8111-111111111111');
""")
        snapshot = tmp / "snapshot.yaml"

        def cli(command, db, *flags, success=True, file=snapshot):
            env = os.environ.copy()
            env["FIXTURE_DSN"] = f"postgres://fixture@127.0.0.1:{port}/{db}?sslmode=disable"
            args = [str(binary), command, "--accounts-only", "--dsn-env", "FIXTURE_DSN"]
            if command == "export": args += ["--output", str(file)]
            else: args += ["--file", str(file), "--target-database", db]
            return run(args + list(flags), env=env, success=success)

        def counts(db):
            return sql(db, "SELECT (SELECT count(*) FROM users)||','||(SELECT count(*) FROM identities)||','||(SELECT count(*) FROM sessions)")

        cli("export", "legacy_fixture")
        assert snapshot.stat().st_mode & 0o777 == 0o600
        cli("import", "accounts_rebuild_fixture", "--dry-run")
        assert counts("accounts_rebuild_fixture") == "0,0,0"
        cli("import", "accounts_rebuild_fixture")
        assert counts("accounts_rebuild_fixture") == "1,1,2"
        replay = cli("import", "accounts_rebuild_fixture")
        assert "users inserted=0 updated=0" in replay and "Sessions inserted=0" in replay
        for table in ("users", "identities", "sessions"):
            columns = sql("legacy_fixture", f"SELECT string_agg(quote_ident(column_name),',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_name='{table}'")
            source = sql("legacy_fixture", f"SELECT row_to_json(t) FROM (SELECT {columns} FROM {table} ORDER BY uuid)t")
            target = sql("accounts_rebuild_fixture", f"SELECT row_to_json(t) FROM (SELECT {columns} FROM {table} ORDER BY uuid)t")
            assert source == target, f"stable field mismatch in {table}"
        assert sql("accounts_rebuild_fixture", "SELECT count(*) FROM identities WHERE created_at IS NULL OR updated_at IS NULL") == "0"
        assert sql("accounts_rebuild_fixture", "SELECT count(*) FROM subscriptions") == "0"
        assert sql("legacy_fixture", "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'") == "3"
        cli("import", "legacy_fixture", success=False)  # non-independent target
        for flag in ("--merge", "--regenerate-user-uuids", "--skip-sessions"):
            cli("import", "accounts_rebuild_empty", flag, success=False)
        sql("accounts_rebuild_fixture", "UPDATE users SET active=true")
        cli("import", "accounts_rebuild_fixture", success=False)  # conflicting UUID profile
        sql("accounts_rebuild_fixture", "DELETE FROM sessions")
        cli("import", "accounts_rebuild_fixture", success=False)  # no partial repair
        assert counts("accounts_rebuild_fixture") == "1,1,0"
        sql("accounts_rebuild_rollback", "ALTER TABLE sessions ADD CONSTRAINT fixture_reject CHECK (token <> 'synthetic-active-token')")
        cli("import", "accounts_rebuild_rollback", success=False)
        assert counts("accounts_rebuild_rollback") == "0,0,0"
        sql("legacy_fixture", "ALTER TABLE identities ADD created_at timestamptz")
        cli("export", "legacy_fixture")
        cli("import", "accounts_rebuild_null", success=False)
        assert counts("accounts_rebuild_null") == "0,0,0"
        sql("legacy_fixture", "ALTER TABLE identities DROP COLUMN created_at; DELETE FROM identities; DELETE FROM sessions")
        cli("export", "legacy_fixture")
        cli("import", "accounts_rebuild_empty")
        cli("import", "accounts_rebuild_empty")
        assert counts("accounts_rebuild_empty") == "1,0,0"
        sql("accounts_rebuild_same", "INSERT INTO users(uuid,username,password,proxy_uuid) VALUES('55555555-5555-4555-8555-555555555555','same','synthetic','55555555-5555-4555-8555-555555555555')")
        cli("export", "accounts_rebuild_same")
        cli("import", "accounts_rebuild_same", success=False)
        sql("legacy_fixture", "ALTER TABLE users ADD plan text DEFAULT 'unreviewed'")
        cli("export", "legacy_fixture", success=False)
        print(json.dumps({"scope":"accounts-only", "data":"synthetic-fixture", "result":"passed",
                          "checks":["real-export-insert-exact-replay", "uuid-email-hash-permissions-active-fk",
                                    "missing-timestamp-default-timezone-microseconds", "explicit-null-rejected",
                                    "empty-identities-sessions", "dry-run-no-row-write", "full-rollback-mid-insert",
                                    "conflict-and-partial-target-rejected", "merge-rekey-skip-rejected",
                                    "same-and-nonisolated-target-rejected", "unknown-plan-rejected", "subscriptions-empty"],
                          "real_original_user_login":"not_run", "production_promotion":False}))
    finally:
        if started:
            run(["pg_ctl", "-D", str(data), "-m", "fast", "-w", "stop"])
