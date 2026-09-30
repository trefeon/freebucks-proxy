# Egress — upstream country verdict and the WARP workaround

Bundled automation (run on the VPS, proxy mode only):

- `scripts/setup-warp-egress.sh [--port 40000] [--relay-port 40001]` —
  installs warp-cli, registers, sets proxy mode, connects, starts the
  bridge relay, prints the exit IP and the exact knob line. Does NOT flip
  the proxy or restart anything.
- `scripts/revert-warp-egress.sh [--keep-warp]` — removes the knob,
  disconnects warp, stops the relay, restarts on direct egress.


Status: **live**. Proxy serves from vps-us with upstream egress via local
Cloudflare WARP (proxy mode) + `UPSTREAM_EGRESS_PROXY` override.
Verified 2026-09-30: admission through the WARP exit returns
`accessTier: full` (no country cap); direct VPS egress reads as
`anonymous_network` (hosting ASN) and loses Tier-1 full-access seats.

## The vendor fact

Upstream resolves country from the egress IP alone
(`backend/internal/registry/testdata/upstream/freebuff-models.ts` — no
client-settable country; a client-chosen country is what abusive clients
rotate). Reputation signals ride `ipPrivacySignals`
(`anonymous`/`vpn`/`proxy`/`relay`/`res_proxy`/`tor`/`hosting`/`service`;
see `common/src/types/freebuff-session.ts` in the vendor clone), and
`anonymous_network` caps trust + gates Tier-1 seats. The 0.2.1 vendor
release added explicit residential-proxy enforcement
(`isResidentialProxyOnly`, "We lose money on every VPN user…"), which is
when the VPS exit started failing.

Declarable signals (timezone header, direct dial, no proxy env) are already
maximally consistent — `transport.Proxy = nil` is deliberate
(`backend/internal/upstream/client.go`: upstream hard-blocks proxy/VPN/Tor
egress). None of them move an ASN verdict. Only the source IP does.

## Environment facts (vps-us, verified 2026-09-30)

- Upstream (`www.codebuff.com`) is IPv4-only → it sees the VPS IPv4
  (`192.154.111.198`, hosting ASN). The box IPv6 is irrelevant.
- `egress_region` (Cloudflare trace) says US — country is fine; the
  *privacy verdict* (`anonymous_network`) is the2014 block, not geo.
- WARP exit (verified live): `2a09:bac1:76e0:430::2c7:5d`, Cloudflare range,
  admitted with full tier. Reputation can shift — re-verify after any
  standing change (below).

## Setup (proxy mode only — never full-tunnel on a remote box)

Full-tunnel WARP grabs the default route and can lock out SSH/Tailscale.
Proxy mode changes no routes: only processes pointed at the SOCKS use it.

```sh
# install (Debian/Ubuntu)
curl -fsSL https://pkg.cloudflareclient.com/pubkey.gpg \
  | sudo gpg --yes --dearmor -o /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] \
  https://pkg.cloudflareclient.com/ $(lsb_release -cs) main" \
  | sudo tee /etc/apt/sources.list.d/cloudflare-client.list
sudo apt-get update -qq && sudo apt-get install -y -qq cloudflare-warp

# register + proxy mode (2026.x CLI verbs; older builds used
# `registration new` / `set-mode` / `set-proxy-port`)
sudo systemctl start warp-svc || (sudo nohup warp-svc >/tmp/warp-svc.log 2>&1 & sleep 3)
warp-cli --accept-tos registration new
warp-cli --accept-tos mode proxy
warp-cli --accept-tos proxy port 40000
warp-cli --accept-tos connect
```

Verify the exit (must answer; note the IP for the standing check):

```sh
curl -s --max-time 15 --socks5-hostname 127.0.0.1:40000 ifconfig.me
```

## Bridging host-localhost into the container

The proxy runs in Docker; container `127.0.0.1` is not the host's.
WARP's SOCKS binds host-localhost only, so relay it onto the bridge:

```sh
sudo apt-get install -y -qq socat
nohup socat TCP-LISTEN:40001,bind=0.0.0.0,reuseaddr,fork \
  TCP:127.0.0.1:40000 >/tmp/socat-warp.log 2>&1 &
GATEWAY=$(docker network inspect freebuff-proxy_default \
  --format '{{(index .IPAM.Config 0).Gateway}}')  # 172.18.0.1
curl -s --max-time 15 --socks5-hostname "$GATEWAY:40001" ifconfig.me
```

## Flipping the proxy (single admission, then read the verdict)

`UPSTREAM_EGRESS_PROXY` is restart-only (warned in dashboard/admin):

```sh
printf '%s\n' 'UPSTREAM_EGRESS_PROXY=socks5://172.18.0.1:40001' \
  >> ~/freebuff-proxy/.env   # use the actual $GATEWAY
cd ~/freebuff-proxy && docker compose restart && sleep 12
```

Fire exactly one chat turn, then read the admission verdict from the dump
(`DEBUG_DUMP=true` writes `~/freebuff-proxy/dump/session-*-admission.dump`):

- `[status 200]` + `"accessTier":"full"` → exit is clean. Keep it.
- `country_blocked` / `anonymous_network` / `proxy` signals → exit is
  flagged. Revert immediately (below).

H2 works through the override (fix/egress-h2-socks); if exchanges fail
with `malformed HTTP response`, the override build predates that fix.

## Revert (30 seconds, back to direct)

```sh
sed -i '/^UPSTREAM_EGRESS_PROXY=/d' ~/freebuff-proxy/.env
cd ~/freebuff-proxy && docker compose restart
```

## Standing watch

Re-check the dashboard standing after any of: WARP reconnect with a new
exit IP, `warp-svc` restart, vendor bump touching availability files, or
fresh caps appearing. If the verdict flips, the exit's reputation shifted —
revert to direct and re-evaluate (residential egress box, clean provider
IP with pre-checked ASN reputation, or capped VPS use).

## What does NOT work (tried or ruled out)

- Header/fingerprint/geo forging: the check is the TCP source IP of a TLS
  session — unspoofable — and forged signals add flags to the account.
- `http_proxy`/`https_proxy` env: the transport deliberately nils them.
- IPv6 egress: upstream is IPv4-only; the v6 address never reaches it.
- Retrying through a refusal: `admission_attempt_closed` re-presents the
  same persisted claim (see `docs/decisions/claim-rotate-closed.md`);
  churn wedges, rotation + fresh identity recovers.
