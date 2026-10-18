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
                    self.assertEqual(len(receipt['prices']), 9)
                    self.assertEqual(output.stat().st_mode & 0o777, 0o600)
        finally:
            server.shutdown(); server.server_close(); worker.join()

    def test_test_catalog_receipt(self): self.run_sync()
    def test_conflicting_currency_stops_mapping(self): self.run_sync(mismatch=True)


if __name__ == '__main__': unittest.main()
