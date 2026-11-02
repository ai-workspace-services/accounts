-- Target-only compatibility for the reviewed Accounts 42-table scope.
-- Native artifact sha256: 80ca865a9b93f1e63ca4c4c76e7a08e69458245ec63f1bba5667e28c4409a619.
-- This is not an application migration checkpoint and does not create one.
-- Execute only through an owner-reviewed, backup-verified target operation.
BEGIN;
SET LOCAL lock_timeout='10s';
SET LOCAL statement_timeout='5min';
DO $guard$
DECLARE name text; tables text;
BEGIN
 IF current_setting('accounts_migration_guard.system_identifier',true) IS DISTINCT FROM (SELECT system_identifier::text FROM pg_control_system())
 OR current_setting('accounts_migration_guard.database_oid',true) IS DISTINCT FROM (SELECT oid::text FROM pg_database WHERE datname=current_database()) THEN
   RAISE EXCEPTION 'target physical identity not bound';
 END IF;
 IF current_setting('server_version_num')::int < 170000 OR current_setting('server_version_num')::int >= 180000 THEN
   RAISE EXCEPTION 'PostgreSQL 17 required';
 END IF;
 IF to_regclass('public.schema_migrations') IS NOT NULL THEN
   RAISE EXCEPTION 'existing migration checkpoint requires separate review';
 END IF;
 FOREACH name IN ARRAY ARRAY['account_billing_profiles','account_lifecycle_events','account_policy_snapshots','account_quota_states','audit_logs','billing_ledger','billing_plans','billing_source_sync_state','rbac_permissions','rbac_role_permissions','rbac_roles','stripe_webhook_events','subscription_lifecycle_states','subscription_records','subscriptions','tenant_domains','tenant_memberships','tenants','traffic_minute_buckets','traffic_stat_checkpoints','users'] LOOP
   IF to_regclass('public.'||name) IS NULL THEN RAISE EXCEPTION 'required target table missing: %',name; END IF;
 END LOOP;
 FOREACH name IN ARRAY ARRAY['finance_invoices','finance_operation_events','finance_operations','finance_payments','finance_refunds','identities','mfa_recovery_codes','network_team_members','network_teams','referral_campaign_revisions','referral_codes','referral_payment_risks','referral_reward_campaign_snapshots','referral_reward_events','referral_rewards','referral_settlement_items','referral_settlements','referral_verified_invoices','referrals','subscription_extension_ledger','team_agent_enrollments'] LOOP
   IF to_regclass('public.'||name) IS NOT NULL THEN RAISE EXCEPTION 'target object already exists: %',name; END IF;
 END LOOP;
 FOREACH name IN ARRAY ARRAY['bump_version','reject_finance_fact_mutation','reject_referral_campaign_mutation','snapshot_referral_campaign','validate_finance_invoice_subscription'] LOOP
   IF EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.proname=name) THEN
      RAISE EXCEPTION 'target function already exists: %',name;
   END IF;
 END LOOP;
 IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='users' AND column_name='email' AND data_type='text' AND is_nullable='NO') THEN
   RAISE EXCEPTION 'users.email precondition differs';
 END IF;
 SELECT string_agg(format('public.%I',v),', ' ORDER BY v) INTO tables FROM unnest(ARRAY['account_billing_profiles','account_lifecycle_events','account_policy_snapshots','account_quota_states','audit_logs','billing_ledger','billing_plans','billing_source_sync_state','rbac_permissions','rbac_role_permissions','rbac_roles','stripe_webhook_events','subscription_lifecycle_states','subscription_records','subscriptions','tenant_domains','tenant_memberships','tenants','traffic_minute_buckets','traffic_stat_checkpoints','users']) v;
 EXECUTE 'LOCK TABLE '||tables||' IN SHARE ROW EXCLUSIVE MODE';
END
$guard$;

CREATE FUNCTION public.bump_version()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    NEW.version := COALESCE(OLD.version, 0) + 1;
  END IF;
  RETURN NEW;
