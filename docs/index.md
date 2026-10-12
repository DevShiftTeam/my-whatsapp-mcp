---
title: My WhatsApp
description: A private WhatsApp archive with honest installation and access choices.
---
# My WhatsApp

My WhatsApp saves a private, searchable WhatsApp archive on your computer or server. Select chats and dates, pair your own account, and search the messages actually received. Original text is kept separate from OCR, generated image descriptions, and voice transcripts.

## Choose an installation

The current options are a **native macOS ARM64 development source build** and a **Linux ARM64/AMD64 Docker source preview awaiting combined-source qualification**. There is no qualified downloadable desktop app or container release. [Start here](start-here.md) explains the choice; [compatibility](compatibility.md) records the qualification boundary.

| Path | Best fit | Current availability |
| :--- | :--- | :--- |
| [Desktop](install/desktop.md) | A Mac used locally | Development source; package planned |
| [Docker Compose](install/docker-compose.md) | An always-on host or NAS | Source-built keyword preview; combined-source qualification pending |
| [Docker](install/docker.md) | A manually managed container | Locally built preview; no release image offered |

## Decide what you want to reach

On the same computer, use the private owner browser interface and local AI-client adapter. For your phone or another personal device, [Tailscale Serve](remote/tailscale-serve.md) is an implemented source preview with [separate phone login](remote/phone-access.md); actual provider and physical-phone acceptance remain **NOT_RUN**. A hosted AI connector needs a public HTTPS MCP endpoint; compare [remote routes](remote/index.md). The existing [manual Cloudflare MCP route](clients/remote-access.md#tunnel-and-authentication) explains dedicated-listener setup.

GitHub Pages is the planned host for these guides; the public site is not deployed. Your archive and MCP server run on your own machine; the documentation site cannot receive WhatsApp history or answer archive queries.

## Make your first archive

Follow [first run](first-run.md), then [search saved messages](everyday/search.md). Keyword search works without a model. [Media and models](everyday/media-and-models.md) describes explicit activation and current native limits.

Keep private owner links, QR codes, session files and tokens out of shared reports. SQLite is not encrypted by the app; use host disk encryption and private local storage. Before changing a working installation, read [updates](maintenance/updates.md) and [backups](maintenance/backups.md).
