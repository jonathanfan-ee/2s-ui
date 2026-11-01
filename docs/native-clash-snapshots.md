# Native Clash snapshots

Collection negotiates native Clash and universal text independently. Successful
responses retain their own payload, metadata and success time. One format's
HTTP error, HTML page, invalid graph or empty result cannot overwrite that
format's previous valid snapshot. A valid sibling can still advance.

Clash exports select a valid native snapshot first and otherwise use a valid
universal snapshot. JSON and plain-link exports require the universal format;
a native-only source does not pretend to be convertible into universal links.
A missing required format returns 503, preserving the client's previous config.
Cache status displays the two formats separately.

The native merge preserves proxy fields directly, including TLS, ShadowTLS,
transports, multiplexing and node-specific options. It:

- creates unique names across template, local and external proxies/groups;
- rewrites source-local `dialer-proxy` references after renaming;
- imports only dependency groups actually needed by those node references;
- rejects unresolved dependencies, cycles and remote-provider dependencies;
- materializes endpoint CNAME aliases and isolates provider address lists under
  deterministic private host aliases, avoiding collisions between sources;
- retains implicit TLS/SNI and transport Host identities when a dial endpoint
  changes, while explicit administrator hosts take precedence;
- imports no provider rules, global DNS, listeners or unrelated hosts records.

Source-local groups and endpoint hosts are validated and scoped; the panel's
own template remains authoritative. YAML input is bounded by body, node and
nesting limits; bounded anchors are accepted. Original protocol options are not
reconstructed through a sing-box conversion step.

Collection is either on the panel's direct IPv4 or through the registered
[fixed-egress agent](../collector/README.md). A remotely assigned URL is never
fetched by the panel, including after registry failure or when legacy cache reads
are off. Native snapshots, registration lease metadata and replay nonces are
included in database backups; actual collector secrets remain outside the DB.

Validation includes synthetic source/group/host conflicts and an optional real
Mihomo config parse. Set `MIHOMO_TEST_BINARY` to an installed validator to run that
check; it uses a temporary config/data directory and no real provider credentials.
A successful parse does not prove a node's connectivity.

Syntax references: [Mihomo Shadowsocks plugins](https://wiki.metacubex.one/en/config/proxies/ss/),
[Mihomo hosts](https://wiki.metacubex.one/en/config/dns/hosts/) and
[proxy-server-nameserver](https://wiki.metacubex.one/en/config/dns/#proxy-server-nameserver).
