---
title: Docker Compose
description: Start the source-built keyword-search preview with private persistent storage.
---
# Docker Compose

You need **Git**, Docker Engine **28+**, Compose **2.20.2+**, and a POSIX shell. Docker builds the app from source; you do not need Go, Node.js, or Python on the host. There is no published container image yet. Full installation and real WhatsApp import tests for this setup are still pending; see [compatibility](../compatibility.md).

## Start and log in

Clone the public repository, then build and start the service:

```sh
git clone https://github.com/DevShiftTeam/my-whatsapp-mcp.git
cd my-whatsapp-mcp
docker compose -f compose.yaml -f compose.build.yaml up -d --build
docker compose ps
docker compose exec wa-indexer wa-indexer owner-link --data-dir /var/lib/wa-indexer/data
```

Open the returned private link on this computer. It expires in **30 seconds**; repeat the owner-link command whenever you need a fresh login. The normal browser address is `http://127.0.0.1:8123`. Login is separate from pairing WhatsApp. The service becomes ready before pairing, history synchronization or model setup. Routine logs contain no owner capability.

The source build downloads its pinned build dependencies and compiles inside Docker. Subsequent application startup performs no installation, compilation or model download. Keep the source checkout accessible only to people you trust to operate this archive. Use the source-build command above; a release image is not available to pull.

## Choose another host port

Keep the selected values in the checkout's `.env` file so later login, restart and update commands use the same settings:

```dotenv
WA_INDEXER_PORT=9000
WA_INDEXER_CONTAINER_NAME=wa-indexer
```

Run the same start command again, then repeat owner-link. Its URL will use `http://127.0.0.1:9000`. The container still listens internally on 8123; the exact browser Host/Origin checks use the selected host port. The mapping binds explicitly to **127.0.0.1**, not your LAN. Use the literal address in the returned link. A custom container name must also be retained in generated client configurations.

## Storage and stopping

Compose uses one application service, SQLite and the named `wa_data` volume mounted at `/var/lib/wa-indexer`. Its actual Docker name is normally `wa-indexer_wa_data`; use the [backup procedure](../maintenance/backups.md#docker-compose-backup) to discover the exact mounted volume instead of guessing. New volumes receive private UID/GID **10001:10001** directories; the app checks private ownership/modes and refuses unsuitable existing roots. It does not repair arbitrary existing storage recursively.

```sh
docker compose stop
docker compose down
```

`stop` retains the container and data. `down` removes the container/network and **retains the named volume**. Restart with the source start command above. Read [updates](../maintenance/updates.md), [backups and restore](../maintenance/backups.md), and [explicit removal](../maintenance/uninstall.md) before maintenance. Do not add volume-removal flags to routine commands.

## Continue privately

Follow [First run](../first-run.md), then confirm messages actually received and search a known phrase. Connection or a history request does not establish import or completeness. For a local AI client, the generated [Codex](../clients/codex.md#docker-stdio) or [Claude Code](../clients/claude-code.md#docker-stdio) adapter runs through `docker exec -i`; its credentials stay inside the private volume. After connecting, try a harmless search in a permitted chat.

For another device or a hosted client, review [remote access](../remote/index.md). Do not expose the owner port as a substitute for a scoped MCP route. GitHub Pages hosts these guides; the app runs on your machine.

## Current limits

This setup is in preview. Linux ARM64 and AMD64 installation, actual AI-client calls, and live-provider connections still need full testing. WhatsApp history may be incomplete; check the received messages and date range after pairing.
