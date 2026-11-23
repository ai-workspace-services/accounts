-- 2026112301_decommission_free_payg_whitelist 涉及目录清理、测试账号清除及用户配额口径切换，
-- 若需回滚请在切换前通过 pg_dump 全量备份恢复：
--   pg_restore / psql -d account < pre-2026112301-backup.sql
SELECT 1;
