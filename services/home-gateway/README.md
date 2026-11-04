# The phones' way home (home gateway, #75)

Jarvis on the phone (`devices/android/phone`) reaches the house's services without a VPN, at home
and away alike:

```
phone (core: internal/hometunnel) ── TLS + client certificate ──▶ home gateway (Raspberry Pi, nginx stream)
   at home: the gateway in the LAN                                  ├─ rpc.jarvis  → the Echos' peer calls (primary first)
   away:    the VPS's public port ── WireGuard (existing) ──▶       ├─ mqtt.jarvis → the broker (the gateway's MQTT forward)
            (passed through unopened)                               └─ ha.jarvis   → Home Assistant (the gateway's HTTP forward)
```

- TLS ends at the gateway at home; the VPS sees only encrypted bytes.
- The gateway lets in client certificates of its own CA whose name is on `DEVICES`. Revoking a
  lost phone: take its name off `DEVICES`, `install.sh pi`.
- The service is chosen by the name the phone asks for (SNI); everything else is refused.
- The phone's core listens on local ports (47811 calls, 41883 MQTT, 48123 HA) and dials the gateway
  in the LAN first, then the VPS, keeping the one that worked first. Away from home the primary's
  calls go over the tunnel when no heartbeat shows it; an Echo that is not the primary passes such
  a call on to the primary (`X-Jarvis-Relay`, `core/internal/peer/rpc.go`), so the gateway needs no
  knowledge of who leads (until that core is on the Echos, the primary first in `ECHOS` takes them).

## Files

| File | What |
|---|---|
| `make_certs.sh <dir> <device>…` | The CA, the gateway's certificate (names `rpc.jarvis`, `mqtt.jarvis`, `ha.jarvis`), a client certificate per device. The CA's key stays in `<dir>` on the PC |
| `pi-jarvis-mobile.conf.tmpl` | nginx `stream` server on the gateway: mTLS, device list, routing by SNI |
| `vps-jarvis-mobile.conf.tmpl` | nginx `stream` on the VPS: the public port to the gateway over WireGuard |
| `install.sh [pi\|vps]` | Renders both from `gateway.env`, copies the certificates, `nginx -t`, reload; a failed check restores the old file. On the VPS it installs `libnginx-mod-stream` and adds a `stream` block to `nginx.conf` once |

The household's values live in `devices/echo/local/home-gateway/` (git-ignored): `gateway.env`
(`PORT`, `GATEWAY_LAN`, `GATEWAY_WG`, `VPS_HOST`, `ECHOS` with the primary first, `DEVICES`,
`PI_SSH`, `VPS_SSH`) and the certificates. The phone gets its certificate and `HOME_ENDPOINTS`
from `devices/android/tools/install_phone.sh --device <name>`.

## Set up

```sh
services/home-gateway/make_certs.sh devices/echo/local/home-gateway phone-<name>
services/home-gateway/install.sh            # the gateway and the VPS
devices/android/tools/install_phone.sh <serial> --device phone-<name>
```

Checked on 01.11.2026: from the PC over the VPS's public port to Home Assistant (200) and the
broker; without a certificate the gateway refuses. The VPS's provider needs no firewall rule for
the port (no packet filter in front of it then).
