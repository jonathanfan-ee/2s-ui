# Subscription policies and historical URL migration

This optional layer keeps distribution separate from node credentials. An empty
installation has no named templates, legacy hosts or aliases; existing native
subscriptions keep their format and global extension settings.

## Named templates

In **Settings → Subscription policies and migration**, save a Clash/Mihomo YAML
policy or a sing-box JSON extension and explicitly assign clients. There is one
assignment per client per format. Reassigning a client updates both affected
revisions so another stale editor cannot silently restore an older assignment.

Clash templates are full policies: they own their proxy groups, DNS, rules and
hosts, and do not receive the global `Proxy`/`Auto` groups. Credentials come only
from the client's inbounds, node links and authorized source snapshots. Static
`proxies`, remote `proxy-providers` and group `use` providers are refused.

```yaml
proxy-groups:
  - name: Main
    type: select
    proxies: [Backup, __AUTO_INSERT_NODES__, DIRECT]
    filter: '^Self - '
  - name: Backup
    type: select
    proxies: [__AUTO_INSERT_NODES__]
    filter: '^Backup - '
rules:
  - MATCH,Main
```

`__AUTO_INSERT_NODES__` (or `all`) expands in place to authorized node names.
A group filter selects only dynamic nodes; explicit fallback groups and DIRECT
remain. A filter without a marker appends matching nodes. Filters are expanded
and removed before output. Empty categories are removed transitively; rules
referencing them target the first surviving group, or DIRECT if none remains.
Filters use Go regular expressions (RE2 syntax; no lookaround).

Catalog sources can set an optional **Node name prefix**, for example `Backup - `.
This is applied at export time, including VMess labels and native Clash dependency
references. It does not rewrite provider snapshots, endpoints or credentials.
Use explicit source grants and node naming to retain an old template's categories.

JSON templates use the panel's existing extension shape (`rules`, `dns`,
`inbounds`, `rule_set`, `final`, etc.), rather than a complete sing-box config.
`outbounds` and top-level `route` are rejected. Native node generation owns
outbound credentials. A disabled, missing or invalid assigned template returns
503; it does not silently revert to another policy. Templates are stored in the
panel database; subscription requests do not fetch a Gist or template URL.
GeoSite/GeoIP datasets must contain the categories used by the template; a valid
YAML policy alone does not establish compatibility with every client's datasets.

## Historical MiSub addresses

Create an alias with the **exact HTTPS URL** already distributed, one central
client identity, an optional expiry, format policy, and optional template overrides.
Examples below are synthetic:

- `https://legacy.example/token/profile`
- `https://legacy.example/sub/token/profile`
- `https://legacy.example/master-token`
- `https://legacy.example/?token=master-token`

Each URL must be registered separately. Host and escaped path are matched exactly;
query-token values are part of the credential. Format parameters do not change
identity. URLs are write-only: editing with an empty URL retains the saved mapping.
Only a SHA-256 lookup digest is stored, not the bearer path or query token. Admin
lists and audit records omit those credentials. Database backups retain the digest,
policy content and assignments. As with all bearer subscription URLs, reverse-proxy
access logs must be configured appropriately before a live migration.

Use a **dedicated legacy hostname**, distinct from the panel/native subscription
hostname. Alias requests are intercepted on the subscription listener before its
native-domain check. Unknown paths on a reserved legacy host return 404; they
never reach native subscription names or dashboard routes. Hosts remain reserved
after alias deletion. Exact credential paths do not receive trailing-slash redirects.
The request's actual Host header is used, not X-Forwarded-Host.

Format precedence: `target`, then `format`, then the flags `clash`, `singbox`,
`base64`, `v2ray`, `trojan`, `plain`, then the saved default, then client User-Agent
for `auto`. Mihomo/Clash/OpenClash/Stash select Clash; sing-box selects JSON;
Shadowrocket and the default select base64 URI subscriptions. Explicit unsupported
formats such as Surge or dashboard/info return an error rather than a wrong config.
Native subscriptions still require their existing explicit format parameter.

Disabled/expired aliases and disabled/expired clients cannot export nodes. Single
and bulk client deletion revoke and detach aliases in the same transaction, so a
reused numeric client id cannot inherit an old URL. Deleted aliases return 404.

### Identity and migration boundaries

One old bearer URL maps to one central identity. A shared link remains a shared
identity; it cannot become independent per-person revocation without new links.
Do not map multiple differently scoped old URLs to a central client whose grants
are their union. Create distinct central identities for distinct authorization
scopes, even when one person owns multiple URLs. Templates select presentation;
they never restrict grants or replace that identity separation.

Saving an alias does not change DNS, reverse-proxy routing, certificates, node
credentials, or an existing MiSub deployment. A separate reviewed rollout must:

1. Inventory old URLs and exact grants privately; back up both systems.
2. Create equivalent central identities, prewarm granted sources, assign reviewed
   template snapshots and appropriate node prefixes.
3. Verify actual output with Mihomo and the supported mobile clients; confirm
   no extra node credentials and expected revocation behavior.
4. Route only the reviewed legacy paths to the subscription listener, preserving
   Host, HTTPS and the original URI. Keep the MiSub admin and unmigrated paths
   routed to MiSub until their migrations are explicitly complete.
5. Switch a small test identity first and retain a routing rollback. DNS alone
   cannot split one historical domain between these applications by path.

A future whole-domain cutover can use the reserved-host handler for all legacy
paths. This feature does not automatically provision legacy virtual hosts/TLS or
modify Nginx configuration.

## Authenticated administration API

Session-authenticated `/api/` and token-authenticated `/apiv2/` expose:

- GET `subscriptionDistribution`: template metadata, alias metadata, minimal clients
- GET `subscriptionTemplate?id=...`: explicit content detail for an administrator
- POST `saveSubscriptionTemplate`, `deleteSubscriptionTemplate`
- POST `saveLegacySubscriptionAlias`, `deleteLegacySubscriptionAlias`

Writes use JSON, optimistic `revision` values, and an audit actor. Template and
alias updates save independently of global settings and do not restart the core.
Public subscription queries cannot select a template id or another client's grants.

Compatibility for the MiSub placeholder expansion is informed by the MIT-licensed
MiSub project; see `LICENSES/MiSub.txt`. No private templates, domains, provider
URLs, credentials or user mappings are included in the fixtures.
