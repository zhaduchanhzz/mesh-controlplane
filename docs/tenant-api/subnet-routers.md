# Subnet routers with the tenant API

This guide follows Tailscale's
["Subnet routers"](https://tailscale.com/docs/features/subnet-routers) step by
step and gives, for each step done in Tailscale's admin console, the call to
make against this control plane's `/api/v2` instead. There is no web console
yet (P12d): everything a tenant does is done with its **API key**, against its
**own tailnet** only. The client side (`tailscale set ...`, IP forwarding,
firewall) is unchanged.

The first part is the **API contract** these calls rely on; the second maps
the Tailscale guide onto it; the last lists the differences from Tailscale.

## 1. API contract

### 1.1 Base URL, credentials, tailnet

- Base URL: the cluster's public URL, e.g. `https://c1.example.net:8443`. The
  API is under `/api/v2`. `/api/v1` does not exist on a cluster.
- Credential: a tailnet API key (`hskey-api-...`) issued by the platform
  operator. Send it as `Authorization: Bearer <key>`, or as HTTP Basic with the
  key as user name and an empty password (what the Tailscale Go SDK, tscli
  and the Terraform provider send). OAuth clients and access tokens are not
  accepted on a cluster.
- `{tailnet}` in a path is `-` (the key's tailnet) or the key's tailnet ID.
  Any other value is `404 {"message":"tailnet not found"}`, the same answer as
  for a tailnet that does not exist.
- A device ID (`{id}`) is the decimal `id` of `GET /api/v2/tailnet/-/devices`.
  A device of another tailnet is `404`, as one that does not exist.

The examples use:

```shell
export API=https://c1.example.net:8443/api/v2
export KEY=hskey-api-...            # never commit or log it
auth() { printf 'Authorization: Bearer %s' "$KEY"; }
```

### 1.2 Errors

Every error has Tailscale's body: `{"message": "...", "status": <code>}`, with
`"data": [...]` for details of a refused value (validation errors, failing
policy tests). A `5xx` answer has a fixed message and never any detail (no
database, cluster or lease internals); the control plane logs the cause.

| Status | When |
|---|---|
| 400 | The value is refused: the policy does not parse, has an unknown field, a failing `tests` entry, or is empty; DNS settings break a rule of section 1.5; a split DNS body that is not an object |
| 401 | Missing, malformed, revoked or expired key (one body for all) |
| 403 | An operation the operator keeps: `PATCH .../settings`, `POST /device/{id}/authorized`, `POST /device/{id}/update-client`, OAuth clients |
| 404 | Another tailnet's ID or device; a tailnet this cluster does not serve |
| 412 | `If-Match` does not match the stored policy |
| 413 | The body is larger than allowed: a policy over `tuning.tenant_api.max_policy_bytes` (default 262144 bytes), a DNS write over 64 KiB. A declared `Content-Length` over the limit is answered before the body is sent or read, and the connection is closed |
| 422 | The JSON body has the wrong shape for a typed endpoint (e.g. `{"dns":"9.9.9.9"}`) |
| 429 | A rate limit; the response has `Retry-After: 1` |
| 500 | An internal error; nothing to fix in the request. Exception: a policy write answered `500` with an `ETag` header and the message "the policy was stored but is not in force yet ..." **was stored** (section 1.4) |
| 501 | DNS writes on a single-tailnet server (`headscale serve`), where DNS comes from the config file |
| 503 | `{"message":"temporarily unavailable, retry"}`: the process answering is a standby, or lost the tailnet (moved, or the cluster lease ended) while writing; nothing was stored. Retry |

### 1.3 Rate limits

- Every request: per key, per tailnet and per process
  (`tuning.tenant_api.key_*`, `tailnet_*`, `global_*`).
- Policy, DNS and device key writes in addition share one bucket per
  tailnet: `tuning.tenant_api.config_write_rate` writes per minute (default
  30), burst `config_write_burst` (default 10). It counts `POST .../acl`,
  `POST .../acl/validate`, every DNS write (`POST`/`PATCH`/`PUT` under
  `.../dns/`) and `POST /device/{id}/key`, whatever the key, refused or not.
  For the policy and DNS writes the order is: `{tailnet}` (another tailnet's
  ID is a 404 and spends nothing), the declared body size (413, spends
  nothing), then the bucket, all before the body is read. A device key
  write spends a token once the device is found to be the tailnet's own
  (another tailnet's device is a 404 and spends nothing). Reads do not
  count.

### 1.4 Policy file

| Call | Body | Answer |
|---|---|---|
| `GET /tailnet/{tailnet}/acl` | – | The stored policy. `Accept: application/hujson` gives the stored bytes (comments kept); otherwise standard JSON. Header `ETag: "<hex sha256 of the stored bytes>"` |
| `POST /tailnet/{tailnet}/acl` | The policy, HuJSON or JSON (`Content-Type: application/hujson` or `application/json`). Optional `If-Match` | `200`, the stored policy and its new `ETag` |
| `POST /tailnet/{tailnet}/acl/validate` | A policy | `200 {}` when it would be accepted; `200 {"message": "...", "data": [...]}` when it would be refused (Tailscale's form). Nothing is stored |

- **ETag / If-Match**: `If-Match` takes the `ETag` of a `GET` (quoted), `*`
  (any), or `"ts-default"` (only while no policy is stored). A mismatch is
  `412` and nothing changes. Without `If-Match` the write is unconditional.
  The check is made in the write's own database transaction, after the
  tailnet's ownership lock, so two writers holding the same ETag cannot both
  succeed, and a policy the operator stored in between is never overwritten
  (the write is `412`).
- **Validation before storing**: the policy is parsed with the tenant
  semantics and its `tests` are run against the tailnet's users and devices.
  Any failure is `400` (failing tests: `"message":"test(s) failed"` and one
  `data` entry per failing test) and the stored **and enforced** policy stay
  as they were. Validating, storing and putting in force are one step: the
  policy put in force is the one validated (its tests are not run again).
- **Refused means not stored**: an answer `4xx` or `503` stores nothing, and
  nothing refused is ever put in force later.
- **Stored but not in force yet** (rare: an internal failure after the
  policy was stored): the answer is `500` with the stored policy's `ETag`
  and the message "the policy was stored but is not in force yet: the
  previous policy stays enforced until the stored one is installed; GET
  returns the stored policy". The write is stored and audited; the previous
  policy stays enforced until the control plane installs the stored one (it
  retries within seconds). Do not resend with the old `ETag` (it is a `412`):
  `GET` the policy.
- **Empty means deny**: `{}` and `{"grants": []}` are valid and deny all
  traffic in the tailnet, at once (connected devices lose their peers). An
  empty body is `400`. A policy is **never** replaced by allow-all, whatever
  fails.
- **No stored policy** reads as `{"grants": []}` with a comment, which is what
  is enforced (deny), not Tailscale's allow-all default.
- A stored policy takes effect immediately for the tailnet's devices, and
  only for them.

### 1.5 DNS

Tailscale's DNS endpoints and JSON shapes (the Go SDK's `DNSResource`):

| Call | Body | Answer |
|---|---|---|
| `GET /tailnet/{tailnet}/dns/nameservers` | – | `{"dns": ["9.9.9.9"]}` |
| `POST /tailnet/{tailnet}/dns/nameservers` | `{"dns": ["9.9.9.9", "https://dns.example/dns-query"]}` (replaces the list; `[]` clears) | `{"dns": [...], "magicDNS": true}` |
| `GET /tailnet/{tailnet}/dns/searchpaths` | – | `{"searchPaths": ["corp.example"]}` |
| `POST /tailnet/{tailnet}/dns/searchpaths` | `{"searchPaths": ["corp.example"]}` (replaces) | `{"searchPaths": [...]}` |
| `GET /tailnet/{tailnet}/dns/split-dns` | – | `{"corp.example": ["192.168.77.53"]}` (`{}` when none) |
| `PATCH /tailnet/{tailnet}/dns/split-dns` | `{"corp.example": ["192.168.77.53"], "old.example": null}`: listed domains are set, `null` removes one, others stay | The whole split DNS map |
| `PUT /tailnet/{tailnet}/dns/split-dns` | The whole map (`{}` removes all) | The whole split DNS map |
| `GET /tailnet/{tailnet}/dns/preferences` | – | `{"magicDNS": true, "overrideLocalDNS": false}` |
| `POST /tailnet/{tailnet}/dns/preferences` | `{"magicDNS": true}`; optional `"overrideLocalDNS": true/false` (left as is when absent) | The preferences |

Rules (ADR 0008), checked before anything is stored (`400` otherwise, nothing
changes):

- A nameserver is an IP address or an `https://` DNS-over-HTTPS URL. The
  address (or the IP address host of a DoH URL) must be one a device can
  ask: **not** loopback (`127.0.0.0/8`, `::1`), link-local (`169.254.0.0/16`,
  `fe80::/10`), unspecified (`0.0.0.0`, `::`) or with a zone (`%eth0`).
  Private addresses (`10.0.0.0/8`, `192.168.0.0/16`, `fd00::/8`, ...) are
  fine, e.g. a DNS server behind a subnet router. Host names in DoH URLs are
  not resolved by the control plane.
- A split DNS domain or search path is a DNS name. A split DNS domain must be
  **outside the cluster's MagicDNS domain** (no `*.<cluster base domain>`,
  including your own `<slug>.<cluster base domain>`) and must not contain it
  (no parent of `<cluster base domain>`, such as its last labels).
- A search path is outside the cluster's MagicDNS domain, or at or below your
  own `<slug>.<cluster base domain>` (never the cluster's domain itself or
  another tailnet's).
- Limits (operator settings `tuning.tenant_dns.*`, defaults): 64 nameservers
  (global and split together), 64 search domains, 64 split DNS domains
  (`max_split_domains`; a domain mapped to `[]` counts), 1000 extra records,
  256 KiB.
- Settings stored before a rule or a lower limit applied keep working; the
  next write must follow the current rules and limits (for the whole
  settings, not only the part written).
- The base domain (`<slug>.<cluster base domain>`), certificates and extra
  records are not settable through these endpoints.
- Domains are stored lower case without a trailing dot. Defaults (nothing
  stored): MagicDNS on, no nameservers, no search paths, no split DNS.
- `overrideLocalDNS: false` (default): the global nameservers are given to
  devices as fallback resolvers; `true`: they replace the device's own DNS.

A stored change is sent to the tailnet's connected devices at once.

### 1.6 Devices, routes, keys (already in P08b)

| Call | Body | Answer |
|---|---|---|
| `GET /tailnet/{tailnet}/devices` | – | `{"devices": [{"id": "7", "name": ..., "hostname": ..., "tags": [...], "keyExpiryDisabled": ...}]}` |
| `GET /device/{id}/routes` | – | `{"advertisedRoutes": [...], "enabledRoutes": [...]}` |
| `POST /device/{id}/routes` | `{"routes": ["192.168.77.0/24"]}` (the routes to enable; replaces) | The same shape as GET |
| `POST /device/{id}/key` | `{"keyExpiryDisabled": true}` | `200 {}` |
| `POST /device/{id}/tags` | `{"tags": ["tag:router"]}` (tags the policy defines) | `200 {}` |
| `POST /tailnet/{tailnet}/keys` | `{"capabilities": {"devices": {"create": {"reusable": true, "ephemeral": false, "preauthorized": true, "tags": ["tag:router"]}}}, "expirySeconds": 86400}` | `{"id": "...", "key": "hskey-auth-...", ...}` (the key is shown once) |

`keyExpiryDisabled: true` also brings back a device whose key **has already
expired**, as on Tailscale: its stored expiry is kept but no longer counts.
Setting it back to `false` makes the stored expiry count again (an expired
device is then expired at once). It is a configuration write (section 1.3)
and is audited.

### 1.7 Audit

Every stored policy, DNS or device key write is logged by the control plane
with the tailnet ID, the operation, the API key's ID, the new revision (policy
and DNS) and the SHA-256 of what was stored; never the content. A device key
write also names the device and the value set; a policy stored but not in
force yet (section 1.4) is marked `not_in_force`.

## 2. The Tailscale guide, step by step

### Step 1: install the client on the router

Unchanged: install Tailscale on the router (Linux in the examples). Enrol it
with an auth key. To let the policy approve its routes (step 3), enrol it
with a **tagged** key; the tag must exist in the policy first (step 4 shows a
policy with `tagOwners`):

```shell
curl -sS -H "$(auth)" -H 'Content-Type: application/json' "$API/tailnet/-/keys" \
  -d '{"capabilities":{"devices":{"create":{"reusable":false,"preauthorized":true,"tags":["tag:router"]}}},"expirySeconds":3600}'
# on the router (the key comes from the answer's "key"; pass it on stdin, not in argv)
sudo tailscale up --login-server=https://c1.example.net:8443 --auth-key=file:/dev/stdin <router.key
```

### Step 2: connect as a subnet router

Unchanged on the device:

```shell
echo 'net.ipv4.ip_forward = 1' | sudo tee -a /etc/sysctl.d/99-tailscale.conf
echo 'net.ipv6.conf.all.forwarding = 1' | sudo tee -a /etc/sysctl.d/99-tailscale.conf
sudo sysctl -p /etc/sysctl.d/99-tailscale.conf
sudo tailscale set --advertise-routes=192.168.77.0/24,fd00:77::/64
```

### Step 3: enable the subnet routes ("admin console > Machines > Subnets")

Either let the policy approve them (`autoApprovers`, step 4; nothing to call)
or approve them with the API:

```shell
curl -sS -H "$(auth)" "$API/tailnet/-/devices"                 # find the router's "id"
curl -sS -H "$(auth)" "$API/device/7/routes"                   # advertisedRoutes / enabledRoutes
curl -sS -H "$(auth)" -H 'Content-Type: application/json' "$API/device/7/routes" \
  -d '{"routes":["192.168.77.0/24","fd00:77::/64"]}'
```

"Optionally disable key expiry" (recommended by Tailscale for routers):

```shell
curl -sS -H "$(auth)" -H 'Content-Type: application/json' "$API/device/7/key" -d '{"keyExpiryDisabled":true}'
```

### Step 4: add access rules ("admin console > Access controls")

Read the policy and its ETag, edit, write it back with `If-Match`:

```shell
curl -sS -D headers.txt -H "$(auth)" -H 'Accept: application/hujson' "$API/tailnet/-/acl" > policy.hujson
ETAG=$(grep -i '^etag:' headers.txt | cut -d' ' -f2 | tr -d '\r')
cat > policy.hujson <<'EOF'
{
  "tagOwners": {"tag:router": ["alice@"]},
  "grants": [
    {"src": ["autogroup:member"], "dst": ["*"], "ip": ["*"]},
    {"src": ["autogroup:member"], "dst": ["192.168.77.0/24", "fd00:77::/64"], "ip": ["*"]},
  ],
  "autoApprovers": {"routes": {"192.168.77.0/24": ["tag:router"], "fd00:77::/64": ["tag:router"]}},
  "tests": [{"src": "alice@", "accept": ["192.168.77.2:7777"]}],
}
EOF
curl -sS -H "$(auth)" -H 'Content-Type: application/json' "$API/tailnet/-/acl/validate" --data-binary @policy.hujson
curl -sS -H "$(auth)" -H 'Content-Type: application/hujson' -H "If-Match: $ETAG" "$API/tailnet/-/acl" --data-binary @policy.hujson
```

A `412` means someone changed the policy since your `GET`: read it again. A
`400` means the policy was refused (a failing test, a parse error) and the
previous policy is still in force.

With `autoApprovers`, a router enrolled with a `tag:router` key has its
advertised routes approved as soon as it advertises them (step 3 is then
nothing to call).

### Step 5: verify the connection

Unchanged: `tailscale ip -4` on the router, `tailscale ping` from another
device. With the API: `GET $API/device/7/routes` shows `enabledRoutes`.

### Step 6: use the routes from other devices

Unchanged: Linux clients run `sudo tailscale set --accept-routes`.

### Updating routes

Advertise the new set on the router (`tailscale set --advertise-routes=...`);
the policy's `autoApprovers` approve it, or `POST $API/device/{id}/routes`
with the full list. Routes approved by `autoApprovers` are dropped when the
router stops advertising them.

### DNS and split DNS ("admin console > DNS")

Resolve internal names through a DNS server inside the subnet:

```shell
curl -sS -X PATCH -H "$(auth)" -H 'Content-Type: application/json' "$API/tailnet/-/dns/split-dns" \
  -d '{"corp.example":["192.168.77.53"]}'
curl -sS -H "$(auth)" -H 'Content-Type: application/json' "$API/tailnet/-/dns/searchpaths" \
  -d '{"searchPaths":["corp.example"]}'
```

The policy must allow the devices to reach `192.168.77.53` (UDP and TCP 53),
e.g. the subnet grant above. Global nameservers and MagicDNS:
`POST $API/tailnet/-/dns/nameservers` and `POST $API/tailnet/-/dns/preferences`
(section 1.5).

### High availability

Unchanged: run a second router advertising the same routes (both enrolled
with the `tag:router` key, so both are approved). One serves each route; when
it goes offline, the other takes over. Do not set `--accept-routes` on the
routers themselves (Tailscale's warning applies).

### Disable SNAT

Unchanged on the router (`sudo tailscale up --snat-subnet-routes=false`, or
`tailscale set`). The hosts of the subnet need a return route to the tailnet's
addresses through the router's LAN address. On this control plane the
tailnet addresses come from the cluster's prefixes
(`100.64.0.0/10` and `fd7a:115c:a1e0::/48` unless the operator set others);
every tailnet allocates its own addresses from them, so two tailnets may use
the same addresses (they never see each other).

### IPv6

Unchanged: advertise and grant the IPv6 prefix like the IPv4 one (examples
above).

## 3. Differences from Tailscale

| Topic | Tailscale | This control plane |
|---|---|---|
| Console | Web admin console | None yet: everything through `/api/v2` with the tailnet's API key |
| Credentials | API keys, OAuth clients | Platform-issued API keys bound to one tailnet; no OAuth clients or tokens; a tenant cannot create API keys |
| `{tailnet}` | Tailnet name or `-` | `-` or the key's tailnet ID; anything else `404` |
| Empty policy | Allow-all default | **Deny all**: `{}`, `{"grants":[]}` and "no policy stored" all deny; never allow-all |
| Policy limits | Tailscale's | `max_policy_bytes` (default 256 KiB, `413`) and the per-tailnet write bucket shared with DNS and device key writes (default 30/min, burst 10, `429`) |
| DNS | Console page; API `dns/*`, `dns/configuration` | The `dns/*` endpoints of section 1.5 (`dns/configuration` is not provided); `preferences` also carries `overrideLocalDNS`; split DNS domains and search paths cannot be in the cluster's MagicDNS domain (search paths: except your own); nameservers cannot be loopback, link-local or unspecified addresses; extra records and the base domain are operator settings |
| Tailnet settings | `PATCH /tailnet/-/settings` | Operator only (`403`) |
| Device approval, client updates | API | Operator only (`403`) |
| Malformed JSON body | `400` | `422` for a body of the wrong shape on a typed endpoint (Huma's schema check); `400` for refused values |
| Route selection | Active-passive failover, regional routing options | Active-passive failover only |
