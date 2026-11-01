# Shared subscription source catalog

The source catalog adds a stable source ID and explicit client grants on top of
the [persistent snapshots](external-subscription-cache.md). Provider URLs are
maintained once; no provider URL is copied into a client's row. Sources and grants
belong to the central panel and are not part of the client payload synchronized
to nodes.

## Workflow

1. Open **Settings → Shared subscription sources** and add a source. Keep its
   client selection empty while preparing a new URL.
2. Queue a refresh and wait for a valid snapshot. Enabled catalog sources can be
   manually prewarmed even without assigned clients and with automatic refresh
   off. Unused sources do not consume automatic collection requests.
3. Edit the source and select the clients allowed to receive its nodes. Existing
   client subscription addresses and local protocol credentials do not change.
4. Optionally enable automatic refresh in the cache controls. The default
   collection interval is five minutes, with the shared request budget and
   cooldown described in the cache documentation.

Catalog sources **always** serve snapshots, independently of the legacy
cache-read switch. Client downloads never trigger provider requests for catalog
entries. A source assigned before prewarming returns HTTP 503 for that client's
configuration until its snapshot is ready. The other clients remain independent.

Removing a grant removes that source from that client's next exported
configuration. Disabling a source removes it from catalog exports for every
client. Deleting a source deletes its grants in the same transaction. None of
these operations changes the client's subscription address or its local node
credentials. Disabling/expiring the client blocks access even if a valid source
snapshot remains available. Changing the source URL preserves its stable source
ID and all grants, but uses the new URL's snapshot identity immediately; the old
URL's content is not served as a fallback.

A subscription already imported by a client still contains the node credentials
it previously received. Removing a catalog grant changes future distribution; it
does not revoke a provider's already-issued node credentials. Personal credential
revocation remains a node/provider responsibility.

## Existing per-client links

Existing `Client.Links` entries retain their behavior. A client can keep independent
legacy external URLs alongside catalog grants. Where the exact same URL is granted
through both paths, export prefers the snapshot-only catalog path and emits it
once. Two catalog entries referencing the same URL also share one snapshot and
emit one set of nodes per client.

Legacy authorization is separate: revoking a catalog grant does not remove an
explicitly retained per-client URL. For a complete transition, select **Convert
matching legacy links to catalog grants** while saving the source. This explicit
operation requires an existing valid snapshot and:

- removes only `type: "sub"` entries with the exact current source URL;
- changes only the selected clients, atomically with source/grant updates;
- retains local/node links, other URLs and unknown fields on retained entries;
- preserves client names, subscription addresses, traffic, expiry and credentials.

The operation never imports a provider over the network. Automatic migration of
all legacy URLs is intentionally absent.

## Admin API

Existing session (`/api`) and token (`/apiv2`) authentication apply. JSON POST
bodies use `Content-Type: application/json`.

- `GET externalSubSources` returns source names, stable IDs, URL hostnames,
  snapshot status, revisions, selected client IDs and a minimal client list.
  Complete URLs, URL queries/tokens and snapshot bodies are not returned.
- `POST saveExternalSubSource` creates or updates a source:

  ```json
  {
    "id": "SOURCE_ID_ON_EDIT",
    "revision": 1,
    "name": "Provider",
    "url": "",
    "enabled": true,
    "clientIds": [1, 2],
    "adoptLegacy": false
  }
  ```

  Omit `id` and `revision` to create a source and supply its public HTTPS URL.
  On edit, a blank URL preserves the write-only stored URL. Omitted `clientIds`
  preserves grants; `[]` explicitly revokes all grants. Nonexistent client IDs
  reject the entire transaction. A stale revision cannot overwrite a newer URL,
  enabled state or grant selection. Source audit records contain IDs and whether
  the URL changed, not the URL itself.
- `POST deleteExternalSubSource`: `{"id":"SOURCE_ID","revision":1}`.
- `POST refreshExternalSubs`: use the source's `cache.key` with the existing queue
  API. Refresh is still asynchronous and respects the shared rate budget.

Snapshots, catalog sources and grants are included in private database backups.
The admin API's write-only URL policy does not remove URLs from database backups.

## Migration boundary

This change adds catalog and access management; it does not install a panel,
modify an existing collector, copy live provider URLs, or migrate distributed
MiSub links. Native Clash snapshots and the optional collector have their own
documentation. Per-client templates and legacy URL routes remain separate acceptance work.

The existing external collector contract uses signed task/result requests and
leases, and returns separate universal and native Clash responses. The
[adapter package](../collector/README.md) uses stable source identities, URL hashes, revision/lease fences and independent per-format validation. Reusing an
admin token or asking a client to fetch a provider is not the collector path.
Real collector credentials and operational registration are not included.
