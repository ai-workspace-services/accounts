# Overlay Config Contract

This document defines the `schema_version: 1` contract returned by
`GET /api/overlay/config`. The same payload is consumed by `overlayctl` and is
the contract Android/macOS clients should implement next.

## Request

```http
GET /api/overlay/config?device_id=<device-id>&network_id=xworkmate-private&node_id=xworkmate-bridge
Authorization: Bearer <account-token>
```

`device_id` is required and must already be registered through
`POST /api/overlay/devices/register`. `network_id` is optional when the device
is registered to the default network. `node_id` is optional; if omitted the
control plane prefers `OVERLAY_GATEWAY_ID`, defaulting to `xworkmate-bridge`.

## Response Shape

```json
{
  "schema_version": 1,
  "revision": "1764556800",
  "digest": "sha256-hex",
  "network": {
    "id": "xworkmate-private",
    "display_name": "XWorkmate Private",
    "cidr": "172.29.10.0/24"
  },
  "device": {
    "id": "shenlan-macos",
    "network_id": "xworkmate-private",
    "wireguard_address": "172.29.10.123/32"
  },
  "wireguard": {
    "interface": "xwg0",
    "address": "172.29.10.123/32",
    "mtu": 1280,
    "dns": ["172.29.10.1"],
    "private_key_ref": "local-keychain",
    "local_proxy_endpoint": "127.0.0.1:51830",
    "persistent_keepalive": 25,
    "peer_public_key": "gateway-wireguard-public-key",
    "peer_allowed_ips": ["172.29.10.0/24"],
    "peer_endpoint": "127.0.0.1:51830",
    "gateway_wireguard_ip": "172.29.10.1",
    "gateway_wireguard_cidr": "172.29.10.1/32"
  },
  "transport": {
    "runtime": "xray-core",
    "type": "vless-tls",
    "security": "tls",
    "server": "xworkmate-bridge.svc.plus",
    "port": 2443,
    "uuid": "11111111-1111-1111-1111-111111111111",
    "path": "",
    "mode": "",
    "flow": "",
    "packet_encoding": "xudp",
    "local_port": 51830
  },
  "nodes": []
}
```

## Field Rules

- `schema_version` is the compatibility gate. Clients must reject unsupported
  versions instead of guessing.
- `revision` changes when the account, device, or selected gateway node changes.
  Clients should persist it and send it back through `/api/overlay/config/ack`
  after successful local application.
- `digest` identifies the downloaded config revision. It is not a secret.
- `device.wireguard_address` and `wireguard.address` include CIDR suffixes and
  are safe to put into WireGuard interface configuration.
- `wireguard.gateway_wireguard_ip` is a host IP without CIDR. Use it as the UDP
  target for local Xray `dokodemo-door`.
- `wireguard.gateway_wireguard_cidr` is the gateway host route with `/32`.
- `wireguard.peer_endpoint` points to the local transport endpoint, not the
  public gateway. WireGuard sends UDP to local Xray.
- `transport.type` is currently fixed to `vless-tls`; `transport.security` is
  currently fixed to `tls`. Clients must reject other values until the contract
  explicitly adds another transport profile.
- `transport.port` and `transport.local_port` must be valid TCP/UDP port
  numbers in the `1..65535` range. `transport.local_port` is the local Xray UDP
  ingress used by WireGuard.
- `transport.uuid` is the gateway VLESS UUID synced from playbooks/Vault through
  `/api/internal/overlay/nodes/heartbeat`. It is not derived from the account
  proxy UUID.
- `transport.flow` is empty for the current plain VLESS/TLS gateway. Clients
  must omit `flow` from rendered Xray user config when this field is empty.
- `transport.packet_encoding` defaults to `xudp` and must be rendered as
  `packetEncoding` in Xray JSON.

## Desktop Runtime Mapping

`overlayctl render` maps the contract to two files:

- WireGuard config: `<state-dir>/<wireguard.interface>.conf`
- Xray config: `<state-dir>/xray-overlay.json`

WireGuard peer:

```ini
[Peer]
PublicKey = <wireguard.peer_public_key>
AllowedIPs = <wireguard.peer_allowed_ips>
Endpoint = <wireguard.peer_endpoint>
PersistentKeepalive = <wireguard.persistent_keepalive>
```

Xray local UDP ingress:

```json
{
  "listen": "127.0.0.1",
  "port": 51830,
  "protocol": "dokodemo-door",
  "settings": {
    "address": "172.29.10.1",
    "port": 51820,
    "network": "udp"
  }
}
```

Xray VLESS user:

```json
{
  "id": "<transport.uuid>",
  "encryption": "none",
  "packetEncoding": "xudp"
}
```

## Mobile Client Notes

Android and macOS clients should keep the same contract boundary:

- authenticate against `accounts.svc.plus`;
- register the local WireGuard public key as a device;
- sync `/api/overlay/config`;
- store the WireGuard private key in the platform keychain/keystore;
- render or apply the WireGuard interface from the `wireguard` block;
- run the VLESS/TLS transport from the `transport` block;
- acknowledge the applied revision through `/api/overlay/config/ack`.

Do not derive gateway transport UUIDs, gateway IPs, or peer allowed IPs locally.
Those are control-plane outputs.
