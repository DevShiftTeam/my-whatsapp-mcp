---
title: Start here
description: Choose a local installation and the access goal before pairing.
---
# Start here

Start with one local archive and keyword search. Native development targets macOS ARM64; Docker has a source-built keyword-search preview with local native Linux ARM64 synthetic acceptance. A downloadable app and published versioned image remain planned. Each guide records its qualification: synthetic fixtures do not prove real WhatsApp history, native AI or actual desktop clients.

## Pick where the archive lives

Choose [desktop](install/desktop.md) for a Mac you keep running. Choose [Docker Compose](install/docker-compose.md) if your host is an always-on server or NAS and you can manage Docker. Both need private local storage, space for selected media and models, and a browser for onboarding. Native Windows installation and Linux desktop packaging are unqualified.

If you already have a built executable, the desktop guide provides the demo and launch commands. Building it is a developer activity described in the repository README development section. End-user use is intended to require no Go, Node, Python, or RTK.

## Pick who needs access

| Goal | Route |
| --- | --- |
| Search on the archive computer | Private local browser; [first run](first-run.md) |
| Local Codex or Claude Code | App-managed private stdio adapter; [user guide](user-guide.md#connect-an-ai-client) |
| Browser on your enrolled phone | Implemented source preview: [private Serve route](remote/tailscale-serve.md) and [separate phone login](remote/phone-access.md) |
| Hosted AI connector | Public MCP through [Funnel](remote/tailscale-funnel.md), [ngrok](remote/ngrok.md), or [Cloudflare](remote/cloudflare.md) |

A reachable tunnel does not grant permission to read messages. Every AI client still needs its own app credential and chat/date scope. [Managed remote setup](remote/index.md) is an implemented source preview. Optional [phone access](remote/phone-access.md) uses a separate owner password and starts off. Keep the local owner interface and its private bootstrap link on the archive computer; never forward them through a tunnel. The separately supervised [manual option](clients/remote-access.md#tunnel-and-authentication) remains MCP-only. Actual provider, client and physical-phone acceptance remain **NOT_RUN**.

## Pair and check what arrived

Read [first run](first-run.md) before pairing. Select chats and dates explicitly, then scan the QR code from WhatsApp Linked devices. A connected session or history request is not proof of import. Confirm a received message and a search result before relying on the archive, and record missing history separately.

[Compatibility](compatibility.md) summarizes what remains unverified. If login, permissions, or import fail, use [troubleshooting](troubleshooting.md) instead of deleting data.
