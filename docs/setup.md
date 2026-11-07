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

## 2. Configure memento

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
- A malformed config file fails loudly — `memento` prints which file it tried to
  read.

Environment variables override the file (the complete list; `memento status`
lists the active ones by name, never by value):
`OPENSEARCH_URL`, `OPENSEARCH_USER`, `OPENSEARCH_PASSWORD`,
`OPENSEARCH_INSECURE_SKIP_VERIFY`, `MEMENTO_CONF`, `MEMENTO_INDEX`,
`MEMENTO_HISTORY_LIMIT`, `MEMENTO_SECRET_GUARD`, `MEMENTO_ALLOW_PLAINTEXT`,
`MEMENTO_DOCS_INDEX`, `MEMENTO_DOCS_TTL_DAYS`, `MEMENTO_FETCH_TIMEOUT`,
`MEMENTO_FETCH_MAX_BYTES`, `MEMENTO_FETCH_HOSTS`, `MEMENTO_FETCH_USER_AGENT`,
`MEMENTO_FETCH_PROXY_URL`, `MEMENTO_FETCH_PROXY_FALLBACK`,
`MEMENTO_FETCH_RESPECT_ROBOTS`, `MEMENTO_LOCAL_REPO_ROOTS`,
`MEMENTO_REPO_SCAN_DEPTH`.

## 3. Verify

```
memento status
```

Expected output:

```
config file: /home/you/.memento.conf
url: https://127.0.0.1:9200
index: memento
history_limit: 20
secret_guard: reject
password: keyring
cluster: opensearch (OpenSearch 3.8.0)
storage: cluster=yellow index=yellow unassigned_shards=1 data_nodes=1
disk: busiest data node at 15% used
status: OK
```

`password:` shows where the password resolves from (`env`, `keyring`, `file`,
`none`); the value is never printed. `memento creds status` shows the same
plus the keyring state.

If OpenSearch is unreachable, `status` prints `NOT AVAILABLE` with the reason and
a hint, and exits non-zero. If any environment variable is overriding the file,
it is listed under `env overrides` — check that first when a setting looks wrong.

## 4. First fact

```
memento add global:demo:hello "hello world" behavior
memento get global:demo:hello
memento search hello
memento list global
memento obsolete global:demo:hello "demo removed"
```

## 5. curl recipes (raw OpenSearch)

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

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `connection refused` | OpenSearch is not running (or not at the configured URL) — check your cluster and `opensearch_url` |
| `x509: certificate signed by unknown authority` | Self-signed certificate: set `insecure_skip_verify: true` |
| `401 Unauthorized` | Wrong user/password (env, keyring, or config file) — or the security plugin was never initialized |
| `password: none` and a REFUSED hint | The file has a plaintext password while `allow_plaintext` is off: run `memento creds set`, then delete `opensearch_password` from the file |
| Timeout after 30s | Host unreachable (firewall, wrong host/port) — requests time out after 30s |
| `status` shows an unexpected config file | The lookup chain picked another file — pass `--conf <path>`, or set `MEMENTO_CONF` |
| `search` misses a fact by its key | Search covers `value` and `notes`, not keys — use `memento get <key>` |

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
- `insecure_skip_verify: true` disables certificate verification. It is fine for a
  local node with the demo certificate and wrong for anything shared.
