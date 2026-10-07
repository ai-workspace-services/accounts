-- Latest native Accounts business schema for absent/empty PostgreSQL only.
-- Apply through migratectl init with its reviewed hash and an empty-database guard.
-- No business rows, historical migration replay, environment grants or replication config.
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

--
-- PostgreSQL database dump
--


-- Dumped from database version 17.11
-- Dumped by pg_dump version 17.11


--
-- Name: public; Type: SCHEMA; Schema: -; Owner: -
--



--
-- Name: bump_version(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_version() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    NEW.version := COALESCE(OLD.version, 0) + 1;
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: reject_account_lifecycle_event_mutation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_account_lifecycle_event_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  RAISE EXCEPTION 'account lifecycle events are immutable'
    USING ERRCODE = '55000';
END;
$$;


--
-- Name: reject_finance_fact_mutation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_finance_fact_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  RAISE EXCEPTION '% is append-only; % is not permitted', TG_TABLE_NAME, TG_OP;
  RETURN OLD;
END;
$$;


--
-- Name: reject_user_hard_delete(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_user_hard_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  RAISE EXCEPTION 'users are retained; archive the account instead'
    USING ERRCODE = '55000';
END;
$$;


--
-- Name: set_updated_at(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.set_updated_at() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;


--
-- Name: validate_finance_invoice_subscription(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_finance_invoice_subscription() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.subscription_uuid IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM public.subscriptions s
    WHERE s.uuid = NEW.subscription_uuid AND s.user_uuid = NEW.account_uuid
  ) THEN
    RAISE EXCEPTION 'subscription % does not belong to account %', NEW.subscription_uuid, NEW.account_uuid;
  END IF;
  RETURN NEW;
END;
$$;




--
-- Name: account_billing_profiles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.account_billing_profiles (
    account_uuid uuid NOT NULL,
    package_name text DEFAULT 'default'::text NOT NULL,
    included_quota_bytes bigint DEFAULT 0 NOT NULL,
    base_price_per_byte double precision DEFAULT 0 NOT NULL,
    region_multiplier double precision DEFAULT 1.0 NOT NULL,
    line_multiplier double precision DEFAULT 1.0 NOT NULL,
    peak_multiplier double precision DEFAULT 1.0 NOT NULL,
    offpeak_multiplier double precision DEFAULT 1.0 NOT NULL,
    pricing_rule_version text DEFAULT 'pricing-default-v1'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: account_lifecycle_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.account_lifecycle_events (
    transition_id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_uuid uuid NOT NULL,
    from_state text,
    to_state text NOT NULL,
    actor_type text NOT NULL,
    actor_ref text,
    reason text DEFAULT ''::text NOT NULL,
    request_id text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT account_lifecycle_events_actor_type_ck CHECK ((actor_type = ANY (ARRAY['user'::text, 'admin'::text, 'system'::text, 'service'::text]))),
    CONSTRAINT account_lifecycle_events_from_state_ck CHECK (((from_state IS NULL) OR (from_state = ANY (ARRAY['active'::text, 'archived'::text, 'self_cancelled'::text, 'reactivation_pending'::text])))),
    CONSTRAINT account_lifecycle_events_metadata_object_ck CHECK ((jsonb_typeof(metadata) = 'object'::text)),
    CONSTRAINT account_lifecycle_events_state_change_ck CHECK (((from_state IS NULL) OR (from_state <> to_state))),
    CONSTRAINT account_lifecycle_events_to_state_ck CHECK ((to_state = ANY (ARRAY['active'::text, 'archived'::text, 'self_cancelled'::text, 'reactivation_pending'::text])))
);


--
-- Name: account_policy_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.account_policy_snapshots (
    account_uuid uuid NOT NULL,
    policy_version text NOT NULL,
    auth_state text DEFAULT 'active'::text NOT NULL,
    rate_profile text DEFAULT 'standard'::text NOT NULL,
    conn_profile text DEFAULT 'standard'::text NOT NULL,
    eligible_node_groups jsonb DEFAULT '[]'::jsonb NOT NULL,
    preferred_strategy text DEFAULT 'ewma'::text NOT NULL,
    degrade_mode text DEFAULT 'deny'::text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: account_quota_states; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.account_quota_states (
    account_uuid uuid NOT NULL,
    remaining_included_quota bigint DEFAULT 0 NOT NULL,
    current_balance double precision DEFAULT 0 NOT NULL,
    arrears boolean DEFAULT false NOT NULL,
    throttle_state text DEFAULT 'normal'::text NOT NULL,
    suspend_state text DEFAULT 'active'::text NOT NULL,
    proxy_access_state text DEFAULT 'active'::text NOT NULL,
    last_rated_bucket_at timestamp with time zone,
    effective_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    arrears_since timestamp with time zone,
    period_start timestamp with time zone,
    period_end timestamp with time zone
);


--
-- Name: admin_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.admin_settings (
    uuid uuid DEFAULT gen_random_uuid() NOT NULL,
    module_key text NOT NULL,
    role text NOT NULL,
    enabled boolean NOT NULL,
    version bigint NOT NULL,
    origin_node text DEFAULT 'local'::text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: agents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agents (
    id text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    groups jsonb DEFAULT '[]'::jsonb NOT NULL,
    healthy boolean DEFAULT false NOT NULL,
    last_heartbeat timestamp with time zone,
    clients_count integer DEFAULT 0 NOT NULL,
    sync_revision text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: audit_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audit_logs (
    uuid uuid DEFAULT gen_random_uuid() NOT NULL,
    action text DEFAULT ''::text NOT NULL,
    actor_uuid uuid,
    details jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: billing_ledger; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.billing_ledger (
    id uuid NOT NULL,
    account_uuid uuid NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    bucket_end timestamp with time zone NOT NULL,
    entry_type text NOT NULL,
    rated_bytes bigint DEFAULT 0 NOT NULL,
    amount_delta double precision DEFAULT 0 NOT NULL,
    balance_after double precision DEFAULT 0 NOT NULL,
    pricing_rule_version text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: billing_plans; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.billing_plans (
    plan_id text NOT NULL,
    stripe_price_id text,
    display_name text DEFAULT ''::text NOT NULL,
    kind text DEFAULT 'subscription'::text NOT NULL,
    included_quota_bytes bigint DEFAULT 0 NOT NULL,
    package_name text DEFAULT 'default'::text NOT NULL,
    price_amount bigint DEFAULT 0 NOT NULL,
    price_currency text DEFAULT ''::text NOT NULL,
    price_unit text DEFAULT ''::text NOT NULL,
    price_multipliers jsonb DEFAULT '{}'::jsonb NOT NULL,
    features jsonb DEFAULT '{}'::jsonb NOT NULL,
    trial_days integer DEFAULT 0 NOT NULL,
    active boolean DEFAULT true NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: billing_source_sync_state; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.billing_source_sync_state (
    source_id text NOT NULL,
    last_completed_until timestamp with time zone,
    last_attempted_at timestamp with time zone,
    last_succeeded_at timestamp with time zone,
    last_error text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: bridge_credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.bridge_credentials (
    credential_uuid uuid NOT NULL,
    user_uuid uuid NOT NULL,
    tenant_id text NOT NULL,
    token_hash text,
    token_prefix text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    source text DEFAULT 'generated'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone
);


--
-- Name: email_blacklist; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.email_blacklist (
    uuid uuid DEFAULT gen_random_uuid() NOT NULL,
    email text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: finance_invoices; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.finance_invoices (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    idempotency_key text NOT NULL,
    account_uuid uuid NOT NULL,
    subscription_uuid uuid,
    provider text DEFAULT 'local'::text NOT NULL,
    provider_invoice_id text,
    amount_minor bigint NOT NULL,
    currency character(3) NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    issued_at timestamp with time zone DEFAULT now() NOT NULL,
    due_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT finance_invoices_amount_minor_check CHECK ((amount_minor >= 0)),
    CONSTRAINT finance_invoices_currency_check CHECK ((currency ~ '^[A-Z]{3}$'::text))
);


--
-- Name: finance_operation_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.finance_operation_events (
    id bigint NOT NULL,
    operation_id uuid NOT NULL,
    attempt integer NOT NULL,
    event_type text NOT NULL,
    status text NOT NULL,
    provider_operation_id text,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT finance_operation_events_attempt_check CHECK ((attempt > 0))
);


--
-- Name: finance_operation_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.finance_operation_events ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.finance_operation_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: finance_operations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.finance_operations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    idempotency_key text NOT NULL,
    operation_type text NOT NULL,
    target_type text DEFAULT ''::text NOT NULL,
    target_id text DEFAULT ''::text NOT NULL,
    provider text DEFAULT 'local'::text NOT NULL,
    provider_operation_id text,
    status text DEFAULT 'pending'::text NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone,
    last_error text DEFAULT ''::text NOT NULL,
    request jsonb DEFAULT '{}'::jsonb NOT NULL,
    response jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT finance_operations_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT finance_operations_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'in_progress'::text, 'succeeded'::text, 'failed'::text, 'reconcile_needed'::text])))
);


--
-- Name: finance_payments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.finance_payments (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    idempotency_key text NOT NULL,
    invoice_id uuid NOT NULL,
    account_uuid uuid NOT NULL,
    provider text DEFAULT 'local'::text NOT NULL,
    provider_payment_id text,
    amount_minor bigint NOT NULL,
    currency character(3) NOT NULL,
    paid_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT finance_payments_amount_minor_check CHECK ((amount_minor > 0)),
    CONSTRAINT finance_payments_currency_check CHECK ((currency ~ '^[A-Z]{3}$'::text))
);


--
-- Name: finance_refunds; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.finance_refunds (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    idempotency_key text NOT NULL,
    payment_id uuid NOT NULL,
    provider text DEFAULT 'local'::text NOT NULL,
    provider_refund_id text,
    amount_minor bigint NOT NULL,
    currency character(3) NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    refunded_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT finance_refunds_amount_minor_check CHECK ((amount_minor > 0)),
    CONSTRAINT finance_refunds_currency_check CHECK ((currency ~ '^[A-Z]{3}$'::text))
);


--
-- Name: homepage_video_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.homepage_video_settings (
    uuid uuid NOT NULL,
    domain_key text NOT NULL,
    video_url text NOT NULL,
    poster_url text DEFAULT ''::text NOT NULL,
    updated_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: identities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.identities (
    uuid uuid DEFAULT gen_random_uuid() NOT NULL,
    provider text NOT NULL,
    external_id text NOT NULL,
    user_uuid uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    version bigint DEFAULT 0 NOT NULL,
    origin_node text DEFAULT 'local'::text NOT NULL
);


--
-- Name: mfa_recovery_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mfa_recovery_codes (
    id uuid NOT NULL,
    user_uuid uuid NOT NULL,
    batch_uuid uuid NOT NULL,
    code_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    failed_attempts integer DEFAULT 0 NOT NULL,
    locked_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    consumed_at timestamp with time zone,
    revoked_at timestamp with time zone,
    CONSTRAINT mfa_recovery_codes_attempts_ck CHECK ((failed_attempts >= 0)),
    CONSTRAINT mfa_recovery_codes_hash_ck CHECK ((length(code_hash) > 0)),
    CONSTRAINT mfa_recovery_codes_terminal_state_ck CHECK (((consumed_at IS NULL) OR (revoked_at IS NULL)))
);


--
-- Name: node_health_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.node_health_snapshots (
    node_id text NOT NULL,
    region text DEFAULT ''::text NOT NULL,
    line_code text DEFAULT ''::text NOT NULL,
    pricing_group text DEFAULT ''::text NOT NULL,
    stats_enabled boolean DEFAULT false NOT NULL,
    xray_revision text DEFAULT ''::text NOT NULL,
    healthy boolean DEFAULT false NOT NULL,
    latency_ms integer DEFAULT 0 NOT NULL,
    error_rate double precision DEFAULT 0 NOT NULL,
    active_connections integer DEFAULT 0 NOT NULL,
    health_score double precision DEFAULT 0 NOT NULL,
    sampled_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: nodes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.nodes (
    uuid uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    location text NOT NULL,
    address text NOT NULL,
    port integer DEFAULT 443 NOT NULL,
    server_name text,
    protocols jsonb DEFAULT '[]'::jsonb NOT NULL,
    available boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    version bigint DEFAULT 0 NOT NULL,
    origin_node text DEFAULT 'local'::text NOT NULL
);


--
-- Name: oauth_exchange_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.oauth_exchange_codes (
    code text NOT NULL,
    session_token text NOT NULL,
    session_expires_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: overlay_config_acks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.overlay_config_acks (
    user_uuid uuid NOT NULL,
    device_id text NOT NULL,
    network_id text DEFAULT 'xworkmate-private'::text NOT NULL,
    revision text NOT NULL,
    digest text DEFAULT ''::text NOT NULL,
    applied_at timestamp with time zone NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: overlay_device_credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.overlay_device_credentials (
    id text NOT NULL,
    device_id text NOT NULL,
    credential_id text NOT NULL,
    token_hash text NOT NULL,
    issued_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL
);


--
-- Name: overlay_devices; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.overlay_devices (
    id text NOT NULL,
    user_uuid uuid NOT NULL,
    network_id text NOT NULL,
    name text NOT NULL,
    platform text NOT NULL,
    hostname text NOT NULL,
    wireguard_public_key text NOT NULL,
    wireguard_address text NOT NULL,
    last_seen_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    user_id text,
    role text DEFAULT 'one'::text NOT NULL,
    status text NOT NULL
);


--
-- Name: overlay_enrollment_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.overlay_enrollment_sessions (
    id text NOT NULL,
    device_id text NOT NULL,
    token_hash text NOT NULL,
    issued_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL
);


--
-- Name: overlay_invites; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.overlay_invites (
    id text NOT NULL,
    network_id text NOT NULL,
    token_hash text NOT NULL,
    device_id text,
    platform text,
    role text DEFAULT 'one'::text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    remaining_uses bigint NOT NULL,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL
);


--
-- Name: overlay_networks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.overlay_networks (
    id text NOT NULL,
    display_name text NOT NULL,
    cidr text NOT NULL,
    gateway_id text NOT NULL,
    gateway_wireguard_key text NOT NULL,
    gateway_wireguard_address text DEFAULT ''::text NOT NULL,
    gateway_endpoint_host text NOT NULL,
    gateway_endpoint_port bigint NOT NULL,
    transport_server_name text NOT NULL,
    transport_port bigint NOT NULL,
    transport_auth_id text NOT NULL,
    transport_kind text DEFAULT 'vless-xhttp'::text NOT NULL,
    transport_path text DEFAULT '/xconnect'::text NOT NULL,
    transport_mode text DEFAULT 'auto'::text NOT NULL,
    transport_host text DEFAULT ''::text NOT NULL,
    gateway_frontend text DEFAULT ''::text NOT NULL,
    gateway_listen_socket text DEFAULT ''::text NOT NULL,
    owner_user_id text,
    policy_json text DEFAULT ''::text NOT NULL,
    config_generation bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: overlay_nodes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.overlay_nodes (
    id text NOT NULL,
    network_id text DEFAULT 'xworkmate-private'::text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    role text DEFAULT 'gateway'::text NOT NULL,
    region text DEFAULT ''::text NOT NULL,
    wireguard_public_key text NOT NULL,
    wireguard_address text NOT NULL,
    endpoint_host text NOT NULL,
    endpoint_port integer DEFAULT 2443 NOT NULL,
    transport_type text DEFAULT 'vless-tls'::text NOT NULL,
    transport_security text DEFAULT 'tls'::text NOT NULL,
    transport_path text DEFAULT ''::text NOT NULL,
    transport_mode text DEFAULT ''::text NOT NULL,
    transport_uuid text DEFAULT ''::text NOT NULL,
    healthy boolean DEFAULT false NOT NULL,
    last_heartbeat timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: overlay_registrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.overlay_registrations (
    id text NOT NULL,
    network_id text NOT NULL,
    owner_user_id text NOT NULL,
    device_id text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    hostname text DEFAULT ''::text NOT NULL,
    platform text NOT NULL,
    wireguard_public_key text NOT NULL,
    wireguard_public_key_fingerprint text NOT NULL,
    token_hash text NOT NULL,
    status text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    approved_at timestamp with time zone,
    rejected_at timestamp with time zone,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: overlay_signed_config_acks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.overlay_signed_config_acks (
    id text NOT NULL,
    device_id text NOT NULL,
    network_id text NOT NULL,
    generation bigint NOT NULL,
    config_id text NOT NULL,
    applied_at timestamp with time zone NOT NULL,
    received_at timestamp with time zone NOT NULL
);


--
-- Name: password_recovery_challenges; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.password_recovery_challenges (
    id uuid NOT NULL,
    user_uuid uuid NOT NULL,
    email_snapshot text NOT NULL,
    challenge_kind text NOT NULL,
    secret_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    failed_attempts integer DEFAULT 0 NOT NULL,
    locked_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    consumed_at timestamp with time zone,
    invalidated_at timestamp with time zone,
    CONSTRAINT password_recovery_challenges_attempts_ck CHECK ((failed_attempts >= 0)),
    CONSTRAINT password_recovery_challenges_hash_ck CHECK ((length(secret_hash) > 0)),
    CONSTRAINT password_recovery_challenges_kind_ck CHECK ((challenge_kind = ANY (ARRAY['token'::text, 'code'::text]))),
    CONSTRAINT password_recovery_challenges_terminal_state_ck CHECK (((consumed_at IS NULL) OR (invalidated_at IS NULL)))
);


--
-- Name: rbac_permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rbac_permissions (
    permission_key text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: rbac_role_permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rbac_role_permissions (
    role_key text NOT NULL,
    permission_key text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: rbac_roles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rbac_roles (
    role_key text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    priority integer DEFAULT 100 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: sandbox_bindings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sandbox_bindings (
    id bigint NOT NULL,
    agent_id text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: sandbox_bindings_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.sandbox_bindings_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: sandbox_bindings_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.sandbox_bindings_id_seq OWNED BY public.sandbox_bindings.id;


--
-- Name: scheduler_decisions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.scheduler_decisions (
    id uuid NOT NULL,
    account_uuid uuid,
    node_group text DEFAULT ''::text NOT NULL,
    strategy text DEFAULT ''::text NOT NULL,
    decision text DEFAULT ''::text NOT NULL,
    generated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    uuid uuid DEFAULT gen_random_uuid() NOT NULL,
    token text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    user_uuid uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    version bigint DEFAULT 0 NOT NULL,
    origin_node text DEFAULT 'local'::text NOT NULL
);


--
-- Name: stripe_webhook_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.stripe_webhook_events (
    event_id text NOT NULL,
    event_type text DEFAULT ''::text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    status text DEFAULT 'received'::text NOT NULL,
    last_error text DEFAULT ''::text NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    processed_at timestamp with time zone
);


--
-- Name: subscriptions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.subscriptions (
    uuid uuid DEFAULT gen_random_uuid() NOT NULL,
    user_uuid uuid NOT NULL,
    provider text NOT NULL,
    payment_method text DEFAULT 'paypal'::text NOT NULL,
    kind text DEFAULT 'subscription'::text NOT NULL,
    plan_id text,
    external_id text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    payment_qr text,
    meta jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    cancelled_at timestamp with time zone
);


--
-- Name: task_namespaces; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.task_namespaces (
    id text NOT NULL,
    account_uuid uuid NOT NULL,
    slug text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    max_active_runs integer DEFAULT 2 NOT NULL,
    last_claimed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT task_namespaces_max_active_runs_check CHECK (((max_active_runs > 0) AND (max_active_runs <= 2)))
);


--
-- Name: task_runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.task_runs (
    id text NOT NULL,
    account_uuid uuid NOT NULL,
    namespace_id text NOT NULL,
    session_id text NOT NULL,
    client_request_id text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'queued'::text NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    not_before timestamp with time zone DEFAULT now() NOT NULL,
    attempt integer DEFAULT 0 NOT NULL,
    lease_owner text DEFAULT ''::text NOT NULL,
    lease_token_hash text DEFAULT ''::text NOT NULL,
    lease_expires_at timestamp with time zone,
    fence bigint DEFAULT 0 NOT NULL,
    routing jsonb DEFAULT '{}'::jsonb NOT NULL,
    bridge_task_ref text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT task_runs_routing_check CHECK ((jsonb_typeof(routing) = 'object'::text)),
    CONSTRAINT task_runs_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'running'::text, 'completed'::text, 'failed'::text, 'cancelled'::text])))
);


--
-- Name: task_session_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.task_session_events (
    session_id text NOT NULL,
    seq bigint NOT NULL,
    event_type text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    actor_id text DEFAULT ''::text NOT NULL,
    client_request_id text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT task_session_events_payload_check CHECK ((jsonb_typeof(payload) = 'object'::text)),
    CONSTRAINT task_session_events_payload_check1 CHECK ((octet_length((payload)::text) <= 16384))
);


--
-- Name: task_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.task_sessions (
    id text NOT NULL,
    account_uuid uuid NOT NULL,
    namespace_id text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    snapshot_version bigint DEFAULT 0 NOT NULL,
    last_event_seq bigint DEFAULT 0 NOT NULL,
    lifecycle_state text DEFAULT 'ready'::text NOT NULL,
    context_summary jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT task_sessions_context_summary_check CHECK ((jsonb_typeof(context_summary) = 'object'::text)),
    CONSTRAINT task_sessions_context_summary_check1 CHECK ((octet_length((context_summary)::text) <= 131072))
);


--
-- Name: tenant_domains; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tenant_domains (
    id text NOT NULL,
    tenant_id text NOT NULL,
    domain text NOT NULL,
    kind text NOT NULL,
    is_primary boolean DEFAULT false NOT NULL,
    status text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: tenant_memberships; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tenant_memberships (
    tenant_id text NOT NULL,
    user_id text NOT NULL,
    role text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: tenants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tenants (
    id text NOT NULL,
    name text NOT NULL,
    edition text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: traffic_minute_buckets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.traffic_minute_buckets (
    bucket_start timestamp with time zone NOT NULL,
    node_id text NOT NULL,
    account_uuid uuid NOT NULL,
    region text DEFAULT ''::text NOT NULL,
    line_code text DEFAULT ''::text NOT NULL,
    uplink_bytes bigint DEFAULT 0 NOT NULL,
    downlink_bytes bigint DEFAULT 0 NOT NULL,
    total_bytes bigint DEFAULT 0 NOT NULL,
    multiplier double precision DEFAULT 1.0 NOT NULL,
    rating_status text DEFAULT 'pending'::text NOT NULL,
    source_revision text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: traffic_stat_checkpoints; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.traffic_stat_checkpoints (
    node_id text NOT NULL,
    account_uuid uuid NOT NULL,
    last_uplink_total bigint DEFAULT 0 NOT NULL,
    last_downlink_total bigint DEFAULT 0 NOT NULL,
    last_seen_at timestamp with time zone NOT NULL,
    xray_revision text DEFAULT ''::text NOT NULL,
    reset_epoch bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    uuid uuid DEFAULT gen_random_uuid() NOT NULL,
    username text NOT NULL,
    password text NOT NULL,
    email text,
    role text DEFAULT 'user'::text NOT NULL,
    level integer DEFAULT 20 NOT NULL,
    groups jsonb DEFAULT '[]'::jsonb NOT NULL,
    permissions jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    version bigint DEFAULT 0 NOT NULL,
    origin_node text DEFAULT 'local'::text NOT NULL,
    mfa_totp_secret text,
    mfa_enabled boolean DEFAULT false NOT NULL,
    mfa_secret_issued_at timestamp with time zone,
    mfa_confirmed_at timestamp with time zone,
    email_verified_at timestamp with time zone,
    email_verified boolean GENERATED ALWAYS AS ((email_verified_at IS NOT NULL)) STORED,
    active boolean DEFAULT true NOT NULL,
    proxy_uuid uuid NOT NULL,
    proxy_uuid_expires_at timestamp with time zone,
    subscription_valid_from timestamp with time zone,
    subscription_valid_until timestamp with time zone,
    last_active_at timestamp with time zone,
    archived_at timestamp with time zone,
    account_lifecycle_state text DEFAULT 'active'::text NOT NULL,
    account_lifecycle_changed_at timestamp with time zone,
    account_lifecycle_actor_type text,
    account_lifecycle_actor_ref text,
    account_lifecycle_reason text,
    account_lifecycle_transition_id uuid,
    CONSTRAINT users_account_lifecycle_state_ck CHECK ((account_lifecycle_state = ANY (ARRAY['active'::text, 'archived'::text, 'self_cancelled'::text, 'reactivation_pending'::text]))),
    CONSTRAINT users_subscription_validity_order_ck CHECK (((subscription_valid_from IS NULL) OR (subscription_valid_until IS NULL) OR (subscription_valid_until >= subscription_valid_from)))
);


--
-- Name: xworkmate_profiles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.xworkmate_profiles (
    id text NOT NULL,
    tenant_id text NOT NULL,
    user_id text DEFAULT ''::text NOT NULL,
    scope text NOT NULL,
    openclaw_url text DEFAULT ''::text NOT NULL,
    openclaw_origin text DEFAULT ''::text NOT NULL,
    vault_url text DEFAULT ''::text NOT NULL,
    vault_namespace text DEFAULT ''::text NOT NULL,
    vault_secret_path text DEFAULT ''::text NOT NULL,
    vault_secret_key text DEFAULT ''::text NOT NULL,
    secret_locators text DEFAULT '[]'::text NOT NULL,
    apisix_url text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: sandbox_bindings id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sandbox_bindings ALTER COLUMN id SET DEFAULT nextval('public.sandbox_bindings_id_seq'::regclass);


--
-- Name: account_billing_profiles account_billing_profiles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_billing_profiles
    ADD CONSTRAINT account_billing_profiles_pkey PRIMARY KEY (account_uuid);


--
-- Name: account_lifecycle_events account_lifecycle_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_lifecycle_events
    ADD CONSTRAINT account_lifecycle_events_pkey PRIMARY KEY (transition_id);


--
-- Name: account_policy_snapshots account_policy_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_policy_snapshots
    ADD CONSTRAINT account_policy_snapshots_pkey PRIMARY KEY (account_uuid);


--
-- Name: account_quota_states account_quota_states_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_quota_states
    ADD CONSTRAINT account_quota_states_pkey PRIMARY KEY (account_uuid);


--
-- Name: admin_settings admin_settings_module_role_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.admin_settings
    ADD CONSTRAINT admin_settings_module_role_uk UNIQUE (module_key, role);


--
-- Name: admin_settings admin_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.admin_settings
    ADD CONSTRAINT admin_settings_pkey PRIMARY KEY (uuid);


--
-- Name: agents agents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT agents_pkey PRIMARY KEY (id);


--
-- Name: audit_logs audit_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_logs
    ADD CONSTRAINT audit_logs_pkey PRIMARY KEY (uuid);


--
-- Name: billing_ledger billing_ledger_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_ledger
    ADD CONSTRAINT billing_ledger_pkey PRIMARY KEY (id);


--
-- Name: billing_plans billing_plans_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_plans
    ADD CONSTRAINT billing_plans_pkey PRIMARY KEY (plan_id);


--
-- Name: billing_plans billing_plans_stripe_price_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_plans
    ADD CONSTRAINT billing_plans_stripe_price_id_key UNIQUE (stripe_price_id);


--
-- Name: billing_source_sync_state billing_source_sync_state_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_source_sync_state
    ADD CONSTRAINT billing_source_sync_state_pkey PRIMARY KEY (source_id);


--
-- Name: bridge_credentials bridge_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bridge_credentials
    ADD CONSTRAINT bridge_credentials_pkey PRIMARY KEY (credential_uuid);


--
-- Name: email_blacklist email_blacklist_email_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.email_blacklist
    ADD CONSTRAINT email_blacklist_email_key UNIQUE (email);


--
-- Name: email_blacklist email_blacklist_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.email_blacklist
    ADD CONSTRAINT email_blacklist_pkey PRIMARY KEY (uuid);


--
-- Name: finance_invoices finance_invoices_idempotency_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_invoices
    ADD CONSTRAINT finance_invoices_idempotency_key_key UNIQUE (idempotency_key);


--
-- Name: finance_invoices finance_invoices_payment_contract_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_invoices
    ADD CONSTRAINT finance_invoices_payment_contract_uk UNIQUE (id, account_uuid, amount_minor, currency);


--
-- Name: finance_invoices finance_invoices_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_invoices
    ADD CONSTRAINT finance_invoices_pkey PRIMARY KEY (id);


--
-- Name: finance_invoices finance_invoices_provider_external_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_invoices
    ADD CONSTRAINT finance_invoices_provider_external_uk UNIQUE (provider, provider_invoice_id);


--
-- Name: finance_operation_events finance_operation_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_operation_events
    ADD CONSTRAINT finance_operation_events_pkey PRIMARY KEY (id);


--
-- Name: finance_operations finance_operations_idempotency_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_operations
    ADD CONSTRAINT finance_operations_idempotency_key_key UNIQUE (idempotency_key);


--
-- Name: finance_operations finance_operations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_operations
    ADD CONSTRAINT finance_operations_pkey PRIMARY KEY (id);


--
-- Name: finance_operations finance_operations_provider_external_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_operations
    ADD CONSTRAINT finance_operations_provider_external_uk UNIQUE (provider, operation_type, provider_operation_id);


--
-- Name: finance_payments finance_payments_id_currency_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_payments
    ADD CONSTRAINT finance_payments_id_currency_uk UNIQUE (id, currency);


--
-- Name: finance_payments finance_payments_idempotency_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_payments
    ADD CONSTRAINT finance_payments_idempotency_key_key UNIQUE (idempotency_key);


--
-- Name: finance_payments finance_payments_invoice_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_payments
    ADD CONSTRAINT finance_payments_invoice_uk UNIQUE (invoice_id);


--
-- Name: finance_payments finance_payments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_payments
    ADD CONSTRAINT finance_payments_pkey PRIMARY KEY (id);


--
-- Name: finance_payments finance_payments_provider_external_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_payments
    ADD CONSTRAINT finance_payments_provider_external_uk UNIQUE (provider, provider_payment_id);


--
-- Name: finance_refunds finance_refunds_idempotency_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_refunds
    ADD CONSTRAINT finance_refunds_idempotency_key_key UNIQUE (idempotency_key);


--
-- Name: finance_refunds finance_refunds_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_refunds
    ADD CONSTRAINT finance_refunds_pkey PRIMARY KEY (id);


--
-- Name: finance_refunds finance_refunds_provider_external_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_refunds
    ADD CONSTRAINT finance_refunds_provider_external_uk UNIQUE (provider, provider_refund_id);


--
-- Name: homepage_video_settings homepage_video_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.homepage_video_settings
    ADD CONSTRAINT homepage_video_settings_pkey PRIMARY KEY (uuid);


--
-- Name: identities identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.identities
    ADD CONSTRAINT identities_pkey PRIMARY KEY (uuid);


--
-- Name: identities identities_provider_external_id_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.identities
    ADD CONSTRAINT identities_provider_external_id_uk UNIQUE (provider, external_id);


--
-- Name: mfa_recovery_codes mfa_recovery_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mfa_recovery_codes
    ADD CONSTRAINT mfa_recovery_codes_pkey PRIMARY KEY (id);


--
-- Name: node_health_snapshots node_health_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.node_health_snapshots
    ADD CONSTRAINT node_health_snapshots_pkey PRIMARY KEY (node_id);


--
-- Name: nodes nodes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_pkey PRIMARY KEY (uuid);


--
-- Name: oauth_exchange_codes oauth_exchange_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oauth_exchange_codes
    ADD CONSTRAINT oauth_exchange_codes_pkey PRIMARY KEY (code);


--
-- Name: overlay_config_acks overlay_config_acks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_config_acks
    ADD CONSTRAINT overlay_config_acks_pkey PRIMARY KEY (user_uuid, device_id);


--
-- Name: overlay_device_credentials overlay_device_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_device_credentials
    ADD CONSTRAINT overlay_device_credentials_pkey PRIMARY KEY (id);


--
-- Name: overlay_devices overlay_devices_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_devices
    ADD CONSTRAINT overlay_devices_pkey PRIMARY KEY (user_uuid, id);


--
-- Name: overlay_enrollment_sessions overlay_enrollment_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_enrollment_sessions
    ADD CONSTRAINT overlay_enrollment_sessions_pkey PRIMARY KEY (id);


--
-- Name: overlay_invites overlay_invites_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_invites
    ADD CONSTRAINT overlay_invites_pkey PRIMARY KEY (id);


--
-- Name: overlay_networks overlay_networks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_networks
    ADD CONSTRAINT overlay_networks_pkey PRIMARY KEY (id);


--
-- Name: overlay_nodes overlay_nodes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_nodes
    ADD CONSTRAINT overlay_nodes_pkey PRIMARY KEY (id);


--
-- Name: overlay_registrations overlay_registrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_registrations
    ADD CONSTRAINT overlay_registrations_pkey PRIMARY KEY (id);


--
-- Name: overlay_signed_config_acks overlay_signed_config_acks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_signed_config_acks
    ADD CONSTRAINT overlay_signed_config_acks_pkey PRIMARY KEY (id);


--
-- Name: password_recovery_challenges password_recovery_challenges_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.password_recovery_challenges
    ADD CONSTRAINT password_recovery_challenges_pkey PRIMARY KEY (id);


--
-- Name: rbac_permissions rbac_permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rbac_permissions
    ADD CONSTRAINT rbac_permissions_pkey PRIMARY KEY (permission_key);


--
-- Name: rbac_role_permissions rbac_role_permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rbac_role_permissions
    ADD CONSTRAINT rbac_role_permissions_pkey PRIMARY KEY (role_key, permission_key);


--
-- Name: rbac_roles rbac_roles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rbac_roles
    ADD CONSTRAINT rbac_roles_pkey PRIMARY KEY (role_key);


--
-- Name: sandbox_bindings sandbox_bindings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sandbox_bindings
    ADD CONSTRAINT sandbox_bindings_pkey PRIMARY KEY (id);


--
-- Name: scheduler_decisions scheduler_decisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduler_decisions
    ADD CONSTRAINT scheduler_decisions_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (uuid);


--
-- Name: stripe_webhook_events stripe_webhook_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.stripe_webhook_events
    ADD CONSTRAINT stripe_webhook_events_pkey PRIMARY KEY (event_id);


--
-- Name: subscriptions subscriptions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscriptions
    ADD CONSTRAINT subscriptions_pkey PRIMARY KEY (uuid);


--
-- Name: subscriptions subscriptions_user_external_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscriptions
    ADD CONSTRAINT subscriptions_user_external_uk UNIQUE (user_uuid, external_id);


--
-- Name: task_namespaces task_namespaces_account_uuid_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_namespaces
    ADD CONSTRAINT task_namespaces_account_uuid_slug_key UNIQUE (account_uuid, slug);


--
-- Name: task_namespaces task_namespaces_max_active_runs_mvp_ck; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.task_namespaces
    ADD CONSTRAINT task_namespaces_max_active_runs_mvp_ck CHECK (((max_active_runs > 0) AND (max_active_runs <= 2))) NOT VALID;


--
-- Name: task_namespaces task_namespaces_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_namespaces
    ADD CONSTRAINT task_namespaces_pkey PRIMARY KEY (id);


--
-- Name: task_runs task_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_runs
    ADD CONSTRAINT task_runs_pkey PRIMARY KEY (id);


--
-- Name: task_session_events task_session_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_session_events
    ADD CONSTRAINT task_session_events_pkey PRIMARY KEY (session_id, seq);


--
-- Name: task_sessions task_sessions_context_summary_size_ck; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.task_sessions
    ADD CONSTRAINT task_sessions_context_summary_size_ck CHECK ((octet_length((context_summary)::text) <= 131072)) NOT VALID;


--
-- Name: task_sessions task_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_sessions
    ADD CONSTRAINT task_sessions_pkey PRIMARY KEY (id);


--
-- Name: tenant_domains tenant_domains_domain_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_domains
    ADD CONSTRAINT tenant_domains_domain_key UNIQUE (domain);


--
-- Name: tenant_domains tenant_domains_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_domains
    ADD CONSTRAINT tenant_domains_pkey PRIMARY KEY (id);


--
-- Name: tenant_memberships tenant_memberships_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_memberships
    ADD CONSTRAINT tenant_memberships_pkey PRIMARY KEY (tenant_id, user_id);


--
-- Name: tenants tenants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenants
    ADD CONSTRAINT tenants_pkey PRIMARY KEY (id);


--
-- Name: traffic_minute_buckets traffic_minute_buckets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.traffic_minute_buckets
    ADD CONSTRAINT traffic_minute_buckets_pkey PRIMARY KEY (bucket_start, node_id, account_uuid, region, line_code);


--
-- Name: traffic_stat_checkpoints traffic_stat_checkpoints_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.traffic_stat_checkpoints
    ADD CONSTRAINT traffic_stat_checkpoints_pkey PRIMARY KEY (node_id, account_uuid);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (uuid);


--
-- Name: xworkmate_profiles xworkmate_profiles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.xworkmate_profiles
    ADD CONSTRAINT xworkmate_profiles_pkey PRIMARY KEY (id);


--
-- Name: account_lifecycle_events_actor_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX account_lifecycle_events_actor_time_idx ON public.account_lifecycle_events USING btree (actor_type, actor_ref, occurred_at DESC);


--
-- Name: account_lifecycle_events_user_request_uk; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX account_lifecycle_events_user_request_uk ON public.account_lifecycle_events USING btree (user_uuid, request_id) WHERE (request_id IS NOT NULL);


--
-- Name: account_lifecycle_events_user_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX account_lifecycle_events_user_time_idx ON public.account_lifecycle_events USING btree (user_uuid, occurred_at DESC);


--
-- Name: bridge_credentials_active_user_tenant_uk; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX bridge_credentials_active_user_tenant_uk ON public.bridge_credentials USING btree (user_uuid, tenant_id) WHERE (status = 'active'::text);


--
-- Name: bridge_credentials_user_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX bridge_credentials_user_tenant_idx ON public.bridge_credentials USING btree (user_uuid, tenant_id, status);


--
-- Name: finance_invoices_account_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX finance_invoices_account_created_idx ON public.finance_invoices USING btree (account_uuid, created_at DESC);


--
-- Name: finance_invoices_subscription_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX finance_invoices_subscription_created_idx ON public.finance_invoices USING btree (subscription_uuid, created_at DESC) WHERE (subscription_uuid IS NOT NULL);


--
-- Name: finance_operation_events_operation_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX finance_operation_events_operation_idx ON public.finance_operation_events USING btree (operation_id, id);


--
-- Name: finance_operations_reconcile_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX finance_operations_reconcile_idx ON public.finance_operations USING btree (status, next_attempt_at, updated_at) WHERE (status = ANY (ARRAY['pending'::text, 'failed'::text, 'reconcile_needed'::text]));


--
-- Name: finance_payments_account_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX finance_payments_account_created_idx ON public.finance_payments USING btree (account_uuid, created_at DESC);


--
-- Name: finance_payments_invoice_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX finance_payments_invoice_created_idx ON public.finance_payments USING btree (invoice_id, created_at DESC);


--
-- Name: finance_refunds_payment_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX finance_refunds_payment_created_idx ON public.finance_refunds USING btree (payment_id, created_at DESC);


--
-- Name: idx_admin_settings_module_role; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_admin_settings_module_role ON public.admin_settings USING btree (module_key, role);


--
-- Name: idx_admin_settings_version; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_admin_settings_version ON public.admin_settings USING btree (version);


--
-- Name: idx_audit_logs_action_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_audit_logs_action_created_at ON public.audit_logs USING btree (action, created_at DESC);


--
-- Name: idx_audit_logs_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_audit_logs_created_at ON public.audit_logs USING btree (created_at DESC);


--
-- Name: idx_billing_ledger_account_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_billing_ledger_account_created ON public.billing_ledger USING btree (account_uuid, created_at DESC);


--
-- Name: idx_homepage_video_domain_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_homepage_video_domain_key ON public.homepage_video_settings USING btree (domain_key);


--
-- Name: idx_identities_user_uuid; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_identities_user_uuid ON public.identities USING btree (user_uuid);


--
-- Name: idx_node_health_snapshots_sampled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_node_health_snapshots_sampled ON public.node_health_snapshots USING btree (sampled_at DESC);


--
-- Name: idx_nodes_available; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_nodes_available ON public.nodes USING btree (available);


--
-- Name: idx_oauth_exchange_codes_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_oauth_exchange_codes_expires_at ON public.oauth_exchange_codes USING btree (expires_at);


--
-- Name: idx_overlay_device_credentials_credential_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_overlay_device_credentials_credential_id ON public.overlay_device_credentials USING btree (credential_id);


--
-- Name: idx_overlay_device_credentials_device_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_device_credentials_device_id ON public.overlay_device_credentials USING btree (device_id);


--
-- Name: idx_overlay_device_credentials_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_device_credentials_expires_at ON public.overlay_device_credentials USING btree (expires_at);


--
-- Name: idx_overlay_device_credentials_token_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_overlay_device_credentials_token_hash ON public.overlay_device_credentials USING btree (token_hash);


--
-- Name: idx_overlay_devices_network; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_devices_network ON public.overlay_devices USING btree (network_id);


--
-- Name: idx_overlay_devices_network_address; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_overlay_devices_network_address ON public.overlay_devices USING btree (network_id, wireguard_address);


--
-- Name: idx_overlay_devices_network_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_devices_network_id ON public.overlay_devices USING btree (network_id);


--
-- Name: idx_overlay_devices_role; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_devices_role ON public.overlay_devices USING btree (role);


--
-- Name: idx_overlay_devices_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_devices_status ON public.overlay_devices USING btree (status);


--
-- Name: idx_overlay_devices_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_devices_user_id ON public.overlay_devices USING btree (user_id);


--
-- Name: idx_overlay_enrollment_sessions_device_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_enrollment_sessions_device_id ON public.overlay_enrollment_sessions USING btree (device_id);


--
-- Name: idx_overlay_enrollment_sessions_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_enrollment_sessions_expires_at ON public.overlay_enrollment_sessions USING btree (expires_at);


--
-- Name: idx_overlay_enrollment_sessions_token_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_overlay_enrollment_sessions_token_hash ON public.overlay_enrollment_sessions USING btree (token_hash);


--
-- Name: idx_overlay_invites_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_invites_expires_at ON public.overlay_invites USING btree (expires_at);


--
-- Name: idx_overlay_invites_network_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_invites_network_id ON public.overlay_invites USING btree (network_id);


--
-- Name: idx_overlay_invites_token_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_overlay_invites_token_hash ON public.overlay_invites USING btree (token_hash);


--
-- Name: idx_overlay_networks_owner_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_networks_owner_user_id ON public.overlay_networks USING btree (owner_user_id);


--
-- Name: idx_overlay_nodes_network; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_nodes_network ON public.overlay_nodes USING btree (network_id);


--
-- Name: idx_overlay_registrations_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_registrations_created_at ON public.overlay_registrations USING btree (created_at);


--
-- Name: idx_overlay_registrations_device_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_registrations_device_id ON public.overlay_registrations USING btree (device_id);


--
-- Name: idx_overlay_registrations_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_registrations_expires_at ON public.overlay_registrations USING btree (expires_at);


--
-- Name: idx_overlay_registrations_network_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_registrations_network_id ON public.overlay_registrations USING btree (network_id);


--
-- Name: idx_overlay_registrations_owner_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_registrations_owner_user_id ON public.overlay_registrations USING btree (owner_user_id);


--
-- Name: idx_overlay_registrations_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_registrations_status ON public.overlay_registrations USING btree (status);


--
-- Name: idx_overlay_registrations_token_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_overlay_registrations_token_hash ON public.overlay_registrations USING btree (token_hash);


--
-- Name: idx_overlay_registrations_wire_guard_public_key_fingerprint; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_registrations_wire_guard_public_key_fingerprint ON public.overlay_registrations USING btree (wireguard_public_key_fingerprint);


--
-- Name: idx_overlay_signed_config_acks_device_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_signed_config_acks_device_id ON public.overlay_signed_config_acks USING btree (device_id);


--
-- Name: idx_overlay_signed_config_acks_network_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_overlay_signed_config_acks_network_id ON public.overlay_signed_config_acks USING btree (network_id);


--
-- Name: idx_sandbox_bindings_agent_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_sandbox_bindings_agent_id ON public.sandbox_bindings USING btree (agent_id);


--
-- Name: idx_scheduler_decisions_generated; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_scheduler_decisions_generated ON public.scheduler_decisions USING btree (generated_at DESC);


--
-- Name: idx_sessions_user_uuid; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sessions_user_uuid ON public.sessions USING btree (user_uuid);


--
-- Name: idx_subscriptions_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_subscriptions_status ON public.subscriptions USING btree (status);


--
-- Name: idx_subscriptions_user_uuid; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_subscriptions_user_uuid ON public.subscriptions USING btree (user_uuid);


--
-- Name: idx_task_namespaces_personal; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_task_namespaces_personal ON public.task_namespaces USING btree (account_uuid) WHERE (slug = 'personal'::text);


--
-- Name: idx_task_runs_claimable; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_runs_claimable ON public.task_runs USING btree (account_uuid, state, not_before, priority DESC, created_at);


--
-- Name: idx_task_runs_client_request; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_task_runs_client_request ON public.task_runs USING btree (session_id, client_request_id) WHERE (client_request_id <> ''::text);


--
-- Name: idx_task_runs_namespace_state; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_runs_namespace_state ON public.task_runs USING btree (namespace_id, state, updated_at DESC);


--
-- Name: idx_task_session_events_client_request; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_task_session_events_client_request ON public.task_session_events USING btree (session_id, client_request_id) WHERE (client_request_id <> ''::text);


--
-- Name: idx_task_session_events_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_session_events_created ON public.task_session_events USING btree (session_id, created_at DESC);


--
-- Name: idx_task_sessions_namespace_updated; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_task_sessions_namespace_updated ON public.task_sessions USING btree (namespace_id, updated_at DESC);


--
-- Name: idx_tenant_domains_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tenant_domains_status ON public.tenant_domains USING btree (status);


--
-- Name: idx_tenant_domains_tenant_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tenant_domains_tenant_id ON public.tenant_domains USING btree (tenant_id);


--
-- Name: idx_tenant_memberships_role; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tenant_memberships_role ON public.tenant_memberships USING btree (role);


--
-- Name: idx_tenants_edition; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tenants_edition ON public.tenants USING btree (edition);


--
-- Name: idx_traffic_minute_buckets_account_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_traffic_minute_buckets_account_bucket ON public.traffic_minute_buckets USING btree (account_uuid, bucket_start DESC);


--
-- Name: idx_xworkmate_profiles_scope; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_xworkmate_profiles_scope ON public.xworkmate_profiles USING btree (tenant_id, user_id, scope);


--
-- Name: mfa_recovery_codes_active_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX mfa_recovery_codes_active_user_idx ON public.mfa_recovery_codes USING btree (user_uuid, batch_uuid, created_at) WHERE ((consumed_at IS NULL) AND (revoked_at IS NULL));


--
-- Name: mfa_recovery_codes_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX mfa_recovery_codes_expiry_idx ON public.mfa_recovery_codes USING btree (expires_at) WHERE ((consumed_at IS NULL) AND (revoked_at IS NULL));


--
-- Name: overlay_registrations_identity_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX overlay_registrations_identity_pending_idx ON public.overlay_registrations USING btree (network_id, device_id, wireguard_public_key_fingerprint, status, expires_at);


--
-- Name: overlay_registrations_network_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX overlay_registrations_network_created_idx ON public.overlay_registrations USING btree (network_id, created_at);


--
-- Name: overlay_registrations_network_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX overlay_registrations_network_pending_idx ON public.overlay_registrations USING btree (network_id, status, expires_at);


--
-- Name: overlay_registrations_owner_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX overlay_registrations_owner_created_idx ON public.overlay_registrations USING btree (owner_user_id, created_at DESC);


--
-- Name: password_recovery_active_user_kind_uk; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX password_recovery_active_user_kind_uk ON public.password_recovery_challenges USING btree (user_uuid, challenge_kind) WHERE ((consumed_at IS NULL) AND (invalidated_at IS NULL));


--
-- Name: password_recovery_code_email_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX password_recovery_code_email_created_idx ON public.password_recovery_challenges USING btree (lower(email_snapshot), created_at DESC) WHERE ((challenge_kind = 'code'::text) AND (consumed_at IS NULL) AND (invalidated_at IS NULL));


--
-- Name: password_recovery_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX password_recovery_expiry_idx ON public.password_recovery_challenges USING btree (expires_at) WHERE ((consumed_at IS NULL) AND (invalidated_at IS NULL));


--
-- Name: sessions_token_uk; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX sessions_token_uk ON public.sessions USING btree (token);


--
-- Name: stripe_webhook_events_received_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX stripe_webhook_events_received_at_idx ON public.stripe_webhook_events USING btree (received_at DESC);


--
-- Name: users_account_lifecycle_worklist_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX users_account_lifecycle_worklist_idx ON public.users USING btree (account_lifecycle_state, account_lifecycle_changed_at, uuid) WHERE (account_lifecycle_state <> 'active'::text);


--
-- Name: users_email_lower_uk; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX users_email_lower_uk ON public.users USING btree (lower(email)) WHERE (email IS NOT NULL);


--
-- Name: users_username_lower_uk; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX users_username_lower_uk ON public.users USING btree (lower(username));


--
-- Name: account_lifecycle_events account_lifecycle_events_immutable_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER account_lifecycle_events_immutable_trg BEFORE DELETE OR UPDATE ON public.account_lifecycle_events FOR EACH ROW EXECUTE FUNCTION public.reject_account_lifecycle_event_mutation();


--
-- Name: account_lifecycle_events account_lifecycle_events_no_truncate_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER account_lifecycle_events_no_truncate_trg BEFORE TRUNCATE ON public.account_lifecycle_events FOR EACH STATEMENT EXECUTE FUNCTION public.reject_account_lifecycle_event_mutation();


--
-- Name: finance_invoices finance_invoice_subscription_owner; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_invoice_subscription_owner BEFORE INSERT ON public.finance_invoices FOR EACH ROW EXECUTE FUNCTION public.validate_finance_invoice_subscription();


--
-- Name: finance_invoices finance_invoices_append_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_invoices_append_only BEFORE DELETE OR UPDATE ON public.finance_invoices FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: finance_invoices finance_invoices_no_truncate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_invoices_no_truncate BEFORE TRUNCATE ON public.finance_invoices FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: finance_operation_events finance_operation_events_append_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_operation_events_append_only BEFORE DELETE OR UPDATE ON public.finance_operation_events FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: finance_operation_events finance_operation_events_no_truncate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_operation_events_no_truncate BEFORE TRUNCATE ON public.finance_operation_events FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: finance_operations finance_operations_no_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_operations_no_delete BEFORE DELETE ON public.finance_operations FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: finance_operations finance_operations_no_truncate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_operations_no_truncate BEFORE TRUNCATE ON public.finance_operations FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: finance_payments finance_payments_append_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_payments_append_only BEFORE DELETE OR UPDATE ON public.finance_payments FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: finance_payments finance_payments_no_truncate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_payments_no_truncate BEFORE TRUNCATE ON public.finance_payments FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: finance_refunds finance_refunds_append_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_refunds_append_only BEFORE DELETE OR UPDATE ON public.finance_refunds FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: finance_refunds finance_refunds_no_truncate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER finance_refunds_no_truncate BEFORE TRUNCATE ON public.finance_refunds FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();


--
-- Name: admin_settings trg_admin_settings_bump_version; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_admin_settings_bump_version BEFORE UPDATE ON public.admin_settings FOR EACH ROW EXECUTE FUNCTION public.bump_version();


--
-- Name: admin_settings trg_admin_settings_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_admin_settings_set_updated_at BEFORE UPDATE ON public.admin_settings FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: agents trg_agents_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_agents_set_updated_at BEFORE UPDATE ON public.agents FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: identities trg_identities_bump_version; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_identities_bump_version BEFORE UPDATE ON public.identities FOR EACH ROW EXECUTE FUNCTION public.bump_version();


--
-- Name: identities trg_identities_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_identities_set_updated_at BEFORE UPDATE ON public.identities FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: nodes trg_nodes_bump_version; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_nodes_bump_version BEFORE UPDATE ON public.nodes FOR EACH ROW EXECUTE FUNCTION public.bump_version();


--
-- Name: nodes trg_nodes_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_nodes_set_updated_at BEFORE UPDATE ON public.nodes FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: rbac_permissions trg_rbac_permissions_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_rbac_permissions_set_updated_at BEFORE UPDATE ON public.rbac_permissions FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: rbac_role_permissions trg_rbac_role_permissions_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_rbac_role_permissions_set_updated_at BEFORE UPDATE ON public.rbac_role_permissions FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: rbac_roles trg_rbac_roles_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_rbac_roles_set_updated_at BEFORE UPDATE ON public.rbac_roles FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: sessions trg_sessions_bump_version; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_sessions_bump_version BEFORE UPDATE ON public.sessions FOR EACH ROW EXECUTE FUNCTION public.bump_version();


--
-- Name: sessions trg_sessions_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_sessions_set_updated_at BEFORE UPDATE ON public.sessions FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: subscriptions trg_subscriptions_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_subscriptions_set_updated_at BEFORE UPDATE ON public.subscriptions FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: users trg_users_bump_version; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_users_bump_version BEFORE UPDATE ON public.users FOR EACH ROW EXECUTE FUNCTION public.bump_version();


--
-- Name: users trg_users_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_users_set_updated_at BEFORE UPDATE ON public.users FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


--
-- Name: users users_no_delete_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER users_no_delete_trg BEFORE DELETE ON public.users FOR EACH ROW EXECUTE FUNCTION public.reject_user_hard_delete();


--
-- Name: users users_no_truncate_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER users_no_truncate_trg BEFORE TRUNCATE ON public.users FOR EACH STATEMENT EXECUTE FUNCTION public.reject_user_hard_delete();


--
-- Name: account_billing_profiles account_billing_profiles_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_billing_profiles
    ADD CONSTRAINT account_billing_profiles_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: account_lifecycle_events account_lifecycle_events_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_lifecycle_events
    ADD CONSTRAINT account_lifecycle_events_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: account_policy_snapshots account_policy_snapshots_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_policy_snapshots
    ADD CONSTRAINT account_policy_snapshots_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: account_quota_states account_quota_states_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_quota_states
    ADD CONSTRAINT account_quota_states_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: billing_ledger billing_ledger_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_ledger
    ADD CONSTRAINT billing_ledger_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: bridge_credentials bridge_credentials_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bridge_credentials
    ADD CONSTRAINT bridge_credentials_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: bridge_credentials bridge_credentials_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bridge_credentials
    ADD CONSTRAINT bridge_credentials_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: finance_invoices finance_invoices_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_invoices
    ADD CONSTRAINT finance_invoices_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: finance_invoices finance_invoices_subscription_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_invoices
    ADD CONSTRAINT finance_invoices_subscription_uuid_fkey FOREIGN KEY (subscription_uuid) REFERENCES public.subscriptions(uuid) ON DELETE RESTRICT;


--
-- Name: finance_operation_events finance_operation_events_operation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_operation_events
    ADD CONSTRAINT finance_operation_events_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.finance_operations(id) ON DELETE RESTRICT;


--
-- Name: finance_payments finance_payments_invoice_contract_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_payments
    ADD CONSTRAINT finance_payments_invoice_contract_fk FOREIGN KEY (invoice_id, account_uuid, amount_minor, currency) REFERENCES public.finance_invoices(id, account_uuid, amount_minor, currency) ON DELETE RESTRICT;


--
-- Name: finance_refunds finance_refunds_payment_currency_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_refunds
    ADD CONSTRAINT finance_refunds_payment_currency_fk FOREIGN KEY (payment_id, currency) REFERENCES public.finance_payments(id, currency) ON DELETE RESTRICT;


--
-- Name: finance_refunds finance_refunds_payment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.finance_refunds
    ADD CONSTRAINT finance_refunds_payment_id_fkey FOREIGN KEY (payment_id) REFERENCES public.finance_payments(id) ON DELETE RESTRICT;


--
-- Name: identities identities_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.identities
    ADD CONSTRAINT identities_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: mfa_recovery_codes mfa_recovery_codes_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mfa_recovery_codes
    ADD CONSTRAINT mfa_recovery_codes_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: overlay_config_acks overlay_config_acks_user_uuid_device_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_config_acks
    ADD CONSTRAINT overlay_config_acks_user_uuid_device_id_fkey FOREIGN KEY (user_uuid, device_id) REFERENCES public.overlay_devices(user_uuid, id) ON DELETE CASCADE;


--
-- Name: overlay_config_acks overlay_config_acks_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_config_acks
    ADD CONSTRAINT overlay_config_acks_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: overlay_devices overlay_devices_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.overlay_devices
    ADD CONSTRAINT overlay_devices_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: password_recovery_challenges password_recovery_challenges_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.password_recovery_challenges
    ADD CONSTRAINT password_recovery_challenges_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: rbac_role_permissions rbac_role_permissions_permission_key_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rbac_role_permissions
    ADD CONSTRAINT rbac_role_permissions_permission_key_fkey FOREIGN KEY (permission_key) REFERENCES public.rbac_permissions(permission_key) ON DELETE CASCADE;


--
-- Name: rbac_role_permissions rbac_role_permissions_role_key_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rbac_role_permissions
    ADD CONSTRAINT rbac_role_permissions_role_key_fkey FOREIGN KEY (role_key) REFERENCES public.rbac_roles(role_key) ON DELETE CASCADE;


--
-- Name: scheduler_decisions scheduler_decisions_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduler_decisions
    ADD CONSTRAINT scheduler_decisions_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: sessions sessions_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: subscriptions subscriptions_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscriptions
    ADD CONSTRAINT subscriptions_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: task_namespaces task_namespaces_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_namespaces
    ADD CONSTRAINT task_namespaces_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: task_runs task_runs_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_runs
    ADD CONSTRAINT task_runs_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: task_runs task_runs_namespace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_runs
    ADD CONSTRAINT task_runs_namespace_id_fkey FOREIGN KEY (namespace_id) REFERENCES public.task_namespaces(id) ON DELETE CASCADE;


--
-- Name: task_runs task_runs_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_runs
    ADD CONSTRAINT task_runs_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.task_sessions(id) ON DELETE CASCADE;


--
-- Name: task_session_events task_session_events_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_session_events
    ADD CONSTRAINT task_session_events_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.task_sessions(id) ON DELETE CASCADE;


--
-- Name: task_sessions task_sessions_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_sessions
    ADD CONSTRAINT task_sessions_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: task_sessions task_sessions_namespace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.task_sessions
    ADD CONSTRAINT task_sessions_namespace_id_fkey FOREIGN KEY (namespace_id) REFERENCES public.task_namespaces(id) ON DELETE CASCADE;


--
-- Name: tenant_domains tenant_domains_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_domains
    ADD CONSTRAINT tenant_domains_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: tenant_memberships tenant_memberships_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_memberships
    ADD CONSTRAINT tenant_memberships_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: traffic_minute_buckets traffic_minute_buckets_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.traffic_minute_buckets
    ADD CONSTRAINT traffic_minute_buckets_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: traffic_stat_checkpoints traffic_stat_checkpoints_account_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.traffic_stat_checkpoints
    ADD CONSTRAINT traffic_stat_checkpoints_account_uuid_fkey FOREIGN KEY (account_uuid) REFERENCES public.users(uuid) ON DELETE CASCADE;


--
-- Name: account_lifecycle_events; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.account_lifecycle_events ENABLE ROW LEVEL SECURITY;

--
-- Name: finance_invoices; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.finance_invoices ENABLE ROW LEVEL SECURITY;

--
-- Name: finance_operation_events; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.finance_operation_events ENABLE ROW LEVEL SECURITY;

--
-- Name: finance_operations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.finance_operations ENABLE ROW LEVEL SECURITY;

--
-- Name: finance_payments; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.finance_payments ENABLE ROW LEVEL SECURITY;

--
-- Name: finance_refunds; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.finance_refunds ENABLE ROW LEVEL SECURITY;

--
-- Name: mfa_recovery_codes; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.mfa_recovery_codes ENABLE ROW LEVEL SECURITY;

--
-- Name: overlay_config_acks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.overlay_config_acks ENABLE ROW LEVEL SECURITY;

--
-- Name: overlay_nodes; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.overlay_nodes ENABLE ROW LEVEL SECURITY;

--
-- Name: password_recovery_challenges; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.password_recovery_challenges ENABLE ROW LEVEL SECURITY;

--
-- PostgreSQL database dump complete
--


