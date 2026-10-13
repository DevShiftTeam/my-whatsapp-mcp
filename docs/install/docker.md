---
title: Docker
description: Run the locally built preview manually with the same private container settings.
---
# Docker

Prefer [Docker Compose](docker-compose.md) to keep settings together. The manual option below uses an image you build locally. First build the `wa-indexer:development` image with the Compose guide's build command, which records the source revision, then stop that installation before starting another archive instance. Docker Engine **28+** is required. Public CI for commit `401c4ba` passed synthetic native Linux ARM64 and AMD64 container checks; real-account, live-provider, and end-user client tests are still pending.

## Run the built image

```sh
docker volume create wa-indexer-standalone-data
docker run -d --name wa-indexer-standalone --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m,mode=1777 \
  -p 127.0.0.1:8123:8123 \
  --mount type=volume,src=wa-indexer-standalone-data,dst=/var/lib/wa-indexer \
  wa-indexer:development serve --container \
  --browser-origin http://127.0.0.1:8123 \
  --docker-container-name wa-indexer-standalone \
  --data-dir /var/lib/wa-indexer/data
docker exec wa-indexer-standalone wa-indexer owner-link --data-dir /var/lib/wa-indexer/data
```

Open the private returned link within **30 seconds**, or repeat owner-link. For host port 9000, change both the published mapping to `127.0.0.1:9000:8123` and browser origin to `http://127.0.0.1:9000`. Keep the internal port 8123. Owner links and browser sessions are distinct from per-client MCP grants. Do not publish the owner interface to your LAN or Internet.

The private volume includes databases, session, models/configuration and client files; the app runs as UID/GID **10001:10001**. Keep host disk encryption enabled: SQLite is not encrypted by the app. Docker access can control this private archive, so grant it only to trusted users.

## Stop and retain data

```sh
docker stop --time 10 wa-indexer-standalone
docker rm wa-indexer-standalone
```

These commands retain `wa-indexer-standalone-data`. Run the original command again to recreate the container with that same volume. For an update, take a [full stopped-writer backup](../maintenance/backups.md) first and select the intended image; do not delete the volume. The backup helper commands also work for this named volume after every writer has stopped, substituting the exact standalone volume and container. [Uninstall](../maintenance/uninstall.md) separates removal from irreversible data deletion.

This uses the same preview image as [Docker Compose](docker-compose.md#current-limits). There is no published image to pull, and real WhatsApp imports, live providers, and actual AI-client calls have not yet been confirmed for this setup.
