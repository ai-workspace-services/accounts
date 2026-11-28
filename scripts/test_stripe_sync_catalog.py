import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from urllib.parse import parse_qs, urlsplit

import yaml


ROOT = Path(__file__).resolve().parent
CATALOG = yaml.safe_load((ROOT / 'stripe-catalog.yaml').read_text())
PRICES = {p['key']: dict(p, product=product['key']) for product in CATALOG['products'] for p in product['prices']}


class SyncTest(unittest.TestCase):
    def run_sync(self, mismatch=False):
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_args): pass
            def respond(self, body):
                self.send_response(200); self.end_headers()
                self.wfile.write(json.dumps(body).encode())
            def do_GET(self):
                if self.headers.get('Authorization') != 'Bearer sk_test_fixture':
                    self.send_error(401); return
                path = urlsplit(self.path)
                if path.path == '/v1/account': self.respond(dict(id='acct_fixture'))
                elif path.path.startswith('/v1/products/'): self.respond(dict(id=path.path.split('/')[-1]))
                elif path.path == '/v1/prices':
                    key = parse_qs(path.query)['lookup_keys[]'][0]
                    p = PRICES[key]
                    price = dict(id='price_' + key.replace('-', ''), product=p['product'], unit_amount=p['unit_amount'], currency=p['currency'])
                    if p.get('recurring_interval'): price['recurring'] = dict(interval=p['recurring_interval'])
                    if mismatch: price['currency'] = 'eur'
                    self.respond(dict(data=[price]))
                elif path.path == '/v1/webhook_endpoints': self.respond(dict(data=[dict(id='we_fixture', url='https://fixture.invalid/api/billing/stripe/webhook', enabled_events=CATALOG['webhook']['events'])]))
                else: self.send_error(404)
            def do_POST(self): self.respond(dict(id=self.path.split('/')[-1]))
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        worker = threading.Thread(target=server.serve_forever, daemon=True); worker.start()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                output = Path(tmp) / 'map.json'
                env = dict(os.environ, STRIPE_SECRET_KEY='sk_test_fixture', STRIPE_WEBHOOK_SECRET='whsec_fixture', STRIPE_WEBHOOK_URL='https://fixture.invalid/api/billing/stripe/webhook', STRIPE_API_BASE=f'http://127.0.0.1:{server.server_port}/v1')
                result = subprocess.run(['bash', str(ROOT / 'stripe-sync-catalog.sh'), '--env', 'prod-v3', '--domain-base', 'svc.plus', '--expected-account', 'acct_fixture', '--price-map-output', str(output)], env=env, capture_output=True, text=True, timeout=20)
                self.assertNotIn('sk_test_fixture', result.stdout + result.stderr)
                if mismatch:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse(output.exists())
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    receipt = json.loads(output.read_text())
                    self.assertEqual(receipt['stripe_mode'], 'test')
                    self.assertEqual(receipt['stripe_profile'], 'hk')
                    self.assertEqual(len(receipt['prices']), 6)
                    self.assertEqual(output.stat().st_mode & 0o777, 0o600)
        finally:
            server.shutdown(); server.server_close(); worker.join()

    def test_test_catalog_receipt(self): self.run_sync()
    def test_conflicting_currency_stops_mapping(self): self.run_sync(mismatch=True)

    def test_stripe_profile_filters_split_hk_plus_and_us_pro_plus(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                path = urlsplit(self.path)
                self.send_response(200)
                self.end_headers()
                if path.path == '/v1/account':
                    account_id = 'acct_us_fixture' if os.environ.get('EXPECT_US') else 'acct_fixture'
                    self.wfile.write(json.dumps({'id': 'acct_us_fixture' if self.headers.get('Authorization') == 'Bearer sk_test_us' else 'acct_hk_fixture'}).encode())
                elif path.path.startswith('/v1/products/'):
                    self.wfile.write(json.dumps({'id': path.path.split('/')[-1]}).encode())
                elif path.path == '/v1/prices':
                    key = parse_qs(path.query)['lookup_keys[]'][0]
                    p = PRICES[key]
                    price = dict(id='price_' + key.replace('-', ''), product=p['product'], unit_amount=p['unit_amount'], currency=p['currency'])
                    if p.get('recurring_interval'):
                        price['recurring'] = dict(interval=p['recurring_interval'])
                    self.wfile.write(json.dumps(dict(data=[price])).encode())
                elif path.path == '/v1/webhook_endpoints':
                    url = 'https://fixture.invalid/api/billing/stripe/webhook/' + ('us' if self.headers.get('Authorization') == 'Bearer sk_test_us' else 'hk')
                    self.wfile.write(json.dumps(dict(data=[dict(id='we_split', url=url, enabled_events=CATALOG['webhook']['events'], status='enabled')])).encode())
                else:
                    self.wfile.write(b'{"data":[]}')
            def do_POST(self):
                self.send_response(200); self.end_headers()
                self.wfile.write(json.dumps({'id': self.path.split('/')[-1]}).encode())
            def log_message(self, *args): pass

        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                hk_out = Path(tmp) / 'hk-map.json'
                hk_env = dict(os.environ, STRIPE_SECRET_KEY='sk_test_hk', STRIPE_WEBHOOK_SECRET='whsec_hk', STRIPE_WEBHOOK_URL='https://fixture.invalid/api/billing/stripe/webhook/hk', STRIPE_API_BASE=f'http://127.0.0.1:{server.server_port}/v1')
                hk_res = subprocess.run(['bash', str(ROOT / 'stripe-sync-catalog.sh'), '--env', 'prod-v3', '--domain-base', 'svc.plus', '--expected-account', 'acct_hk_fixture', '--stripe-profile', 'hk', '--price-map-output', str(hk_out)], env=hk_env, capture_output=True, text=True, timeout=20)
                self.assertEqual(hk_res.returncode, 0, hk_res.stderr)
                hk_receipt = json.loads(hk_out.read_text())
                self.assertEqual(hk_receipt['stripe_profile'], 'hk')
                self.assertEqual([p['plan_id'] for p in hk_receipt['prices']], ['PLUS', 'PLUS-MONTHLY-2026'])
                self.assertTrue(all(p['reconciliation_tag'] == 'FIN-HK-PLUS-APAC' for p in hk_receipt['prices']))

                us_out = Path(tmp) / 'us-map.json'
                us_env = dict(os.environ, STRIPE_SECRET_KEY='sk_test_us', STRIPE_WEBHOOK_SECRET='whsec_us', STRIPE_WEBHOOK_URL='https://fixture.invalid/api/billing/stripe/webhook/us', STRIPE_API_BASE=f'http://127.0.0.1:{server.server_port}/v1')
                us_res = subprocess.run(['bash', str(ROOT / 'stripe-sync-catalog.sh'), '--env', 'prod-v3', '--domain-base', 'svc.plus', '--expected-account', 'acct_us_fixture', '--stripe-profile', 'us', '--price-map-output', str(us_out)], env=us_env, capture_output=True, text=True, timeout=20)
                self.assertEqual(us_res.returncode, 0, us_res.stderr)
                us_receipt = json.loads(us_out.read_text())
                self.assertEqual(us_receipt['stripe_profile'], 'us')
                self.assertEqual([p['plan_id'] for p in us_receipt['prices']], ['PRO-MONTHLY-V2', 'PRO-YEARLY-V2', 'CUSTOM-NODE-MONTHLY-2026', 'CUSTOM-AI-CONSULT-2026'])
                self.assertTrue(all(p['reconciliation_tag'] == 'FIN-US-PRO-EDGE' for p in us_receipt['prices']))

                eu_out = Path(tmp) / 'eu-map.json'
                eu_res = subprocess.run(['bash', str(ROOT / 'stripe-sync-catalog.sh'), '--env', 'prod-v3', '--domain-base', 'svc.plus', '--expected-account', 'acct_hk_fixture', '--stripe-profile', 'eu', '--price-map-output', str(eu_out)], env=hk_env, capture_output=True, text=True, timeout=20)
                self.assertEqual(eu_res.returncode, 0, eu_res.stderr)
                eu_receipt = json.loads(eu_out.read_text())
                self.assertEqual(len(eu_receipt['prices']), 0)
        finally:
            server.shutdown(); server.server_close(); worker.join()


if __name__ == '__main__': unittest.main()
