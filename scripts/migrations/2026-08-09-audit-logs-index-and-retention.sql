-- 针对频繁迁移/审计场景导致的 audit_logs 表暴胀（12GB）问题的修复补丁。
--
-- 核心优化点：
-- 1. 创建按 created_at 倒序的索引 idx_audit_logs_created_at，消灭全表扫描。
-- 2. 创建 (action, created_at) 复合索引，加速特定迁移或高频接口日志筛选。
-- 3. 提供批次清理过期审计日志函数 clean_expired_audit_logs()，按批次释放空间防膨胀。

CREATE TABLE IF NOT EXISTS public.audit_logs (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  action TEXT NOT NULL DEFAULT '',
  actor_uuid UUID,
  details JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- CONCURRENTLY 创建索引，线上无锁执行
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_audit_logs_created_at
ON public.audit_logs (created_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_audit_logs_action_created_at
ON public.audit_logs (action, created_at DESC);

-- 分批清理过期审计日志函数（防止单次大 DELETE 锁表与产生大容量 WAL）
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