END;
$function$
;
CREATE FUNCTION public.reject_finance_fact_mutation()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN
  IF TG_TABLE_NAME = 'finance_invoices'
     AND TG_OP = 'UPDATE'
     AND OLD.service_period_start IS NULL
     AND OLD.service_period_end IS NULL
     AND NEW.service_period_start IS NOT NULL
     AND NEW.service_period_end IS NOT NULL
     AND (to_jsonb(NEW) - 'service_period_start' - 'service_period_end') = (to_jsonb(OLD) - 'service_period_start' - 'service_period_end') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION '% is append-only; % is not permitted', TG_TABLE_NAME, TG_OP;
  RETURN OLD;
END;
$function$
;
CREATE FUNCTION public.reject_referral_campaign_mutation()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN RAISE EXCEPTION '% is append-only; % is not permitted',TG_TABLE_NAME,TG_OP; END $function$
;
CREATE FUNCTION public.snapshot_referral_campaign()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
DECLARE selected_revision BIGINT;
BEGIN
 SELECT revision INTO STRICT selected_revision FROM public.referral_campaign_revisions
 WHERE effective_at<=NEW.paid_at ORDER BY effective_at DESC,revision DESC LIMIT 1;
 INSERT INTO public.referral_reward_campaign_snapshots(reward_id,campaign_revision) VALUES(NEW.id,selected_revision);
 RETURN NEW;
END $function$
;
CREATE FUNCTION public.validate_finance_invoice_subscription()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN
  IF NEW.subscription_uuid IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM public.subscriptions s
    WHERE s.uuid = NEW.subscription_uuid AND s.user_uuid = NEW.account_uuid
  ) THEN
    RAISE EXCEPTION 'subscription % does not belong to account %', NEW.subscription_uuid, NEW.account_uuid;
  END IF;
  RETURN NEW;
END;
$function$
;

--
-- PostgreSQL database dump
--


-- Dumped from database version 17.11 (Homebrew)
-- Dumped by pg_dump version 17.11 (Homebrew)




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
    service_period_start timestamp with time zone,
    service_period_end timestamp with time zone,
    CONSTRAINT finance_invoices_amount_minor_check CHECK ((amount_minor >= 0)),
    CONSTRAINT finance_invoices_currency_check CHECK ((currency ~ '^[A-Z]{3}$'::text)),
    CONSTRAINT finance_invoices_service_period_ck CHECK ((((service_period_start IS NULL) = (service_period_end IS NULL)) AND ((service_period_start IS NULL) OR (service_period_start < service_period_end))))
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
-- Name: TABLE mfa_recovery_codes; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.mfa_recovery_codes IS 'One-time MFA recovery code hashes; plaintext values are returned only on issuance.';


--
-- Name: network_team_members; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.network_team_members (
    user_uuid uuid NOT NULL,
    team_id text,
    dedicated_only boolean DEFAULT true NOT NULL,
    CONSTRAINT network_team_members_check CHECK (((team_id IS NULL) OR dedicated_only))
);


--
-- Name: network_teams; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.network_teams (
    id text NOT NULL,
    name text NOT NULL,
    member_limit integer NOT NULL,
    valid_from timestamp with time zone NOT NULL,
    valid_until timestamp with time zone NOT NULL,
    active boolean DEFAULT false NOT NULL,
    CONSTRAINT network_teams_check CHECK ((valid_until > valid_from)),
    CONSTRAINT network_teams_member_limit_check CHECK ((member_limit > 0))
);


--
-- Name: referral_campaign_revisions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referral_campaign_revisions (
    revision bigint NOT NULL,
    enabled boolean NOT NULL,
    starts_at timestamp with time zone,
    ends_at timestamp with time zone,
    effective_at timestamp with time zone NOT NULL,
    actor_uuid uuid,
    reason text NOT NULL,
    cashback_bps integer DEFAULT 1000 NOT NULL,
    annual_bonus_days integer DEFAULT 30 NOT NULL,
    CONSTRAINT referral_campaign_revisions_annual_bonus_days_check CHECK ((annual_bonus_days = 30)),
    CONSTRAINT referral_campaign_revisions_cashback_bps_check CHECK ((cashback_bps = 1000)),
    CONSTRAINT referral_campaign_revisions_check CHECK (((starts_at IS NULL) OR (ends_at IS NULL) OR (starts_at < ends_at))),
    CONSTRAINT referral_campaign_revisions_reason_check CHECK (((length(btrim(reason)) >= 1) AND (length(btrim(reason)) <= 500)))
);


