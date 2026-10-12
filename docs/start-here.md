---
title: Start here
description: Choose where to run your archive and how to access it.
---
# Start here

Choose where to keep your archive, then follow the installation guide. Start with keyword search; you can add voice transcription, AI clients, and remote access later.

## Choose an installation

Use the [local app](install/desktop.md) on a Mac you keep running, or [Docker Compose](install/docker-compose.md) on an always-on computer, server, or NAS. Both need a browser and enough private disk space for your selected messages and media.

You currently need to build from source. There is no downloadable desktop app or published container image. Docker builds the app for you and does not need Go, Node.js, or Python installed on the host. Native Windows installation is not supported. See [compatibility](compatibility.md) for platform and testing limits.

## Choose how to reach it

| What you want to do | Follow this guide |
| --- | --- |
| Search on the archive computer | [First run](first-run.md) |
| Connect local Codex or Claude Code | [Connect an AI client](user-guide.md#connect-an-ai-client) |
| Use a browser on your own phone | [Tailscale Serve](remote/tailscale-serve.md), then [Phone access](remote/phone-access.md) |
| Connect a hosted AI service | [Remote access](remote/index.md) |

Remote setup is in preview and has not been tested with every provider, client, or phone. Each AI client needs its own My WhatsApp credential, even when the tunnel is connected. Phone access uses a separate owner password and starts off.

Keep the local owner login link on the archive computer. For remote access, use the app's remote setup rather than forwarding the owner port.

## Link WhatsApp and check your messages

Follow [First run](first-run.md) to select chats and dates before pairing. Search for a message you recognize and check the received count and date range. A connected session alone does not mean your history has arrived.

## Before changing an existing installation

Read [Backups](maintenance/backups.md) and [Updates](maintenance/updates.md). Keep the same data directory or Docker volume. Changing archive choices can delete saved messages; search filters only change what you see.
