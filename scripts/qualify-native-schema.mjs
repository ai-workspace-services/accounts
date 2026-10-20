// Development fixture only. Execute after native-schema-render against an empty
// local accounts_native_render database. Never accepts an application database.
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync, readdirSync } from 'node:fs';
import { createHash } from 'node:crypto';

const dsn = process.env.NATIVE_SCHEMA_RENDER_DSN;
const url = new URL(dsn);
if (!['127.0.0.1', 'localhost'].includes(url.hostname) || url.pathname !== '/accounts_native_render') {
  throw new Error('requires local accounts_native_render fixture');
}
const bin = process.env.PG17_BIN;
if (!bin || !execFileSync(`${bin}/pg_dump`, ['--version'], { encoding: 'utf8' }).includes(' 17.')) {
  throw new Error('explicit PostgreSQL 17 binaries required');
}
const env = { ...process.env, PGHOST: url.hostname, PGPORT: url.port || '5432',
  PGDATABASE: 'accounts_native_render', PGUSER: decodeURIComponent(url.username),
  PGPASSWORD: decodeURIComponent(url.password) };
const query = sql => JSON.parse(execFileSync(`${bin}/psql`, ['-XAt', '-v', 'ON_ERROR_STOP=1', '-c', sql], { env, encoding: 'utf8' }));
const tables = query(`SELECT json_agg(tablename ORDER BY tablename) FROM pg_tables
 WHERE schemaname='public' AND tablename NOT IN ('schema_migrations','accounts_controlled_migration_history')`);
const old = JSON.parse(readFileSync('sql/init/accounts-native.manifest.json', 'utf8'));
const expected = [...old.business_tables.filter(x => x !== 'subscription_lifecycle_states'), 'subscription_lifecycle_states'].sort();
if (JSON.stringify(tables) !== JSON.stringify(expected)) throw new Error('unreviewed fixture table scope');
let dump = execFileSync(`${bin}/pg_dump`, ['--schema-only', '--schema=public', '--no-owner', '--no-privileges',
  '--exclude-table=public.schema_migrations', '--exclude-table=public.accounts_controlled_migration_history'], { env, encoding: 'utf8' });
dump = dump.split('\n').filter(line => !line.startsWith('\\') &&
  !/^CREATE SCHEMA public;/.test(line) && !/^COMMENT ON SCHEMA public /.test(line) &&
  !/^SET (lock|statement)_timeout = 0;/.test(line)).join('\n');
const header = '-- Latest native Accounts business schema for absent/empty PostgreSQL only.\n' +
  '-- Generated from the isolated PostgreSQL 17 fixture; no business rows.\n' +
  'CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;\n';
const body = (header + dump).trimEnd() + '\n';
if (/^(DROP |INSERT |UPDATE |GRANT |REVOKE |\\)/m.test(body)) throw new Error('non-schema statement');
const hash = bytes => createHash('sha256').update(bytes).digest('hex');
const sourcePaths = [...new Set([...old.source_files.filter(s => !s.path.startsWith('sql/migrations/')).map(s => s.path),
  'scripts/qualify-native-schema.mjs', ...readdirSync('sql/migrations').filter(x => x.endsWith('.up.sql')).map(x => `sql/migrations/${x}`)])].sort();
const manifest = { format: 1, migration_version: Math.max(...sourcePaths.filter(x => x.startsWith('sql/migrations/')).map(x => Number(x.split('/').at(-1).split('_')[0]))),
  schema_sha256: hash(body), business_table_count: tables.length,
  source_files: sourcePaths.map(path => ({ path, sha256: hash(readFileSync(path)) })), business_tables: tables };
writeFileSync('sql/init/accounts-native.sql', body);
writeFileSync('sql/init/accounts-native.manifest.json', JSON.stringify(manifest, null, 2) + '\n');
console.log(JSON.stringify({ qualified_tables: tables.length, migration_version: manifest.migration_version, schema_sha256: manifest.schema_sha256 }));
