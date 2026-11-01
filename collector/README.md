# Fixed IPv4 collector for 2S-UI

Development package; no installation, credentials, provider URLs or production
registration are included. The Python agent is adapted from MiSub under MIT
(see [license](../LICENSES/MiSub.txt)). Its existing two-format task/result wire
protocol is retained, with support for a panel mounted below a path such as
`/app/`. The existing deployed MiSub collector is not changed by this package.

## Server registration

The panel reads an optional private environment variable, `SUI_SUB_COLLECTORS`:

```json
{
  "collector-example": {
    "secret": "REPLACE_WITH_A_RANDOM_SECRET_OF_AT_LEAST_32_CHARACTERS",
    "expectedIpv4": "REPLACE_WITH_VERIFIED_PUBLIC_IPV4"
  }
}
```

The placeholders deliberately fail validation. Register the VM's actual direct
outbound IPv4 after an approved preflight. Secrets must be random, at least 32
characters, and kept out of source control and subscription templates. The admin
API returns only collector IDs and expected IPv4 addresses. The registry is not
stored in the panel database or returned by the settings endpoint; keep its
private deployment configuration in a separate secure backup.

An empty registry enables no machine identity. Invalid registration fails closed.
A missing/changed registration never sends a remotely assigned source through
the panel's own exit. Existing valid snapshots remain readable by authorized
clients while collection is unavailable.

Select the collector in a catalog source's **Collection exit** field. A source
with the same exact URL cannot declare a different collector exit through an
alias entry. Explicit legacy URLs matching a remote source must be migrated
before assigning or removing its remote route, so a route handoff cannot expose
an overlooked live-fetch path.

## Agent configuration

`config.example.json` demonstrates the shape. A real private configuration is:

- `/etc/subscription-collector/config.json`, mode 0600, owned by the service user;
- `panelUrl`: the HTTPS panel base URL, including its path, e.g.
  `https://panel.example/app`;
- `id` and `secret`: the server registration's exact values.

The compatibility key `misubUrl` remains accepted for the existing root-path
MiSub endpoint. If both URL keys are present they must agree. `--check` validates
local configuration without network access. The script requires Python 3.9+
standard library only and opens no inbound listener.

The supplied systemd examples expect program files under
`/opt/subscription-collector`, the `subscription-collector` service account and a
private state directory `/var/lib/subscription-collector`. They poll approximately
once a minute. This does not make provider refreshes one-minute requests: source
cooldowns are controlled by the panel, normally five minutes. Installation and
service activation require the operator's separately reviewed approval.

The agent uses verified direct IPv4 HTTPS, ignores proxy environment variables,
pins validated public DNS addresses while retaining the original TLS hostname,
validates redirects and bounds the complete upstream request to ten seconds.
Each response is limited to 512 KiB. Universal and native Clash requests run
concurrently with `SubscriptionFetcher/1.0` and `ClashMeta/1.19.32` respectively.
A persisted rate file admits at most four upstream attempts in a rolling minute,
including redirect hops and failures. Corrupt rate state stops requests rather
than resetting the budget. The process lock prevents overlapping agent runs.

## Signed API

Routes are below the panel base, independent of browser sessions and apiv2 admin
tokens:

- `POST <panel-base>/api/collector/tasks`, JSON `{}`: zero or one leased source;
- `POST <panel-base>/api/collector/results`: per-format results for that lease.

Headers are `X-Collector-Id`, `X-Collector-Timestamp` (epoch milliseconds),
`X-Collector-Nonce` and `X-Collector-Signature` (hex HMAC-SHA256). Sign:

```text
POST\n<FULL_ESCAPED_PATH>\n<COLLECTOR_ID>\n<TIMESTAMP>\n<NONCE>\n<HEX_SHA256_OF_EXACT_BODY>
```

