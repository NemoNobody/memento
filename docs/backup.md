# Backup & restore

`memento` keeps everything in OpenSearch, so the backup plan is the standard
OpenSearch snapshot API — no bespoke export tooling.

What matters:

- the facts index (`index_name`, default `memento`) — **irreplaceable**: this is
  the accumulated memory;
- the docs index (`docs_index_name`, default `memento-docs`) — a cache: a lost
  snapshot only costs re-fetches, include it if convenient;
- snapshots carry **plaintext facts** exactly like the store: the same rule
  applies — no secrets in facts, protect the backup location like the data
  directory itself.

## 1. Filesystem repository (single machine)

`path.repo` is a node setting: it must be declared in `opensearch.yml` before a
repository can be registered there (dynamic registration into an undeclared
location is refused by design).

As root (package install):

```bash
mkdir -p /var/lib/opensearch/backup
chown opensearch:opensearch /var/lib/opensearch/backup
chmod 770 /var/lib/opensearch/backup

# declare the location, then restart the node
echo 'path.repo: ["/var/lib/opensearch/backup"]' >> /etc/opensearch/opensearch.yml
systemctl restart opensearch
```

Register the repository (it survives restarts; one-time):

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X PUT \
  "https://127.0.0.1:9200/_snapshot/memento-fs" \
  -H 'Content-Type: application/json' \
  -d '{"type":"fs","settings":{"location":"/var/lib/opensearch/backup","compress":true}}'
```

## 2. Snapshot

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X PUT \
  "https://127.0.0.1:9200/_snapshot/memento-fs/backup-$(date +%Y%m%d)?wait_for_completion=true" \
  -H 'Content-Type: application/json' \
  -d '{"indices":"memento,memento-docs","ignore_unavailable":true,"include_global_state":false}'
```

Check it:

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" \
  "https://127.0.0.1:9200/_snapshot/memento-fs/backup-$(date +%Y%m%d)/_status?pretty"
```

Schedule the `curl` with cron/systemd-timer; prune old snapshots by deleting
their names (the API removes repository files safely):

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X DELETE \
  "https://127.0.0.1:9200/_snapshot/memento-fs/backup-YYYYMMDD"
```

## 3. Restore — and verify it

A backup you never restored is not a backup. Restore to new index names so the
running store is untouched:

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X POST \
  "https://127.0.0.1:9200/_snapshot/memento-fs/backup-YYYYMMDD/_restore" \
  -H 'Content-Type: application/json' \
  -d '{"indices":"memento","include_global_state":false,
       "rename_pattern":"memento","rename_replacement":"memento-restore-test"}'
```

Verify, then clean up:

```bash
memento status                              # the real index is untouched
curl -k -u "admin:$OPENSEARCH_PASSWORD" "https://127.0.0.1:9200/memento-restore-test/_count?pretty"
# compare _count with the live index, sample a known key with memento get, then:
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X DELETE \
  "https://127.0.0.1:9200/memento-restore-test"
```

A full recovery (store lost): create the index name as it was (the mapping is
restored from the snapshot), stop `memento mcp`, restore without the rename,
restart the MCP server.

## 4. Off-box backup

The fs location can be any mount (`rsync` it away, mount an S3 fuse bucket, …).
For native remote snapshots use the s3 repository plugin: declare
`s3.client.*` in `opensearch.yml`, register `{"type":"s3","settings":{"bucket":…}}`.
Same snapshot/restore calls.

## 5. Containers (dev-opensearch, single-machine recipe)

Containers lose data when the operator cleans up. Two rules:

- put the data directory in a **named volume** (not an anonymous one —
  anonymous volumes get pruned);
- take a snapshot **before** `compose down`: snapshot → copy the location out
  of the volume → `down`. The repository location must be declared in the
  container's `opensearch.yml` the same way as above (bind-mount the directory
  so copying it out is trivial).