--
-- Name: referral_campaign_revisions_revision_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.referral_campaign_revisions_revision_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: referral_campaign_revisions_revision_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.referral_campaign_revisions_revision_seq OWNED BY public.referral_campaign_revisions.revision;


--
-- Name: referral_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referral_codes (
    user_uuid uuid NOT NULL,
    code text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT referral_codes_code_check CHECK ((code ~ '^[A-Z0-9]{8,16}$'::text))
);


--
-- Name: referral_payment_risks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referral_payment_risks (
    source_key text NOT NULL,
    provider_invoice_id text NOT NULL,
    risk_type text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT referral_payment_risks_risk_type_check CHECK ((risk_type = ANY (ARRAY['refund'::text, 'dispute'::text])))
);


--
-- Name: referral_reward_campaign_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referral_reward_campaign_snapshots (
    reward_id uuid NOT NULL,
    campaign_revision bigint NOT NULL
);


--
-- Name: referral_reward_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referral_reward_events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    reward_id uuid NOT NULL,
    source_key text NOT NULL,
    event_type text NOT NULL,
    amount_delta_minor bigint NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    actor_uuid uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT referral_reward_events_amount_delta_minor_check CHECK ((amount_delta_minor <> 0)),
    CONSTRAINT referral_reward_events_event_type_check CHECK ((event_type = ANY (ARRAY['award'::text, 'reversal'::text])))
);


--
-- Name: referral_rewards; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referral_rewards (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    referral_id uuid NOT NULL,
    provider_invoice_id text NOT NULL,
    referrer_uuid uuid NOT NULL,
    invitee_uuid uuid NOT NULL,
    plan_id text NOT NULL,
    product_slug text NOT NULL,
    paid_minor bigint NOT NULL,
    reward_minor bigint NOT NULL,
    currency character(3) NOT NULL,
    paid_at timestamp with time zone NOT NULL,
    eligible_at timestamp with time zone NOT NULL,
    annual_days integer DEFAULT 0 NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT referral_rewards_annual_days_check CHECK ((annual_days = ANY (ARRAY[0, 30]))),
    CONSTRAINT referral_rewards_currency_check CHECK ((currency ~ '^[A-Z]{3}$'::text)),
    CONSTRAINT referral_rewards_paid_minor_check CHECK ((paid_minor > 0)),
    CONSTRAINT referral_rewards_product_slug_check CHECK ((product_slug = 'xconnect'::text)),
    CONSTRAINT referral_rewards_reward_minor_check CHECK ((reward_minor >= 0)),
    CONSTRAINT referral_rewards_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'eligible'::text, 'settled'::text, 'reversed'::text])))
);


--
-- Name: referral_settlement_items; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referral_settlement_items (
    settlement_id uuid NOT NULL,
    reward_id uuid NOT NULL,
    amount_minor bigint NOT NULL,
    CONSTRAINT referral_settlement_items_amount_minor_check CHECK ((amount_minor <> 0))
);


--
-- Name: referral_settlements; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referral_settlements (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    referrer_uuid uuid NOT NULL,
    currency character(3) NOT NULL,
    amount_minor bigint NOT NULL,
    status text DEFAULT 'pending_review'::text NOT NULL,
    payout_reference text DEFAULT ''::text NOT NULL,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    completed_by uuid,
    reason text DEFAULT ''::text NOT NULL,
    CONSTRAINT referral_settlements_amount_minor_check CHECK ((amount_minor > 0)),
    CONSTRAINT referral_settlements_currency_check CHECK ((currency ~ '^[A-Z]{3}$'::text)),
    CONSTRAINT referral_settlements_status_check CHECK ((status = ANY (ARRAY['pending_review'::text, 'paid'::text, 'rejected'::text])))
);


