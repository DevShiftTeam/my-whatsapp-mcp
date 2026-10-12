---
title: My WhatsApp
description: Search your WhatsApp archive on your own computer or server.
---
# My WhatsApp

Save and search your WhatsApp conversations on your own computer or server. Link your account by QR code, choose the chats and dates to keep, then search in Hebrew or English. You can also connect an AI client with access to only the chats you allow.

## Get started

| Installation | What you need |
| --- | --- |
| [Local app](install/desktop.md) | A Mac and the tools to build from source. There is no downloadable app yet. |
| [Docker Compose](install/docker-compose.md) | Docker on an always-on computer, server, or NAS. The image is built from source. |
| [Docker](install/docker.md) | A locally built image, if you prefer to manage the container yourself. |

Docker and remote setup are in preview. Read [compatibility](compatibility.md) for current platform support and testing limits.

After installation, follow [First run](first-run.md) to choose your chats and link WhatsApp. [Start here](start-here.md) helps you choose an installation and access method.

## Use your archive

- [Search saved messages](everyday/search.md) and read the surrounding conversation.
- [Find attachments](attachments.md) by filename, caption, or nearby messages.
- [Transcribe voice notes](transcription.md) with an optional local service.
- [Connect an AI client](mcp.md), with separate permissions for each connection.
- [Open the archive on your phone](remote/phone-access.md) through optional remote access.

Keyword search needs no AI model. Semantic search, image text, and image descriptions are experimental; see [media and models](everyday/media-and-models.md).

## Your data

The archive runs on your machine. GitHub Pages hosts these guides, not your conversations or MCP server. WhatsApp only sends the history available to a linked device, so older messages and files may be missing.

The app does not encrypt its SQLite files. Use disk encryption and keep the data directory and [backups](maintenance/backups.md) private. An AI client receives the results you permit it to read. Keep owner login links, QR codes, and client tokens private.
