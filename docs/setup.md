# Setup

## Requirements

- **OpenSearch** — you provide it: `memento` connects to an existing cluster and
  does not embed a store. Install and configure it per the official
  documentation (see [section 1](#1-opensearch)).
- **Go 1.26.8+** — only if you build `memento` from source (`go build -o memento .`).

`memento` talks to OpenSearch over HTTPS with basic auth; any reachable 2.x/3.x
works.

## 1. OpenSearch

`memento` needs a reachable OpenSearch — it does not embed a store and does not
ship one. Install and configure it per the official documentation; that part is
not covered here:

- **Install** (packages, Docker, Helm): <https://docs.opensearch.org/latest/install-and-configure/install-opensearch/index/>
  — a single local node is enough to start.
- **Configure** (security, TLS, users): <https://docs.opensearch.org/latest/>

What `memento` expects:

| What | Value |
|---|---|
| Version | OpenSearch 3.8.0 (any reachable 2.x/3.x works) |
| REST API | `https://127.0.0.1:9200` — HTTPS with the bundled demo certificate is fine (`insecure_skip_verify: true`) |
| Auth | basic auth, user `admin` |

Check that it answers before continuing:

```
curl -k -u admin:<your-password> https://127.0.0.1:9200
```

If you get the cluster info JSON back, the cluster is up. Remember the admin
password — you will put it in the `memento` config below.

## 2. OpenSearch in a container (dev / single-machine recipe)

`make dev-opensearch` starts a single-node OpenSearch in a container
(docker, podman-compose, podman — auto-detected). This is the fastest
onboarding path and the dev cycle recipe:

```
make dev-opensearch          # up; data in the named volume memento-os-data
make dev-opensearch-logs     # tail the node logs
make dev-opensearch-down     # stop; the named volume keeps the data
```

The node binds **127.0.0.1:9200 only** and runs with the security plugin off —
this is the dev rule: plaintext credentials are acceptable only while the node
cannot be reached off this machine. Snapshots land in `docker/os-snapshots/`
on the host (`path.repo` is configured). If this machine already runs
OpenSearch on 9200, start the dev node beside it: `MEMENTO_OS_PORT=9299 make dev-opensearch`.

For a dev node, `memento.conf` uses dummy credentials with plaintext allowed
(the file is `chmod 600`, the node is localhost-only):

```json
{
  "opensearch_url": "http://127.0.0.1:9200",
  "opensearch_user": "dev",
  "opensearch_password": "dev",
  "allow_plaintext": true
}
```

Containers love to lose data: before `make dev-opensearch-down` take a
snapshot (repository `memento-fs` → `/snapshots` → `docker/os-snapshots/`) —
see [backup.md](backup.md) §5.

### Dashboards (optional UI)

`make dev-opensearch-dashboards` adds OpenSearch Dashboards on top of the dev
node (compose profile `dashboards`, so the plain `up`/`down` cycle does not
pull it in). UI: http://127.0.0.1:5601 (`MEMENTO_DASH_PORT=5699` to move it);
with the security plugin off it opens without login.

A ready-made bundle ships in `docker/dashboards/objects.ndjson`: index
patterns for `memento`, `memento-history-*` and `memento-docs`, visualizations
(facts by project/category/status, journal writes over time, most-written keys,
counts) and the "Memento — store overview" dashboard. Push it with:

```
scripts/push-dashboards.sh
```

Re-runnable: objects carry stable `memento-store-*` ids and the import
overwrites them in place. Against a secured cluster (security plugin on) the
script resolves the password wallet-first: `memento creds get` — the same
keyring entry the MCP server uses (point at a config outside the cwd with
`MEMENTO_CONF=...`). Overrides: `DASHBOARDS_USER`/`DASHBOARDS_PASSWORD` env;
headless fallback (no session bus): a `~/.config/memento/dashboards.env`
file (mode 600):

```sh
DASHBOARDS_USER=admin
DASHBOARDS_PASSWORD=...
```

## 3. Configure memento

Config lookup order:

1. `--conf <path>` flag (`memento mcp` and `memento status` only)
2. `MEMENTO_CONF` environment variable
3. `./memento.conf` (current directory)
4. `memento.conf` next to the binary
5. `~/.memento.conf`

Create `~/.memento.conf`:

```json
{
  "opensearch_url": "https://127.0.0.1:9200",
  "opensearch_user": "admin",
  "insecure_skip_verify": true,
  "index_name": "memento",
  "history_index_period": "month",
  "allow_plaintext": false
}
```

```bash
chmod 600 ~/.memento.conf
```

- Leave `opensearch_password` out of the file: store the password in the OS
  keyring with `memento creds set` (run it in a terminal — it prompts for the
  password; the password is never an argument, so it never ends up in shell
  history). Resolution order: `OPENSEARCH_PASSWORD` env → keyring → file.
  A plaintext password in the file is only read when `allow_plaintext: true`
  (off by default). Containers and ssh sessions have no keyring — export
  `OPENSEARCH_PASSWORD` there.
- The index is created automatically on first use — you do not have to create it.
- The history journal is created automatically on the first accepted write: an
  ingest pipeline `<index>-journal-ts` (stamps entries with the server clock),
  a composable index template `<index>-journal`, and dated journal indices
  `<index>-history-<period-date>` (e.g. `memento-history-2026.11`) — monthly
  by default (`history_index_period`:
  `day`/`week`/`month`/`quarter`/`year`). The store user needs privileges to
  create the template, the pipeline, and `<index>-history-*` indices.
- Journal entries are append-only and never pruned automatically (write
  forever): disk usage grows with the write rate, not with fact count.
  Dated indices keep old periods cheap to inspect, delete, or snapshot;
  `memento prune` is a manual admin tool for one key.
- Every write is stamped with authorship labels (`author`, `author_source`,
  `model`, `host`, `session`; later writes move `updated_by`). The label comes
  from the store's security plugin (`/_security/authinfo`) when it is
  available, otherwise from the config keys `author`/`model`/`host`/`session`
  (env `MEMENTO_AUTHOR`/`MEMENTO_MODEL`/`MEMENTO_HOST`/`MEMENTO_SESSION`),
  otherwise the OS user. This is self-reported audit metadata — see
  [docs/how-it-works.md](how-it-works.md).
- Long facts stay skimmable: a value over `summary_min_len` characters
  (default 2000) must ship a `summary` (CLI `--summary`, MCP `summary`), kept
  under `summary_max_len` (default 400). Set `summary_min_len: 0` to disable
  the requirement, `summary_max_len: 0` to leave the summary uncapped.
- Upgrading from a pre-journal release: run `memento migrate-history` (prints
  the plan; `--yes` writes) to move the old in-doc history arrays into the
  journal. Unmigrated facts keep working; their history just stays in-doc.
- A malformed config file fails loudly — `memento` prints which file it tried to
  read.

Environment variables override the file (the complete list; `memento status`
lists the active ones by name, never by value):
`OPENSEARCH_URL`, `OPENSEARCH_USER`, `OPENSEARCH_PASSWORD`,
`OPENSEARCH_INSECURE_SKIP_VERIFY`, `MEMENTO_CONF`, `MEMENTO_INDEX`,
`MEMENTO_HISTORY_PERIOD`, `MEMENTO_HISTORY_LIMIT`, `MEMENTO_SUMMARY_MIN_LEN`,
`MEMENTO_SUMMARY_MAX_LEN`, `MEMENTO_SECRET_GUARD`,
`MEMENTO_ALLOW_PLAINTEXT`,
`MEMENTO_AUTHOR`, `MEMENTO_MODEL`, `MEMENTO_HOST`, `MEMENTO_SESSION`,
`MEMENTO_DOCS_INDEX`, `MEMENTO_DOCS_TTL_DAYS`, `MEMENTO_FETCH_TIMEOUT`,
`MEMENTO_FETCH_MAX_BYTES`, `MEMENTO_FETCH_HOSTS`, `MEMENTO_FETCH_USER_AGENT`,
`MEMENTO_FETCH_PROXY_URL`, `MEMENTO_FETCH_PROXY_FALLBACK`,
`MEMENTO_FETCH_RESPECT_ROBOTS`, `MEMENTO_FETCH_ALLOW_PRIVATE`,
`MEMENTO_FETCH_RENDER`, `MEMENTO_FETCH_OBSCURA_PATH`,
`MEMENTO_FETCH_OBSCURA_STEALTH`,
`MEMENTO_LOCAL_REPO_ROOTS`, `MEMENTO_REPO_SCAN_DEPTH`.

## 4. Verify

```
memento status
```

Expected output:

```
config file: /home/you/.memento.conf
url: https://127.0.0.1:9200
index: memento
history_index_period: month
history_limit: 20
secret_guard: reject
author: "admin" (security)
password: keyring
cluster: opensearch (OpenSearch 3.8.0)
storage: cluster=yellow index=yellow unassigned_shards=1 data_nodes=1
  all primary shards are active; some replica shards are unassigned, so redundancy is reduced
disk: busiest data node node-1 at 15% used
status: OK
```

`password:` shows where the password resolves from (`env`, `keyring`, `file`,
`none`); the value is never printed. `memento creds status` shows the same
plus the keyring state. `author:` is the label stamped on new writes and
which chain link produced it (`security` / `config` / `os`); it is absent
when nothing resolves.

If OpenSearch is unreachable, `status` prints `NOT AVAILABLE` with the reason and
a hint, and exits non-zero. If any environment variable is overriding the file,
it is listed under `env overrides` — check that first when a setting looks wrong.

## 5. First fact

```
memento add global:demo:hello "hello world" behavior
memento get global:demo:hello
memento search hello
memento list global
memento obsolete global:demo:hello "demo removed"
```

## 6. curl recipes (raw OpenSearch)

`memento` never prints the password, so raw curl takes it from one of the
sources:

- **Keyring** (desktop; needs the tools package, e.g. `libsecret-tools` on
  Debian/Ubuntu):

  ```bash
  curl -k -u admin:"$(secret-tool lookup service memento username 127.0.0.1:9200)" \
       https://127.0.0.1:9200
  ```

- **Environment**: `read -rs OPENSEARCH_PASSWORD` (prompt, nothing lands in
  history), then `curl -k -u "admin:$OPENSEARCH_PASSWORD" https://127.0.0.1:9200`.
- **Plaintext config** (only when `allow_plaintext: true`): read
  `opensearch_password` out of the file with your JSON tool.

## 7. Least-privilege store user (security plugin)

With the security plugin enabled, `memento` does not need an admin account.
Create a dedicated user whose role can do exactly what the app does — and
**cannot delete the facts index**. The names below use the default index
names; if you set `index_name`/`docs_index_name`, adjust the patterns
(the journal pattern is `<index>-history-*`).

What the app touches, permission by permission:

| Target | Why | Actions |
|---|---|---|
| cluster | version probe, `status` health/disk, journal infra | `cluster:monitor/main`, `cluster:monitor/health`, `cluster:monitor/nodes/stats`, `manage_index_templates`, `manage_ingest_pipelines` |
| `memento` (facts) | index checks, read, search, create/update docs | `read`, `search`, `indices:admin/get`, `indices:data/write/index`, `indices:data/write/update`, `indices:admin/create` — **no delete** (the app never deletes facts; `obsolete` is a status, and index loss must be an admin decision) |
| `memento-history-*` | append entries, read journal, `prune`, pin the ts pipeline | `read`, `search`, `indices:admin/get`, `indices:data/write/*` (append-only by app design; `delete` here exists only for the manual `prune`), `indices:admin/create` (a new period index is auto-created on the first append of that period), `indices:admin/settings/update` (the app pins the ts pipeline on journal indices) |
| `memento-docs` | fetch cache: store, read, search, re-tag snapshots | `read`, `search`, `indices:admin/get`, `indices:data/write/index`, `indices:data/write/update` (re-tagging a stored snapshot with a different project), `indices:admin/create` |

`indices:admin/get` covers the index-existence HEAD the app runs on every
start and the `GET /<index>` settings read behind `memento status`.

If you provisioned the §7 role **before v0.7.0**, update it: journal appends
now also require `indices:admin/create` and `indices:admin/settings/update`
on `memento-history-*` (without them appends fail loudly with 403), and the
docs index needs `indices:data/write/update` for re-tagging.

`GET /_security/authinfo` (the authorship probe) is self-service: every
authenticated user may call it for themselves, so no extra permission is
needed.

Create it as admin (passwords via prompts so they never land in shell history):

```bash
read -rs OS_ADMIN_PW

cat > /tmp/memento_role.json << 'EOF'
{
  "cluster_permissions": [
    "cluster:monitor/main",
    "cluster:monitor/health",
    "cluster:monitor/nodes/stats",
    "manage_index_templates",
    "manage_ingest_pipelines"
  ],
  "index_permissions": [
    {"index_patterns": ["memento"],
     "allowed_actions": ["read", "search", "indices:admin/get",
                         "indices:data/write/index",
                         "indices:data/write/update", "indices:admin/create"]},
    {"index_patterns": ["memento-history-*"],
     "allowed_actions": ["read", "search", "indices:admin/get",
                         "indices:data/write/*",
                         "indices:admin/create", "indices:admin/settings/update"]},
    {"index_patterns": ["memento-docs"],
     "allowed_actions": ["read", "search", "indices:admin/get",
                         "indices:data/write/index",
                         "indices:data/write/update", "indices:admin/create"]}
  ]
}
EOF

curl -k -u "admin:$OS_ADMIN_PW" -X PUT \
  https://127.0.0.1:9200/_plugins/_security/api/roles/memento_app \
  -H 'Content-Type: application/json' -d @/tmp/memento_role.json

# the app user (the prompt reads the password; the value stays in your shell).
# jq builds valid JSON no matter what the password contains (", \, $...).
read -rs MEMENTO_USER_PW
curl -k -u "admin:$OS_ADMIN_PW" -X PUT \
  https://127.0.0.1:9200/_plugins/_security/api/internalusers/memento \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --arg pw "$MEMENTO_USER_PW" '{password: $pw}')"

# map the role to the user
curl -k -u "admin:$OS_ADMIN_PW" -X PUT \
  https://127.0.0.1:9200/_plugins/_security/api/rolesmapping/memento_app \
  -H 'Content-Type: application/json' -d '{"users":["memento"]}'
rm -f /tmp/memento_role.json
```

Then set `opensearch_user: "memento"` in the config and run `memento creds set`
(and `memento status`). Two side effects worth knowing:

- Writes now stamp `author: "memento" (security)` — the name the *server*
  authenticated, which is exactly the point of a shared store.
- The journal template pins `index.number_of_replicas: 0`: on a single-node
  cluster journal indices stay green (replicas would hang unassigned — the
  classic single-node trap). Multi-node shared stores can raise it; the
  template is yours to edit, the app only creates it once if absent.

The dev compose (`make dev-opensearch`) runs with the security plugin off and
no auth at all — that is the documented dev posture (localhost-only bind).

## 8. JS rendering (optional, obscura)

Sites that gate on TLS/HTTP fingerprints answer plain fetches with
403/422/429. `fetch_render: "obscura"` adds a fallback through the obscura
headless browser (Chrome-shaped stealth fingerprint). It is opt-in: a
third-party binary (~75 MB, linux x86_64, glibc 2.35+) that executes page
JS locally.

```
make obscura
```

installs `obscura` and `obscura-worker` into `~/.local/bin` (keep them in
one directory) from the sha256-pinned 0.2.6 stealth release; put the
directory on PATH or point `fetch_obscura_path` at the binary. Enable with
`fetch_render: "obscura"` (env `MEMENTO_FETCH_RENDER`); a typo in the value
is a startup error, never a silent plain fetch. Rendered docs are marked
`via: obscura`; rendered pages flow through the same robots gate, guard and
TTL pipeline as plain fetches. v1 limitation: `final_url` of a rendered doc
is the requested URL (the dump mode does not report redirects).

The proxy is the operator's chosen egress and the renderer must not bypass
it: when `fetch_proxy_url` is set, obscura is passed `--proxy`; when only the
standard `HTTP_PROXY`/`HTTPS_PROXY` environment is configured, the renderer
consults it for the target (the plain transport honors it, so the render must
not run direct behind it). A proxy URL that cannot be forwarded (credentials
in the URL, a scheme-less value like `host:3128`) is a **config load error**
for every command — the render never falls back to direct egress behind your
back. The environment proxy is read through Go's standard proxy machinery,
which normalizes malformed values; the render validates the **parsed struct**,
so a credential hidden in a normalized path never reaches the renderer argv
(`/proc`-visible). `NO_PROXY` applies to the render as well: the renderer
shares the plain transport's egress decision. An explicit `socks5://` proxy is
forwarded to obscura verbatim — the renderer must support the scheme.

SSRF: the plain fetch refuses internal addresses at dial time (loopback,
RFC1918, link-local/cloud metadata, and the IPv4 addresses embedded in v6
tunnels — 6to4, Teredo, the NAT64 well-known prefix with its local /48
variant, and the deprecated IPv4-compatible form `::x.x.x.x`; arbitrary
RFC 6052 prefixes stay a documented residual), and a host refused
that way fails as a network error — which makes it render-eligible. A
POLICY refusal (a host outside `fetch_allow_hosts`, a redirect that would
cross to a blocked host) is final: neither the proxy nor the render
fallback retries it — the operator's wall is the same wall everywhere.
The renderer dials on its
own, so a **direct** render resolves the target through the same rule and
refuses a non-public answer (`fetch_allow_private: true` is the documented
opt-in for self-hosted targets). With a proxy the proxy is the egress and
enforces the policy — run the renderer behind a proxy if you need the
allowlist and the internal-address rule enforced on the render route: obscura
follows redirects on its own, and a DNS-rebinding window between the
pre-check and obscura's dial cannot be closed from memento's side.
Stored docs also drop image references aimed at internal hosts (the
drop sets `truncated`; `fetch_allow_private` keeps them for self-hosted
documentation).

Fetch honesty limits, stated plainly: `robots.txt` gates the REQUESTED URL
(the rules honor RFC 9309 path wildcards `*` and end-anchor `$`; a redirect
to a different host is checked by the allowlist/SSRF wall, not by a re-read
robots file); response bodies are treated as UTF-8 — a server declaring
another charset is not transcoded (mojibake is visible in the stored text;
the renderer or a manual re-encode are the cures), and the renderer's
environment drops every variable naming MEMENTO or OPENSEARCH (the MCP's
own config, up to a password set via env) plus a sandboxed HOME; the rest
of the operator environment passes through, so wrapper scripts around
`obscura_bin` keep working.

Time budget of one `memento fetch` / `memento_fetch_doc`, worst case: the
polite 429 retry (a ~2 s pause + one more request) plus the proxy fallback
(one more `fetch_timeout`) plus the render fallback (the nav timeout + 20 s
of process headroom) — a few times `fetch_timeout` plus the render window.
The stages are each bounded; there is no single global deadline (MCP clients
set their own tool timeouts).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `connection refused` | OpenSearch is not running (or not at the configured URL) — check your cluster and `opensearch_url` |
| `x509: certificate signed by unknown authority` | Self-signed certificate: set `insecure_skip_verify: true` |
| `401 Unauthorized` | Wrong user/password (env, keyring, or config file) — or the security plugin was never initialized |
| `password: none` and a REFUSED hint | The file has a plaintext password while `allow_plaintext` is off: run `memento creds set`, then delete `opensearch_password` from the file |
| Timeout after 30s | Host unreachable (firewall, wrong host/port) — requests time out after 30s |
| `status` shows an unexpected config file | The lookup chain picked another file — pass `--conf <path>`, or set `MEMENTO_CONF` |
| `search` misses a fact by its key | Search covers `value`, `notes` and `summary`, not keys — use `memento get <key>` |
| render error reports both reasons, obscura side: `navigation exceeded ... deadline` | The renderer ran but could not load the page: raise `fetch_timeout_seconds` (it is the nav budget) or the site hangs the render route |
| a doc with `via: obscura` whose body is a block page ("Access denied", IP-Block/Country-Block) | The site gates on egress IP/country, not the fingerprint — the renderer passes the TLS wall only: configure `fetch_proxy_url` (the renderer inherits it; a proxy is a new IP), then re-fetch with `--refresh` (the block page stays cached until TTL) |

## Security notes

- **Secrets do not belong in facts** (passwords, tokens, private keys): the index
  is plaintext. The write paths scan for credential-like text and refuse it —
  `secret_guard` in the config: `reject` (default), `ask` (confirm a false
  positive with `--allow-secret` / `confirm_secret: true`), `allow` (scanning
  off). Detection is best effort: the rule is still the first line of defense.
- The OpenSearch password lives in the OS keyring (`memento creds set`), not in
  the config file: `allow_plaintext` is off by default, so a file password is
  refused. `memento creds status` shows the source; neither command prints the
  value.
- Change the default `admin` password before the cluster is reachable by anyone else.
- The facts index is the accumulated memory — back it up (snapshot API; recipe:
  [docs/backup.md](backup.md)). Snapshots are plaintext, like the store.
- `insecure_skip_verify: true` disables certificate verification. TLS is
  verified by default; enable this only for a local node with the demo
  certificate, never for anything shared.
- On a cluster with the security plugin, run `memento` as a least-privilege
  user (§7): the app cannot delete the facts index, and every write carries
  the authorship label the server authenticated.
