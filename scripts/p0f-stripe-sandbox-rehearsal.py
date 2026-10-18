#!/usr/bin/env python3
"""Run real test-mode Stripe payments against one declared V3 test account.

Requires a prepared V3 checkpoint. Credentials are runtime environment values;
the JSON state file stores only test object identities and nonsecret evidence.
Historical finance facts and the dedicated test account are retained.
"""
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

import yaml


def quote(value):
    return "'" + str(value).replace("'", "''") + "'"


def main():
    manifest = yaml.safe_load(Path(os.environ["P0F_MANIFEST_FILE"]).read_text())
    target, stripe = manifest["target"], manifest["stripe"]
    expected = "https://accounts-selfhost-prod-v3.svc.plus/api/billing/stripe/webhook"
    if target["generation"] != "prod-v3" or stripe["mode"] != "test" or stripe["webhook_url"] != expected:
        raise RuntimeError("only the declared V3 Sandbox endpoint is supported")
    key, signing = os.environ["STRIPE_SECRET_KEY"], os.environ["STRIPE_WEBHOOK_SECRET"]
    if not key.startswith("sk_test_") or not signing.startswith("whsec_"):
        raise RuntimeError("Sandbox credentials required")
    cmdb = json.loads(Path(os.environ["P0F_CMDB_FILE"]).read_text())
    host = cmdb[target["console_domain"]]
    if host["name"] != "web-saas-prod-v3":
        raise RuntimeError("CMDB target mismatch")
    ssh = ["ssh", "-i", os.environ["P0F_SSH_KEY_FILE"], "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes",
           "-o", "UserKnownHostsFile=" + os.environ["P0F_KNOWN_HOSTS_FILE"],
           "-o", "HostKeyAlias=" + os.environ["P0F_HOST_KEY_ALIAS"],
           os.environ["P0F_SSH_USER"] + "@" + host["ip"]]
    actual = subprocess.run(ssh + ["hostname -s"], capture_output=True, text=True, check=True).stdout.strip()
    if actual != "web-saas-prod-v3":
        raise RuntimeError("remote host mismatch")

    def sql(query):
        command = "sudo docker exec -i web-saas-postgresql psql -U postgres -d account -X -qAt -v ON_ERROR_STOP=1"
        result = subprocess.run(ssh + [command], input=query, capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError("V3 fixture/query failed; inspect the private target checkpoint")
        return result.stdout.strip()

    release, prepare_run = os.environ["P0F_RELEASE_TAG"], os.environ["P0F_PREPARE_RUN_ID"]
    if not re.fullmatch(r"prod-v3-[A-Za-z0-9._-]+", release) or not re.fullmatch(r"[A-Za-z0-9_-]+", prepare_run):
        raise RuntimeError("invalid checkpoint identity")
    receipt_path = "/data/backups/web-saas/prod-v3/" + release + "/" + prepare_run + "/prepared.json"
    receipt = json.loads(subprocess.run(ssh + ["sudo cat " + receipt_path], capture_output=True, text=True, check=True).stdout)
    if receipt.get("restore_verified") is not True or receipt.get("dirty") is not False or receipt.get("webhook_url") != expected:
        raise RuntimeError("successful V3 checkpoint restoration evidence is required")

    state_path = Path(os.environ["P0F_STATE_FILE"])
    state_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(state_path.parent, 0o700)
    if state_path.exists():
        raise RuntimeError("state exists; preserve it and inspect before a new rehearsal")
    run = "p0f-v3-" + time.strftime("%Y%m%d-%H%M%S", time.gmtime())
    account_id, proxy_id = str(uuid.uuid4()), str(uuid.uuid4())
    state = {"run": run, "account_uuid": account_id, "webhook_url": expected, "schema": manifest["database"]["expected_schema_version"]}

    def save():
        with state_path.open("w", opener=lambda path, flags: os.open(path, flags, 0o600)) as output:
            json.dump(state, output, sort_keys=True, indent=2)

    save()

    def api(method, path, form=None, operation=""):
        data = None if form is None else urllib.parse.urlencode(form).encode()
        headers = {"Authorization": "Bearer " + key, "Stripe-Version": stripe["api_version"]}
        if operation:
            headers["Idempotency-Key"] = run + ":" + operation
        request = urllib.request.Request("https://api.stripe.com/v1" + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                result = json.load(response)
        except urllib.error.HTTPError as error:
            raise RuntimeError("Stripe Sandbox request failed: HTTP " + str(error.code)) from None
        if result.get("livemode") is True:
            raise RuntimeError("live object rejected")
        return result

    baseline = sql("SELECT json_build_object('users', (SELECT md5(string_agg(row_to_json(t)::text, '' ORDER BY uuid)) FROM users t), 'subscriptions', (SELECT md5(string_agg(row_to_json(t)::text, '' ORDER BY uuid)) FROM subscriptions t), 'usage', (SELECT md5(string_agg(row_to_json(t)::text, '' ORDER BY id)) FROM billing_ledger t), 'users_count',(SELECT count(*) FROM users), 'schema',(SELECT version FROM schema_migrations WHERE NOT dirty));")
    state["baseline"] = json.loads(baseline)
    if state["baseline"]["users_count"] != 32 or state["baseline"]["schema"] != state["schema"]:
        raise RuntimeError("V3 baseline changed; review before testing")
    save()
    product = api("POST", "/products", {"name": run, "metadata[environment]": "prod-v3", "metadata[purpose]": "p0f-sandbox"}, "product")
    state["product"] = product["id"]; save()
    price = api("POST", "/prices", {"product": product["id"], "currency": "usd", "unit_amount": 100, "recurring[interval]": "month"}, "price")
    state["price"] = price["id"]; save()
    sql("BEGIN; INSERT INTO users(uuid,username,password,email,proxy_uuid,email_verified_at) VALUES (" + ",".join(map(quote, [account_id, run, "!disabled-sandbox-fixture", run + "@example.invalid", proxy_id])) + ",now()); INSERT INTO billing_plans(plan_id,stripe_price_id,display_name,kind,included_quota_bytes,package_name,price_amount,price_currency,price_unit,active) VALUES (" + ",".join(map(quote, [run, price["id"], run, "subscription"])) + ",21474836480,'plus',100,'usd','month',true); COMMIT;")
    print("dedicated V3 fixture and Sandbox price prepared", flush=True)
    customer = api("POST", "/customers", {"email": run + "@example.invalid", "payment_method": "pm_card_visa", "invoice_settings[default_payment_method]": "pm_card_visa", "metadata[environment]": "prod-v3"}, "customer")
    state["customer"] = customer["id"]; save()
    subscription = api("POST", "/subscriptions", {"customer": customer["id"], "items[0][price]": price["id"], "metadata[user_id]": account_id, "metadata[plan_id]": run, "metadata[product_slug]": "p0f-sandbox"}, "subscription")
    state["subscription"] = subscription["id"]; state["invoice"] = subscription["latest_invoice"]; save()
    invoice = api("GET", "/invoices/" + state["invoice"] + "?expand[]=payments")
    if invoice["status"] != "paid" or invoice["amount_paid"] != 100:
        raise RuntimeError("Sandbox invoice is not paid")
    state["payment_intent"] = invoice["payments"]["data"][0]["payment"]["payment_intent"]; save()

    def facts():
        return json.loads(sql("SELECT json_build_object('invoices',(SELECT count(*) FROM finance_invoices WHERE account_uuid=" + quote(account_id) + "),'payments',(SELECT count(*) FROM finance_payments WHERE account_uuid=" + quote(account_id) + "),'refunded',COALESCE((SELECT sum(r.amount_minor) FROM finance_refunds r JOIN finance_payments p ON p.id=r.payment_id WHERE p.account_uuid=" + quote(account_id) + "),0),'status',(SELECT status FROM subscriptions WHERE external_id=" + quote(state["subscription"]) + "));"))

    def wait_for(predicate, description):
        for _ in range(45):
            result = facts()
            if predicate(result):
                print(description + ": passed", flush=True)
                return result
            time.sleep(4)
        state["last_facts"] = facts(); save()
        raise RuntimeError(description + " timed out; checkpoint retained")

    wait_for(lambda f: f["invoices"] == 1 and f["payments"] == 1 and f["status"] == "active", "Stripe delivery and local invoice/payment")
    event = next(e for e in api("GET", "/events?type=invoice.paid&limit=100")["data"] if e["data"]["object"]["id"] == state["invoice"])
    body = json.dumps(event, separators=(",", ":")).encode()
    stamp = str(int(time.time()))
    signature = hmac.new(signing.encode(), stamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    request = urllib.request.Request(expected, data=body, headers={"Content-Type": "application/json", "Stripe-Signature": "t=" + stamp + ",v1=" + signature})
    with urllib.request.urlopen(request, timeout=30) as response:
        if json.load(response).get("duplicate") is not True:
            raise RuntimeError("provider event replay was not deduplicated")
    state["duplicate_event"] = True; save()
    for amount, label in [(40, "partial"), (60, "remaining")]:
        refund = api("POST", "/refunds", {"payment_intent": state["payment_intent"], "amount": amount}, "refund-" + label)
        state["refund_" + label] = refund["id"]; save()
        if label == "partial":
            wait_for(lambda f: f["refunded"] == 40 and f["status"] == "active", "partial refund preserves subscription")
        else:
            wait_for(lambda f: f["refunded"] == 100 and f["status"] == "refunded", "full refund local ledger and revocation")
    api("DELETE", "/subscriptions/" + state["subscription"], operation="cancel")
    wait_for(lambda f: f["status"] == "canceled", "Sandbox recurring subscription cancellation")
    after = json.loads(sql("SELECT json_build_object('users',(SELECT md5(string_agg(row_to_json(t)::text, '' ORDER BY uuid)) FROM users t WHERE uuid<>" + quote(account_id) + "),'subscriptions',(SELECT md5(string_agg(row_to_json(t)::text, '' ORDER BY uuid)) FROM subscriptions t WHERE user_uuid<>" + quote(account_id) + "),'usage',(SELECT md5(string_agg(row_to_json(t)::text, '' ORDER BY id)) FROM billing_ledger t WHERE account_uuid<>" + quote(account_id) + "));"))
    state["original_data_preserved"] = all(after[k] == state["baseline"][k] for k in after)
    state["final_facts"] = facts(); save()
    if not state["original_data_preserved"]:
        raise RuntimeError("original V3 data changed; inspect checkpoint")
    print("original users/subscriptions/usage fingerprints preserved; Sandbox rehearsal passed", flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Do not print HTTP bodies, request headers, DSNs or credentials.
        print("P0-F rehearsal stopped: " + (str(error) if isinstance(error, RuntimeError) else type(error).__name__), file=sys.stderr)
        sys.exit(1)
