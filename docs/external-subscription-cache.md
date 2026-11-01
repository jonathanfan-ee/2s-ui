# External subscription snapshots (opt-in)

This feature caches existing clients' external subscription entries
(`type: "sub"` in `Client.Links`) and the
[shared source catalog](subscription-source-catalog.md). The exact URL is the
snapshot identity; catalog source IDs remain stable when URLs change. Active
clients using the same URL share a snapshot and refresh schedule. Disabling or
expiring a client still prevents subscription exports, even if its source is
cached. This does not introduce shared user credentials or change subscription
URLs.

## Enable

1. Open **Settings → Shared subscription sources**. The cache controls list
   enabled catalog sources and active clients' existing external URLs.
2. Choose **Queue refresh for all**. Manual refreshes work while both switches
   are off. A background poll runs every 15 seconds and handles at most one due
   source, so a queue of sources is processed gradually.
3. Wait until every source is **Ready**. Cache reads cannot be enabled while an
   source used by an active client has no valid snapshot. Unused catalog sources
   can be prepared separately.
4. Enable **Serve cached snapshots**, optionally enable **Automatic refresh**,
   choose a collection interval (5–1440 minutes, default 5), then **Apply cache
   settings**. These controls save separately and do not restart the proxy core.

Both switches default off. Catalog sources always read snapshots. The cache-read
switch applies only to legacy per-client URLs: with it off, they retain live-fetch
behavior. With it on, their downloads only read persisted snapshots;
there is no fallback to an upstream request. Adding an unprimed source after
activation makes that client's export fail with HTTP 503 and `Retry-After: 300`,
instead of publishing a partial configuration that could replace a working one.
Removing a source from a client removes that client's access immediately.

The collection interval is independent of the client update interval. A local
client's full configuration refresh may reload its core; it need not be set to
five minutes just because source collection is scheduled every five minutes.

## Refresh behavior

- Successful nonempty URI lists, Base64 URI lists, or sing-box JSON `outbounds`
  replace the persisted snapshot atomically. Native Clash YAML is not supported
  by this phase. Existing Clash/XHTTP conversion remains in use.
- Non-2xx responses, HTML/login pages, empty/unreadable responses and unsupported
  content do not replace the last valid snapshot. Generic failures back off
  exponentially, up to six hours. Success resets this backoff.
- HTTP 401/403 pauses a source. **Resume and queue** is explicit and does not
  bypass a cooldown. HTTP 429/503 honors a later `Retry-After` deadline.
- The shared budget allows at most four request attempts in a rolling minute,
  including redirect hops and failed requests. It is persisted across restarts.
  Repeated manual queueing does not bypass it or an existing source cooldown.
  A job waiting for this local budget stays queued and retries after a minute,
  without counting the wait as a provider failure.
- A source is leased during a fetch. An expired/replaced lease or a source no
  longer present as an enabled catalog source or an active legacy URL cannot
  publish that job's response.
- Source status never includes the snapshot payload, complete upstream URL,
  URL query/token or lease. Fetch errors use a fixed, URL-free vocabulary.

Scheduled fetches require a public HTTPS endpoint on port 443, use IPv4, ignore
proxy environment variables and verify TLS certificates. DNS addresses are
validated and pinned for dialing; private/reserved targets and unsafe redirect
hops are rejected. Fetches use `SubscriptionFetcher/1.0`, a ten-second total
HTTP timeout and an eight-MiB body limit. Sources with other requirements must
keep live mode or be adapted separately before cache mode is enabled.

## API

Existing `/api` session authentication and `/apiv2` token authentication apply.
POST bodies use `Content-Type: application/json`.

- `GET externalSubCache`: configuration and metadata for assigned sources.
- `POST configureExternalSubCache`:
  `{"enabled":true,"autoRefresh":true,"refreshMinutes":5}`.
- `POST refreshExternalSubs`: `{"keys":["source-sha256"],"resume":false}`.
  An empty keys array queues all assigned sources; paused sources require an
  explicit resume. The call only queues work; it never fetches inline.

The cache settings cannot be written through the general settings action.
Database exports include snapshots and the shared request budget; backups
contain node credentials just as the panel's main database does and should be
kept private.

## Scope

This is not a migration/deployment procedure. The shared source catalog and
client grants are described separately. Native Clash snapshots, per-user client
templates, remote fixed-egress collectors and legacy subscription URL routes
remain separate work. Scheduled requests currently originate from the
panel host's IPv4; this feature does not contact an existing external collector.