--
-- Name: referral_verified_invoices; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referral_verified_invoices (
    invoice_id uuid NOT NULL,
    provider_price_id text NOT NULL,
    plan_id text NOT NULL,
    product_slug text DEFAULT 'xconnect'::text NOT NULL,
    verified_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT referral_verified_invoices_plan_id_check CHECK ((plan_id = ANY (ARRAY['PRO-MONTHLY'::text, 'PRO-YEARLY'::text, 'PRO-MONTHLY-V2'::text, 'PRO-YEARLY-V2'::text]))),
    CONSTRAINT referral_verified_invoices_product_slug_check CHECK ((product_slug = 'xconnect'::text)),
    CONSTRAINT referral_verified_invoices_provider_price_id_check CHECK ((length(provider_price_id) > 0))
);


--
-- Name: referrals; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.referrals (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    referrer_uuid uuid NOT NULL,
    invitee_uuid uuid NOT NULL,
    referral_code text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT referrals_no_self_ck CHECK ((referrer_uuid <> invitee_uuid))
);


--
-- Name: subscription_extension_ledger; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.subscription_extension_ledger (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    reward_id uuid NOT NULL,
    user_uuid uuid NOT NULL,
    subscription_uuid uuid NOT NULL,
    days integer NOT NULL,
    effective_from timestamp with time zone NOT NULL,
    effective_until timestamp with time zone NOT NULL,
    status text DEFAULT 'applied'::text NOT NULL,
    reversal_source_key text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT subscription_extension_ledger_check CHECK ((effective_until > effective_from)),
    CONSTRAINT subscription_extension_ledger_days_check CHECK ((days = 30)),
    CONSTRAINT subscription_extension_ledger_status_check CHECK ((status = ANY (ARRAY['applied'::text, 'reversed'::text])))
);


--
-- Name: team_agent_enrollments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.team_agent_enrollments (
    agent_id text NOT NULL,
    suggested_team_id text DEFAULT ''::text NOT NULL,
    assigned_team_id text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    decided_by text DEFAULT ''::text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_agent_enrollments_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'rejected'::text, 'revoked'::text])))
);


--
-- Name: referral_campaign_revisions revision; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_campaign_revisions ALTER COLUMN revision SET DEFAULT nextval('public.referral_campaign_revisions_revision_seq'::regclass);


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
-- Name: network_team_members network_team_members_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.network_team_members
    ADD CONSTRAINT network_team_members_pkey PRIMARY KEY (user_uuid);


--
-- Name: network_teams network_teams_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.network_teams
    ADD CONSTRAINT network_teams_pkey PRIMARY KEY (id);


--
-- Name: referral_campaign_revisions referral_campaign_revisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_campaign_revisions
    ADD CONSTRAINT referral_campaign_revisions_pkey PRIMARY KEY (revision);


--
-- Name: referral_codes referral_codes_code_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_codes
    ADD CONSTRAINT referral_codes_code_key UNIQUE (code);


--
-- Name: referral_codes referral_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_codes
    ADD CONSTRAINT referral_codes_pkey PRIMARY KEY (user_uuid);


--
-- Name: referral_payment_risks referral_payment_risks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_payment_risks
    ADD CONSTRAINT referral_payment_risks_pkey PRIMARY KEY (source_key);


--
-- Name: referral_reward_campaign_snapshots referral_reward_campaign_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_reward_campaign_snapshots
    ADD CONSTRAINT referral_reward_campaign_snapshots_pkey PRIMARY KEY (reward_id);


--
-- Name: referral_reward_events referral_reward_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_reward_events
    ADD CONSTRAINT referral_reward_events_pkey PRIMARY KEY (id);


--
-- Name: referral_reward_events referral_reward_events_reward_type_uk; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_reward_events
    ADD CONSTRAINT referral_reward_events_reward_type_uk UNIQUE (reward_id, event_type);


--
-- Name: referral_reward_events referral_reward_events_source_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_reward_events
    ADD CONSTRAINT referral_reward_events_source_key_key UNIQUE (source_key);


--
-- Name: referral_rewards referral_rewards_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_rewards
    ADD CONSTRAINT referral_rewards_pkey PRIMARY KEY (id);


--
-- Name: referral_rewards referral_rewards_provider_invoice_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_rewards
    ADD CONSTRAINT referral_rewards_provider_invoice_id_key UNIQUE (provider_invoice_id);


