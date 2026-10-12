---
title: Compatibility
description: Installation options, platform support, and current limitations.
---
# Compatibility

My WhatsApp is available as source code. Docker and managed remote access are in preview. There is no downloadable desktop app or published container image yet.

## Installation

| Option | Current support |
| --- | --- |
| [Local app](install/desktop.md) | Build from source on macOS ARM64. No signed installer or clean-machine installation test yet. |
| [Docker Compose](install/docker-compose.md) or [Docker](install/docker.md) | Build the image from source. Linux ARM64 and AMD64 are configured, but full installation tests for the current version are still pending. |
| Native Windows | Not supported. |
| Other native platforms | No packaged installation or confirmed desktop support. |
| Documentation website | Hosted on GitHub Pages. The archive runs on your own machine. |

## Search, history, and media

| Feature | What to expect |
| --- | --- |
| Keyword search | Hebrew and English search over messages received and retained by your archive. No model required. |
| WhatsApp history | Limited to what WhatsApp sends to a linked device. Older messages, edits, deletions, and media may be missing. |
| Attachments | Search filenames, types, captions, and nearby messages. Document contents are not indexed. |
| Voice transcripts | Optional local faster-whisper service. Accuracy and speed depend on the recording, language, model, and hardware. |
| Semantic search, OCR, and image descriptions | Experimental. A supported native bundle and default model profile are not available yet. |
| Memory use | No confirmed 8 GiB requirement or published archive-scale benchmark. Native model memory and output checks are still incomplete. |
| [Backup and restore](maintenance/backups.md) | Manual backup of the complete stopped archive. Restore testing for the current Docker setup and real WhatsApp accounts is still pending. |

## Remote access and AI clients

| Connection | Current limitation |
| --- | --- |
| [Tailscale Serve](remote/tailscale-serve.md), [Funnel](remote/tailscale-funnel.md), [ngrok](remote/ngrok.md) | Setup is included, but a complete connection through a live provider and AI client has not been confirmed. |
| [Cloudflare Tunnel](remote/cloudflare.md) | You must configure the hostname route in Cloudflare. Starting the helper does not confirm that the public route works. |
| [Phone access](remote/phone-access.md) | Password-protected browser access is included. Testing on a physical phone through a live provider is still pending. |
| [Codex](clients/codex.md) and [Claude Code](clients/claude-code.md) | Configuration examples are provided. Successful tool calls in the actual client applications have not yet been confirmed. |
| [Claude remote connectors](clients/claude-connectors.md) | Requires an account that supports custom request headers. The managed setup does not issue Claude connector credentials. |
| [Composio](clients/composio.md) | Uses the experimental Custom MCP API. Registration and authenticated tool calls still need testing in an actual Composio project. |
| OAuth | Not supplied by the remote setup. Use a supported bearer-token connection. |

The setup instructions have been reviewed. Complete installation, real-account import and restore, live-provider connections, and actual AI-client calls remain untested for these setup options. Tests using sample data cannot establish WhatsApp delivery, complete history, or recognition quality.

After connecting, [check access](clients/remote-access.md#check-your-connection) with a harmless query and confirm that excluded chats stay hidden. Keep new connections read-only until you need [sending](clients/outbound-messaging.md).