A panel at `/app/` signs `/app/api/collector/tasks`, not `/api/collector/tasks`.
The panel verifies the signature and actual caller IPv4. Forwarding headers are
accepted only through the panel's existing trusted-proxy configuration; a forged
`X-Forwarded-For` or `CF-Connecting-IP` from an untrusted peer cannot select the
collector's identity. A reverse proxy deployment must verify the resulting
client-IP chain before collector activation.

Signatures allow less than five minutes of clock skew. Nonces persist across
restarts, including the full remaining validity of future-dated requests.
Authenticated control requests are limited to 30 per rolling minute per identity.
Tasks carry only the assigned URL, URL hash, source ID, lease and requested formats;
no user list, profile templates or administrator credentials are returned.

Publication rechecks the source's enabled state, collector assignment, URL hash,
revision, lease token, lease deadline and registration fingerprint in a
transaction. Each format is validated again independently. Invalid content
cannot replace that format's previous valid snapshot. HTTP 401/403 or an explicit
provider suspension pauses collection; resuming is explicit and preserves the
cooldown. HTTP Retry-After can extend the next attempt. Browser downloads never
claim work or fetch a remote provider.

## Controlled handoff from MiSub

Before any production migration, review a concrete plan covering the selected
VM, direct IPv4, private registrations, panel/proxy URL, backups, paths, owner and
rollback. Keep one active collection owner per provider URL. Do not start a
second collector against the same provider while the old MiSub schedule is still
active, or reset its persisted rate history to force a request. Coordinate the
old schedule/lease/cooldown and reuse protected rate state when moving the same
agent on the same VM. First prewarm and verify both formats, then validate real
client connectivity before changing legacy distribution.

This package alone does not migrate existing MiSub user URLs or templates. Their
compatibility and the actual collector handoff remain separate rollout checks.

## Optional subscription-listener control endpoint

`SUI_SUB_COLLECTOR_API=true` registers only `/api/collector/tasks` and
`/api/collector/results` on the subscription listener. It is off by default.
It requires native HTTPS, a valid HMAC and the registered public socket-peer
IPv4. Forwarding headers are deliberately ignored on this listener; use the
existing management endpoint for a separately reviewed trusted-proxy setup.
No browser/admin API is exposed by this flag. Registration still comes from
`SUI_SUB_COLLECTORS`, separately from this non-secret opt-in flag.

An agent may set `panelUrl` to an HTTPS subscription origin with a custom port,
for example `https://panel.example:32443`. Only control-plane requests accept
that configured port. Provider requests and provider redirects still require
HTTPS port 443 and public IPv4 DNS results. TLS hostname verification and the
persisted upstream rate limit are unchanged.

## Internal raw source feeds for a legacy aggregator

An administrator may create an opaque, dedicated client in group `@source-feed`
with no self-hosted inbound grants, then grant it only the catalog source it
must forward. Its ordinary subscription bearer is the credential. A raw feed is:

```
https://panel.example:32443/sub/OPAQUE_READER?source=SOURCE_ID
```

Universal fetchers receive the original universal snapshot. Clash/Mihomo UAs
receive the original native Clash snapshot. `format=universal` or `format=clash`
selects explicitly; no alternate-format fallback is performed. GET and HEAD
check enabled state, expiry, quota, the internal group and exact source grant.
Ordinary clients cannot use this mode even if they have a source grant.

The handler reads only validated persistent snapshots, without applying node
prefixes/templates or touching any provider, lease, cooldown or cache row.
`Subscription-Userinfo`, `Last-Modified` and `X-Snapshot-Updated-At` belong to
that provider snapshot, rather than the feed account or render time. Paused,
missing, future-dated or over-15-minute snapshots fail with 503; invalid or
revoked credentials fail with 404. Do not share these internal bearer URLs.

This enables a controlled migration where the central panel owns collection
while MiSub keeps its original profile/source identifiers and reads only these
feeds. It does not itself change any deployed service, source, schedule or old
link. Stop the old owner and honor its cooldown before activating the new owner;
prewarm and compare both formats before changing distribution. Never create a
cycle by feeding the central panel from MiSub while MiSub reads it back.