--
-- Name: referral_rewards referral_rewards_referral_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_rewards
    ADD CONSTRAINT referral_rewards_referral_id_key UNIQUE (referral_id);


--
-- Name: referral_settlement_items referral_settlement_items_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_settlement_items
    ADD CONSTRAINT referral_settlement_items_pkey PRIMARY KEY (settlement_id, reward_id);


--
-- Name: referral_settlements referral_settlements_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_settlements
    ADD CONSTRAINT referral_settlements_pkey PRIMARY KEY (id);


--
-- Name: referral_verified_invoices referral_verified_invoices_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_verified_invoices
    ADD CONSTRAINT referral_verified_invoices_pkey PRIMARY KEY (invoice_id);


--
-- Name: referrals referrals_invitee_uuid_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referrals
    ADD CONSTRAINT referrals_invitee_uuid_key UNIQUE (invitee_uuid);


--
-- Name: referrals referrals_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referrals
    ADD CONSTRAINT referrals_pkey PRIMARY KEY (id);


--
-- Name: subscription_extension_ledger subscription_extension_ledger_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscription_extension_ledger
    ADD CONSTRAINT subscription_extension_ledger_pkey PRIMARY KEY (id);


--
-- Name: subscription_extension_ledger subscription_extension_ledger_reversal_source_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscription_extension_ledger
    ADD CONSTRAINT subscription_extension_ledger_reversal_source_key_key UNIQUE (reversal_source_key);


--
-- Name: subscription_extension_ledger subscription_extension_ledger_reward_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscription_extension_ledger
    ADD CONSTRAINT subscription_extension_ledger_reward_id_key UNIQUE (reward_id);


--
-- Name: team_agent_enrollments team_agent_enrollments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_agent_enrollments
    ADD CONSTRAINT team_agent_enrollments_pkey PRIMARY KEY (agent_id);


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
-- Name: idx_identities_user_uuid; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_identities_user_uuid ON public.identities USING btree (user_uuid);


--
-- Name: mfa_recovery_codes_active_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX mfa_recovery_codes_active_user_idx ON public.mfa_recovery_codes USING btree (user_uuid, batch_uuid, created_at) WHERE ((consumed_at IS NULL) AND (revoked_at IS NULL));


--
-- Name: mfa_recovery_codes_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX mfa_recovery_codes_expiry_idx ON public.mfa_recovery_codes USING btree (expires_at) WHERE ((consumed_at IS NULL) AND (revoked_at IS NULL));


--
-- Name: network_team_members_team_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX network_team_members_team_idx ON public.network_team_members USING btree (team_id);


--
-- Name: referral_campaign_effective_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX referral_campaign_effective_idx ON public.referral_campaign_revisions USING btree (effective_at DESC, revision DESC);


--
-- Name: referral_payment_risks_invoice_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX referral_payment_risks_invoice_idx ON public.referral_payment_risks USING btree (provider_invoice_id, occurred_at DESC);


--
-- Name: referral_rewards_referrer_eligible_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX referral_rewards_referrer_eligible_idx ON public.referral_rewards USING btree (referrer_uuid, currency, eligible_at) WHERE (status = ANY (ARRAY['pending'::text, 'eligible'::text]));


--
-- Name: referral_settlements_owner_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX referral_settlements_owner_idx ON public.referral_settlements USING btree (referrer_uuid, requested_at DESC);


--
-- Name: referral_settlements_payout_ref_uk; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX referral_settlements_payout_ref_uk ON public.referral_settlements USING btree (payout_reference) WHERE ((status = 'paid'::text) AND (payout_reference <> ''::text));


--
-- Name: referrals_referrer_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX referrals_referrer_created_idx ON public.referrals USING btree (referrer_uuid, created_at DESC);


--
-- Name: subscription_extension_owner_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX subscription_extension_owner_idx ON public.subscription_extension_ledger USING btree (user_uuid, created_at DESC);


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
-- Name: referral_campaign_revisions referral_campaign_revisions_append_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER referral_campaign_revisions_append_only BEFORE DELETE OR UPDATE ON public.referral_campaign_revisions FOR EACH ROW EXECUTE FUNCTION public.reject_referral_campaign_mutation();


