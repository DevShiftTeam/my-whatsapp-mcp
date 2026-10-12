---
title: Compatibility
description: A sanitized qualification summary with explicit unverified boundaries.
---
# Compatibility

This page summarizes public qualification as of **2026-10-12**. Every guide starts at `documentation_reviewed`; no provider, client or runtime test is invented. Documentation review establishes instructions and links, not installation or live execution. The per-page status record is also included in copied/downloaded Markdown and the combined bundle.

## Installation and platform

| Area | Availability | Evidence boundary |
| --- | --- | --- |
| [macOS ARM64 desktop](install/desktop.md) | Development source | No downloadable qualified `.app`; clean-machine installation unverified |
| macOS AMD64 / native Windows / Linux desktop | Unqualified | No advertised native package |
| [Docker Compose](install/docker-compose.md) and [Docker](install/docker.md) | Source-built preview | Combined-source integration qualification pending; no published image; native ARM64 and AMD64 acceptance pending |
| GitHub Pages | Planned documentation hosting | Does not run the archive or MCP server |

## Archive and local AI

| Area | Current behavior or boundary |
| --- | --- |
| Keyword search | Implemented; Hebrew/English and keyword fallback |
| Pairing, history and restart | Implemented workflow; real-account import, completeness and reconciliation unverified |
| Native AI | No qualified bundle; accepted profile allowlist empty |
| Memory and quality | FP32 memory and experimental INT8 numerical gates remain open; 8 GiB and Hebrew/media quality unverified |
| Backup and restore | [Manual full stopped-writer procedure](maintenance/backups.md); combined-source volume restore qualification pending; real-account restore remains unverified |

## Remote routes and clients

| Route or client | Qualification boundary |
| --- | --- |
| [Serve](remote/tailscale-serve.md), [Funnel](remote/tailscale-funnel.md), [ngrok](remote/ngrok.md) | Managed source previews implemented; fresh provider/client/device acceptance NOT_RUN |
| [Cloudflare](remote/cloudflare.md) | Managed helper and manual MCP-only instructions; dashboard route and provider acceptance NOT_RUN |
| [Phone access](remote/phone-access.md) | Managed password/session source integration; physical phone and live provider journey NOT_RUN |
| [Codex](clients/codex.md) / [Claude Code](clients/claude-code.md) | Configuration mechanisms documented; exact CLI/backend/UI tool calls unverified |
| [Claude connectors](clients/claude-connectors.md) / [Composio](clients/composio.md) | Account eligibility, hosted authentication and outbound headers need actual tests |

## What changes a status

`locally_tested` requires a passing record tied to an immutable build SHA, platform, architecture, fixture and date. `end_to_end_verified` additionally requires passing provider and exact-client version records. A fixture, parsed configuration, tool list, SDK ACTIVE state, healthy listener or browser-visible tunnel cannot replace the complete journey.

Public records exclude private paths, credentials, real message content and owner links. Read [first-run coverage](first-run.md#check-observed-coverage) and [client verification](clients/remote-access.md#acceptance-test) for the practical proof boundary.

## Combined-source qualification

The additive integration preserves the main archive, attachment, send, Stats and transcription implementations. Its source, module, handler and image bytes differ from the earlier setup candidate. All per-page records therefore remain `documentation_reviewed` with no current runtime PASS records. New image IDs, binary hashes, browser/SDK checks and native architecture results must be tied to the exact integrated revision before updating this page. Native AMD64 requires an actual hosted native run; local ARM64 is insufficient.

Synthetic demo tests prove catalog exposure, scope and send denial; they cannot prove WhatsApp delivery. Authorized fake-connector tests exercise send state transitions and larger payloads separately. Real provider/client/phone acceptance, real WhatsApp history, native AI and recording quality remain separate gates. GitHub Pages hosts only documentation.
