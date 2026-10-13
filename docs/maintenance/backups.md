---
title: Backups and restore
description: Stop every writer and copy the complete private volume with ownership and modes.
---
# Backups and restore

Back up the **entire private data root/volume**: archive databases and any WAL/journals, WhatsApp session, originals, models, policies, configuration and client files. Never copy a live SQLite file alone. The archive contains session material and credentials; protect backups with private permissions and disk/backup encryption. No secret is needed in helper command arguments.

The Docker procedure preserves the complete volume, file ownership, and permissions. It has not yet been tested with the current Docker setup and a real WhatsApp account. Keep your original installation until you have checked the restored archive.

## Docker Compose backup

Use a standard **POSIX shell** on the Docker host, in the same Compose project/checkout as the running archive. The pinned helper contains GNU tar; acquire it before stopping the app:

```sh
set -eu
BACKUP_HELPER=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
docker pull "$BACKUP_HELPER"
CONTAINER=$(docker compose ps -a -q wa-indexer)
test -n "$CONTAINER"
DATA_VOLUME=$(docker inspect "$CONTAINER" --format '{{range .Mounts}}{{if eq .Destination "/var/lib/wa-indexer"}}{{.Name}}{{end}}{{end}}')
test -n "$DATA_VOLUME"
printf '%s\n' "$DATA_VOLUME"
docker volume inspect "$DATA_VOLUME"
```

Confirm this exact named volume belongs to the intended installation. Record your Compose project, `.env`, custom port/container name and any overlays. Do not proceed if the service or named volume is missing or ambiguous. For standalone Docker use its exact container and named volume instead.

Stop the app and **every provider/model helper or other writer using this volume**. Pause any external supervisor that could restart them. Closing the browser, pausing import or merely disconnecting WhatsApp is insufficient. The normal preview has only one application service; separately managed helpers must be stopped separately.

```sh
docker compose stop
docker inspect "$CONTAINER" --format '{{.State.Running}}'
RUNNING=$(docker ps -q --filter volume="$DATA_VOLUME")
test -z "$RUNNING"
```

The inspection must report `false`, and the last check must succeed. `set -eu` stops the shell on command failure or a missing variable; keep it enabled through backup/restore. Wait for complete shutdown; do not copy during cleanup. Verify that no native process or external writer also mounts this storage. The Docker check covers running containers, not arbitrary host processes.

Create a **new** private backup directory; if it already exists, choose another name. Record the exact image identity so restoration uses the same version:

```sh
umask 077
mkdir -m 700 wa-indexer-backup
docker inspect "$CONTAINER" --format '{{.Image}}' > wa-indexer-backup/image-id.txt
docker inspect "$CONTAINER" --format '{{.Config.Image}}' > wa-indexer-backup/image-reference.txt
printf '%s\n' "$DATA_VOLUME" > wa-indexer-backup/volume-name.txt
docker run --rm --network=none --read-only --cap-drop ALL --cap-add DAC_OVERRIDE \
  --mount "type=volume,src=$DATA_VOLUME,dst=/volume,readonly" \
  "$BACKUP_HELPER" tar --numeric-owner -cpf - -C /volume . \
  > wa-indexer-backup/volume.tar.partial
```

Only if that command exits successfully, verify the archive and finalize it:

```sh
docker run --rm -i --network=none --read-only --cap-drop ALL \
  "$BACKUP_HELPER" tar -tf - < wa-indexer-backup/volume.tar.partial > /dev/null
mv wa-indexer-backup/volume.tar.partial wa-indexer-backup/volume.tar
```

Do not treat a failed/partial archive as a backup. This copies the whole volume, including hidden files and both data/model directories. The helper is temporary and has no network or writable image filesystem. Its explicitly granted root capability permits reading private UID 10001 files; it does not change source ownership/modes. Protect and retain the recorded image plus launch settings with the backup; an image ID alone will not download an unpublished local image on another host.

You may now restart the original service with `docker compose up -d` (or the [source start command](../install/docker-compose.md#start-and-log-in) if recreating from source). Do not run it while validating a restored linked session.

## Docker Compose restore

Restore only a trusted archive, to a **new empty named volume**, using the same recorded application image/version. Keep the original volume separately. Stop every writer first and keep it stopped while the restored instance runs. Never merge backups or run the same WhatsApp session concurrently.

```sh
set -eu
BACKUP_HELPER=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
docker pull "$BACKUP_HELPER"
docker compose stop
RESTORE_VOLUME=wa-indexer-restored-data
docker volume create "$RESTORE_VOLUME"
RUNNING=$(docker ps -q --filter volume="$RESTORE_VOLUME")
test -z "$RUNNING"
docker run --rm -i --network=none --read-only --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
  --mount "type=volume,src=$RESTORE_VOLUME,dst=/volume" \
  "$BACKUP_HELPER" sh -c \
  'entries=$(find /volume -mindepth 1 -print -quit) && test -z "$entries" && tar --numeric-owner -xpf - -C /volume' \
  < wa-indexer-backup/volume.tar
```

`docker volume create` can reuse an existing volume: the helper **refuses a nonempty destination before extraction or metadata changes**. If it refuses, choose another unused volume name; do not empty or repair that volume. If extraction fails, retain the partial destination for diagnosis and choose a new empty volume for a retry. The temporary restore helper preserves numeric UID/GID and modes, including the private volume root, instead of recursively repairing a working archive. Do not start other writers during extraction.

Use the recorded local image ID, which must still exist on this Docker host, or the image you saved separately for restoration on another host:

```sh
WA_INDEXER_RESTORE_IMAGE=$(cat wa-indexer-backup/image-id.txt)
export WA_INDEXER_RESTORE_IMAGE
export RESTORE_VOLUME
docker image inspect "$WA_INDEXER_RESTORE_IMAGE"
cat > compose.restore.yaml <<'YAML'
services:
  wa-indexer:
    image: ${WA_INDEXER_RESTORE_IMAGE:?recorded backup image required}
volumes:
  wa_data:
    name: ${RESTORE_VOLUME:?new restored volume required}
    external: true
YAML
docker compose -f compose.yaml -f compose.restore.yaml up -d --no-build
docker compose -f compose.yaml -f compose.restore.yaml exec wa-indexer wa-indexer owner-link --data-dir /var/lib/wa-indexer/data
```

Keep this overlay and the recorded `.env` settings for subsequent commands; omitting it would select the original volume. Open a fresh 30-second owner link and verify saved choices/counts, a known retained search result, originals/model/config storage and the private client adapter. Restored grants may reintroduce old credentials; revoke or replace them deliberately. A provider may have revoked the linked session after backup, requiring pairing again. Verify any native model resources again before enabling them.

These binary-stream examples are for POSIX shells. Do not translate `>` or `<` to older Windows PowerShell: it can decode/re-encode binary tar bytes. Use a supported POSIX environment for these procedures; a Windows backup recipe has not been tested.

## Native data roots

For native development, record the build and explicit `--data-dir`; macOS defaults to `~/Library/Application Support/WA Indexer`. Stop the app, supervisors and every model/resource writer, confirm they have exited, then use a trusted backup tool to copy the complete root to a new private location preserving ownership, modes and resource metadata. Restore to a separate private root with the recorded build and verify it deliberately.

Test a restore before depending on a backup. Backups cannot recover history WhatsApp never supplied or messages discarded before selection. [Updates](updates.md) explains migration/rollback limits; [compatibility](../compatibility.md) lists the current limits.
