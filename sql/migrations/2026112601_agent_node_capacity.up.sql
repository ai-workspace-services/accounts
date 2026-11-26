CREATE SCHEMA accounts_node_capacity;

CREATE TABLE accounts_node_capacity.agent_node_capacity (
    agent_id text PRIMARY KEY,
    monthly_limit_bytes bigint CHECK (monthly_limit_bytes > 0),
    bandwidth_limit_mbps integer CHECK (bandwidth_limit_mbps > 0),
    interface text,
    boot_id text,
    rx_bytes bigint CHECK (rx_bytes >= 0),
    tx_bytes bigint CHECK (tx_bytes >= 0),
    sampled_at timestamptz
);

CREATE TABLE accounts_node_capacity.agent_node_monthly_traffic (
    agent_id text NOT NULL REFERENCES accounts_node_capacity.agent_node_capacity (agent_id),
    month date NOT NULL,
    used_bytes bigint CHECK (used_bytes >= 0),
    observed_from timestamptz NOT NULL,
    sampled_at timestamptz NOT NULL,
    PRIMARY KEY (agent_id, month)
);

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'account_user') THEN
        GRANT USAGE ON SCHEMA accounts_node_capacity TO account_user;
        IF to_regclass('public.accounts_controlled_migration_history') IS NOT NULL THEN
            GRANT SELECT ON public.accounts_controlled_migration_history TO account_user;
        END IF;
        GRANT SELECT, INSERT, UPDATE ON
            accounts_node_capacity.agent_node_capacity,
            accounts_node_capacity.agent_node_monthly_traffic TO account_user;
    END IF;
END
$$;
