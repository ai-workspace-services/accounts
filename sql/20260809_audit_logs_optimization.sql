-- 20260809_audit_logs_optimization.sql
-- 针对审计日志表（audit_logs）进行并发索引建立与清理优化，消除全表扫描，防止数据爆满至 12GB。

-- 1. 确保 audit_logs 基础表结构存在
CREATE TABLE IF NOT EXISTS public.audit_logs (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  action TEXT NOT NULL DEFAULT '',
  actor_uuid UUID,
  details JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 2. 并发建立创建时间 B-Tree 索引（线上不锁表，消除全表扫描）
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_audit_logs_created_at
ON public.audit_logs (created_at DESC);

-- 3. 按操作类型+创建时间的复合并发索引（快速过滤高频事件或迁移审计日志）
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_audit_logs_action_created_at
ON public.audit_logs (action, created_at DESC);

-- 4. 辅助存储过程：支持过期审计日志按批次批量清理（避免大事务锁表膨胀）
CREATE OR REPLACE FUNCTION public.clean_expired_audit_logs(retention_days INT DEFAULT 30, batch_size INT DEFAULT 5000)
RETURNS INT
LANGUAGE plpgsql AS $$
DECLARE
  deleted_count INT := 0;
  total_deleted INT := 0;
  cutoff_time TIMESTAMPTZ;
BEGIN
  cutoff_time := now() - (retention_days || ' days')::INTERVAL;
  LOOP
    DELETE FROM public.audit_logs
    WHERE uuid IN (
      SELECT uuid FROM public.audit_logs
      WHERE created_at < cutoff_time
      LIMIT batch_size
    );
    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    total_deleted := total_deleted + deleted_count;
    EXIT WHEN deleted_count = 0;
  END LOOP;
  RETURN total_deleted;
END;
$$;
