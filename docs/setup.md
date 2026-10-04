# Setup

## Requirements

- **OpenSearch** — required. `memento` does not embed a store and does not ship one.
- **Go 1.25.9+** — only if you build `memento` from source (`go build -o memento .`).

`memento` talks to OpenSearch over HTTPS with basic auth. Any reachable OpenSearch
2.x/3.x works; the setup below is what this project is developed and tested against.

## 1. OpenSearch

If you already have a cluster you can reach, skip to step 2.

A single local node is enough to start — no cluster, no dedicated hardware. A full
multi-node cluster works the same way: nothing in `memento` assumes one or the other
(it talks to the cluster you point it at). The verified setup on the development
machine:

| What | Where |
|---|---|
| Package | `opensearch` 3.8.0 (Debian/Ubuntu, official apt repo) |
| Service | `opensearch.service` (systemd), runs as user `opensearch` |
| REST API | `https://127.0.0.1:9200` |
| Transport | TLS with the bundled self-signed demo certificate |
| Auth | security plugin enabled → basic auth, user `admin` |

The apt repository line used here:

```
deb [signed-by=/etc/apt/keyrings/opensearch.gpg] https://artifacts.opensearch.org/releases/bundle/opensearch/3.x/apt stable main
```

Full instructions for every platform (Debian, RPM, tarball, Docker, Helm):
<https://docs.opensearch.org/latest/install-and-configure/install-opensearch/index/>
Debian/Ubuntu specifically:
<https://docs.opensearch.org/latest/install-and-configure/install-opensearch/debian/>

Start it and check it answers:

```
sudo systemctl enable --now opensearch
curl -k -u admin:<your-password> https://127.0.0.1:9200
```

If you get the cluster info JSON back, the cluster is up.

If you started from scratch, set the admin password during installation and
remember it — you will put it in the `memento` config.

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
  "opensearch_password": "<your-password>",
  "insecure_skip_verify": true,
  "index_name": "memento"
}
```

```bash
chmod 600 ~/.memento.conf
```

- `insecure_skip_verify: true` is needed for the bundled self-signed certificate.
  For anything shared, use a proper certificate and set it to `false`.
- The index is created automatically on first use — you do not have to create it.
- A malformed config file is an error, not a silent fallback: `memento` tells you
  which file it tried to read.

Environment variables override the file:
`OPENSEARCH_URL`, `OPENSEARCH_USER`, `OPENSEARCH_PASSWORD`,
`OPENSEARCH_INSECURE_SKIP_VERIFY`, `MEMENTO_INDEX`.

## 3. Verify

```
memento status
```

Expected output:

```
config file: /home/you/.memento.conf
url: https://127.0.0.1:9200
index: memento
cluster: opensearch (OpenSearch 3.8.0)
status: OK
```

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

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `connection refused` | OpenSearch is not running: `systemctl status opensearch` |
| `x509: certificate signed by unknown authority` | Self-signed certificate: set `insecure_skip_verify: true` |
| `401 Unauthorized` | Wrong user/password in the config file — or the security plugin was never initialized |
| Timeout after 30s | Host unreachable (firewall, wrong host/port). `memento` gives up after 30s by design |
| `status` shows an unexpected config file | Lookup order: run with `--conf`, or set `MEMENTO_CONF` |
| Search finds nothing right after `add` | Should not happen: writes wait for a refresh. If it does, call `memento status` and report it |
| `search` cannot find a fact by its key | By design: search covers `value` and `notes`, not keys. Use `memento get <key>` |

## Security notes

- **`memento.conf` contains a password.** The repository `.gitignore` excludes
  `memento.conf` — do not force-add it. Keep the file at mode `600`.
- **Never store secrets as facts** (passwords, tokens, private keys). The index is
  plaintext and facts are meant to be readable by agents.
- Change the default `admin` password before the cluster is reachable by anyone else.
- `insecure_skip_verify: true` disables certificate verification. It is fine for a
  local node with the demo certificate and wrong for anything shared.
