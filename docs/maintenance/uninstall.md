---
title: Uninstall
description: Stop or remove the service first; delete private data only deliberately.
---
# Uninstall

Decide whether to retain the private archive. Take a [complete stopped-writer backup](backups.md) before retiring a working installation.

## Docker: stop or remove while keeping data

In the same Compose checkout/project used to start it:

```sh
docker compose stop
docker compose down
```

Both retain the named data volume. `stop` retains containers; `down` removes service containers/network. Neither logs you out of WhatsApp on the phone. Never add volume-deletion flags or use daemon pruning as an ordinary uninstall/update step.

For [standalone Docker](../install/docker.md), stop then remove the exact container with `docker stop --time 10 wa-indexer-standalone` and `docker rm wa-indexer-standalone`. Its named volume remains.

## Explicit Docker data deletion

**Irreversible:** this removes the chosen volume's archive, WhatsApp session, originals, model/config storage and client grants. It does not delete WhatsApp messages or backups. While the container still exists, discover its exact volume:

```sh
set -eu
CONTAINER=$(docker compose ps -a -q wa-indexer)
test -n "$CONTAINER"
DATA_VOLUME=$(docker inspect "$CONTAINER" --format '{{range .Mounts}}{{if eq .Destination "/var/lib/wa-indexer"}}{{.Name}}{{end}}{{end}}')
test -n "$DATA_VOLUME"
printf '%s\n' "$DATA_VOLUME"
docker volume inspect "$DATA_VOLUME"
```

Check that this is the project-owned volume you intend to erase; a restored/external volume may have another name. Retire/revoke client grants and linked-device access first if needed. Then stop every writer, remove the app containers and deliberately delete only that exact volume:

```sh
docker compose stop
RUNNING=$(docker ps -q --filter volume="$DATA_VOLUME")
test -z "$RUNNING"
docker compose down
docker volume rm "$DATA_VOLUME"
```

If any command fails, stop and investigate instead of forcing deletion. Do not run this deletion block when keeping the archive.

## Native installation and remote access

For a development executable, log out in WhatsApp's **Linked devices** if retiring the session, stop the app gracefully, and remove only its intended executable, client configuration and service/autostart registration. There is no packaged desktop installer yet.

Native owner-interface deletion is separately available through **Archive settings → Delete all local data**, typing `DELETE LOCAL DATA`. It removes the local archive/session/models/client material, not WhatsApp messages. Do not use deletion to repair history or reconciliation warnings.

Revoke app credentials, stop provider forwarding, and remove the exact tunnel/domain/route in the provider account separately; see [remote access](../remote/index.md). Removing a client configuration alone does not revoke its server grant. Deleting active data does not erase backups or results already sent to AI services; encrypted storage and your operating system's disposal process govern residual copies.
