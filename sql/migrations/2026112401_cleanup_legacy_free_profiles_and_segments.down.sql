-- 2026112401_cleanup_legacy_free_profiles_and_segments 清理历史遗留免费标签与配额档位，
-- 若需回滚请从执行前全量 pg_dump 备份恢复：
--   pg_restore / psql -d account < pre-2026112401-backup.sql
SELECT 1;
