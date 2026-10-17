\set ON_ERROR_STOP on
-- Run only in an empty disposable database initialized from accounts-native.sql.
INSERT INTO public.users(uuid,username,email,password,proxy_uuid,role,groups,permissions,active,created_at,updated_at,version)
SELECT ('00000000-0000-4000-8000-' || lpad(i::text,12,'0'))::uuid,
 'quota-fixture-'||i, 'quota-fixture-'||i||'@example.invalid','unused',gen_random_uuid(),
 CASE WHEN i=5 THEN 'root' ELSE 'user' END,
 CASE WHEN i=4 THEN '["segment:quota:plus-20gb"]'::jsonb
 WHEN i=7 THEN '["segment:quota:free-3gb","segment:subscription:unlimited"]'::jsonb
 ELSE '["segment:quota:free-3gb"]'::jsonb END,
 '[]'::jsonb,true,now(),now(),0
FROM generate_series(1,9) i;
INSERT INTO public.network_teams(id,name,member_limit,valid_from,valid_until,active)
VALUES ('quota-fixture-team','fixture',10,now()-interval '1 day',now()+interval '1 day',true);
INSERT INTO public.network_team_members(user_uuid,team_id,dedicated_only)
VALUES ('00000000-0000-4000-8000-000000000006','quota-fixture-team',true);
INSERT INTO public.account_billing_profiles(account_uuid,included_quota_bytes,pricing_rule_version)
SELECT uuid,3221225472,CASE WHEN username='quota-fixture-4' THEN 'plan:PLUS' ELSE 'plan:FREE' END
FROM public.users WHERE username <> 'quota-fixture-3';
INSERT INTO public.account_quota_states(account_uuid,remaining_included_quota,period_start,period_end,proxy_access_state,current_balance)
SELECT uuid,0,date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
 (date_trunc('month',now() AT TIME ZONE 'UTC')+interval '1 month') AT TIME ZONE 'UTC','paused',12.5
FROM public.users WHERE username <> 'quota-fixture-3';
UPDATE public.account_quota_states SET period_start=NULL,period_end=NULL WHERE account_uuid='00000000-0000-4000-8000-000000000008';
UPDATE public.account_quota_states SET period_start=period_start-interval '1 month',period_end=period_end-interval '1 month' WHERE account_uuid='00000000-0000-4000-8000-000000000009';
INSERT INTO public.traffic_minute_buckets(account_uuid,node_id,bucket_start,total_bytes)
VALUES ('00000000-0000-4000-8000-000000000002','fixture',now(),6442450944);
INSERT INTO public.billing_plans(plan_id,display_name,kind,package_name,included_quota_bytes,features)
VALUES ('FREE','Free','subscription','free',3221225472,'{}'::jsonb);
\ir ../2026101701_free_quota_5gb.up.sql
DO $$ BEGIN
 IF (SELECT included_quota_bytes FROM billing_plans WHERE plan_id='FREE') <> 5368709120
 OR (SELECT features->>'quota_cycle' FROM billing_plans WHERE plan_id='FREE') <> 'natural_month'
 THEN RAISE EXCEPTION 'Free catalog not upgraded'; END IF;
 IF (SELECT remaining_included_quota FROM account_quota_states WHERE account_uuid='00000000-0000-4000-8000-000000000008') <> 2147483648 THEN RAISE EXCEPTION 'lost usage with missing period bounds'; END IF;
 IF (SELECT remaining_included_quota FROM account_quota_states WHERE account_uuid='00000000-0000-4000-8000-000000000009') <> 5368709120 THEN RAISE EXCEPTION 'old month usage carried forward'; END IF;
 IF (SELECT remaining_included_quota FROM account_quota_states WHERE account_uuid='00000000-0000-4000-8000-000000000001') <> 2147483648 THEN RAISE EXCEPTION 'lost previously consumed 3 GiB'; END IF;
 IF (SELECT remaining_included_quota FROM account_quota_states WHERE account_uuid='00000000-0000-4000-8000-000000000002') <> 0 THEN RAISE EXCEPTION 'over 5 GiB must stay exhausted'; END IF;
 IF (SELECT remaining_included_quota FROM account_quota_states WHERE account_uuid='00000000-0000-4000-8000-000000000003') <> 5368709120 THEN RAISE EXCEPTION 'missing Free grant not provisioned'; END IF;
 IF EXISTS (SELECT 1 FROM account_billing_profiles WHERE account_uuid::text IN ('00000000-0000-4000-8000-000000000004','00000000-0000-4000-8000-000000000005','00000000-0000-4000-8000-000000000006','00000000-0000-4000-8000-000000000007') AND included_quota_bytes <> 3221225472) THEN RAISE EXCEPTION 'changed exempt account'; END IF;
 IF EXISTS (SELECT 1 FROM account_quota_states WHERE account_uuid <> '00000000-0000-4000-8000-000000000003' AND (proxy_access_state <> 'paused' OR current_balance <> 12.5)) THEN RAISE EXCEPTION 'changed operator pause or balance'; END IF;
END $$;
\ir ../2026101701_free_quota_5gb.up.sql
DO $$ BEGIN
 IF (SELECT remaining_included_quota FROM account_quota_states WHERE account_uuid='00000000-0000-4000-8000-000000000001') <> 2147483648 THEN RAISE EXCEPTION 'replay granted twice'; END IF;
END $$;