--
-- Name: referral_reward_campaign_snapshots referral_campaign_snapshots_append_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER referral_campaign_snapshots_append_only BEFORE DELETE OR UPDATE ON public.referral_reward_campaign_snapshots FOR EACH ROW EXECUTE FUNCTION public.reject_referral_campaign_mutation();


--
-- Name: referral_rewards referral_reward_campaign_snapshot; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER referral_reward_campaign_snapshot AFTER INSERT ON public.referral_rewards FOR EACH ROW EXECUTE FUNCTION public.snapshot_referral_campaign();


--
-- Name: referral_verified_invoices referral_verified_invoices_append_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER referral_verified_invoices_append_only BEFORE DELETE OR UPDATE ON public.referral_verified_invoices FOR EACH ROW EXECUTE FUNCTION public.reject_referral_campaign_mutation();


--
-- Name: identities trg_identities_bump_version; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_identities_bump_version BEFORE UPDATE ON public.identities FOR EACH ROW EXECUTE FUNCTION public.bump_version();


--
-- Name: identities trg_identities_set_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_identities_set_updated_at BEFORE UPDATE ON public.identities FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();


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
-- Name: network_team_members network_team_members_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.network_team_members
    ADD CONSTRAINT network_team_members_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.network_teams(id) ON DELETE RESTRICT;


--
-- Name: network_team_members network_team_members_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.network_team_members
    ADD CONSTRAINT network_team_members_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: referral_campaign_revisions referral_campaign_revisions_actor_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_campaign_revisions
    ADD CONSTRAINT referral_campaign_revisions_actor_uuid_fkey FOREIGN KEY (actor_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: referral_codes referral_codes_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_codes
    ADD CONSTRAINT referral_codes_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: referral_reward_campaign_snapshots referral_reward_campaign_snapshots_campaign_revision_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_reward_campaign_snapshots
    ADD CONSTRAINT referral_reward_campaign_snapshots_campaign_revision_fkey FOREIGN KEY (campaign_revision) REFERENCES public.referral_campaign_revisions(revision) ON DELETE RESTRICT;


--
-- Name: referral_reward_campaign_snapshots referral_reward_campaign_snapshots_reward_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_reward_campaign_snapshots
    ADD CONSTRAINT referral_reward_campaign_snapshots_reward_id_fkey FOREIGN KEY (reward_id) REFERENCES public.referral_rewards(id) ON DELETE RESTRICT;


--
-- Name: referral_reward_events referral_reward_events_actor_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_reward_events
    ADD CONSTRAINT referral_reward_events_actor_uuid_fkey FOREIGN KEY (actor_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: referral_reward_events referral_reward_events_reward_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_reward_events
    ADD CONSTRAINT referral_reward_events_reward_id_fkey FOREIGN KEY (reward_id) REFERENCES public.referral_rewards(id) ON DELETE RESTRICT;


--
-- Name: referral_rewards referral_rewards_invitee_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_rewards
    ADD CONSTRAINT referral_rewards_invitee_uuid_fkey FOREIGN KEY (invitee_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: referral_rewards referral_rewards_referral_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_rewards
    ADD CONSTRAINT referral_rewards_referral_id_fkey FOREIGN KEY (referral_id) REFERENCES public.referrals(id) ON DELETE RESTRICT;


--
-- Name: referral_rewards referral_rewards_referrer_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_rewards
    ADD CONSTRAINT referral_rewards_referrer_uuid_fkey FOREIGN KEY (referrer_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: referral_settlement_items referral_settlement_items_reward_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_settlement_items
    ADD CONSTRAINT referral_settlement_items_reward_id_fkey FOREIGN KEY (reward_id) REFERENCES public.referral_rewards(id) ON DELETE RESTRICT;


--
-- Name: referral_settlement_items referral_settlement_items_settlement_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_settlement_items
    ADD CONSTRAINT referral_settlement_items_settlement_id_fkey FOREIGN KEY (settlement_id) REFERENCES public.referral_settlements(id) ON DELETE RESTRICT;


--
-- Name: referral_settlements referral_settlements_completed_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_settlements
    ADD CONSTRAINT referral_settlements_completed_by_fkey FOREIGN KEY (completed_by) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: referral_settlements referral_settlements_referrer_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_settlements
    ADD CONSTRAINT referral_settlements_referrer_uuid_fkey FOREIGN KEY (referrer_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: referral_verified_invoices referral_verified_invoices_invoice_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referral_verified_invoices
    ADD CONSTRAINT referral_verified_invoices_invoice_id_fkey FOREIGN KEY (invoice_id) REFERENCES public.finance_invoices(id) ON DELETE RESTRICT;


--
-- Name: referrals referrals_invitee_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referrals
    ADD CONSTRAINT referrals_invitee_uuid_fkey FOREIGN KEY (invitee_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: referrals referrals_referral_code_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referrals
    ADD CONSTRAINT referrals_referral_code_fkey FOREIGN KEY (referral_code) REFERENCES public.referral_codes(code) ON DELETE RESTRICT;


--
-- Name: referrals referrals_referrer_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.referrals
    ADD CONSTRAINT referrals_referrer_uuid_fkey FOREIGN KEY (referrer_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


--
-- Name: subscription_extension_ledger subscription_extension_ledger_reward_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscription_extension_ledger
    ADD CONSTRAINT subscription_extension_ledger_reward_id_fkey FOREIGN KEY (reward_id) REFERENCES public.referral_rewards(id) ON DELETE RESTRICT;


--
-- Name: subscription_extension_ledger subscription_extension_ledger_subscription_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscription_extension_ledger
    ADD CONSTRAINT subscription_extension_ledger_subscription_uuid_fkey FOREIGN KEY (subscription_uuid) REFERENCES public.subscriptions(uuid) ON DELETE RESTRICT;


--
-- Name: subscription_extension_ledger subscription_extension_ledger_user_uuid_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscription_extension_ledger
    ADD CONSTRAINT subscription_extension_ledger_user_uuid_fkey FOREIGN KEY (user_uuid) REFERENCES public.users(uuid) ON DELETE RESTRICT;


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
-- Name: network_team_members; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.network_team_members ENABLE ROW LEVEL SECURITY;

--
-- Name: network_teams; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.network_teams ENABLE ROW LEVEL SECURITY;

--
-- Name: team_agent_enrollments; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.team_agent_enrollments ENABLE ROW LEVEL SECURITY;

--
-- PostgreSQL database dump complete
--



ALTER TABLE public.users ALTER COLUMN email DROP NOT NULL;
ALTER TABLE public.subscription_lifecycle_states ADD CONSTRAINT subscription_lifecycle_states_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.finance_operations(id);
ALTER TABLE public.tenant_domains ADD CONSTRAINT tenant_domains_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
ALTER TABLE public.tenant_memberships ADD CONSTRAINT tenant_memberships_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;
DO $verify$
DECLARE name text;
BEGIN
 FOREACH name IN ARRAY ARRAY['users','identities','mfa_recovery_codes','account_lifecycle_events','subscriptions','subscription_records','subscription_lifecycle_states','billing_plans','account_billing_profiles','account_quota_states','account_policy_snapshots','billing_ledger','billing_source_sync_state','traffic_minute_buckets','traffic_stat_checkpoints','stripe_webhook_events','finance_invoices','finance_payments','finance_refunds','finance_operations','finance_operation_events','referral_verified_invoices','referral_campaign_revisions','referral_reward_campaign_snapshots','referral_codes','referrals','referral_rewards','referral_reward_events','referral_payment_risks','referral_settlements','referral_settlement_items','subscription_extension_ledger','network_teams','network_team_members','team_agent_enrollments','audit_logs','tenants','tenant_domains','tenant_memberships','rbac_roles','rbac_permissions','rbac_role_permissions'] LOOP
   IF to_regclass('public.'||name) IS NULL THEN RAISE EXCEPTION 'scope table missing after change: %',name; END IF;
 END LOOP;
 IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='users' AND column_name='email' AND data_type='text' AND is_nullable='YES') THEN
   RAISE EXCEPTION 'users.email postcondition differs';
 END IF;
END
$verify$;
COMMIT;
