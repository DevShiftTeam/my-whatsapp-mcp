---
title: Troubleshooting
description: Diagnose common failures without exposing private state or deleting the archive.
---
# Troubleshooting

Begin with the observed symptom and your exact build/platform. Keep message bodies, QR content, owner links, tokens and session files out of reports. Current [qualification limits](compatibility.md) explain which flows still need real acceptance.

## Browser or owner login fails

Keep the app running and use its current private owner link on the archive computer. After restart an old tab/link may be stale; reopen the app-managed link. If automatic browser opening failed, request a fresh private `owner-link` locally. For Compose, run `docker compose exec wa-indexer wa-indexer owner-link --data-dir /var/lib/wa-indexer/data`; the link expires in 30 seconds and can be repeated. Never paste that capability into an issue or tunnel the owner port. A remote phone login is planned, not established by today's local interface.

## Listener cannot start or an instance already exists

The owner listener requires a literal loopback address. Remote MCP flags must be provided together: a fixed dedicated loopback listener plus the exact HTTPS `/mcp` public URL. Preserve your usual data root and restart to change route settings; a second launch does not reconfigure the existing process. If a port is busy, identify its owner rather than terminating an unrelated process. Do not remove locks or ownership markers to bypass an active writer.

## Data permissions are rejected

Use a private local data root owned by the app user. The app validates ownership, directory/file protections and symlinks; immutable model resources have their own approved protections. Shared, cloud-synced or manually copied roots can violate these rules. Stop writers and restore permissions/ownership from a trusted full backup rather than making the root world-readable. Do not edit database files or marker contents.

## WhatsApp connects but messages are missing

Check saved chat/date policy, whether selective mode has any selected chats, received counts and pause state. A connected session or backfill request does not prove import. Confirm a stored message and search; record the visible coverage. WhatsApp may not supply earlier history, and changing filters cannot restore previously discarded bodies. Request a new QR code if linking expires.

## Media or AI is unavailable

Check original-media storage usage and available disk space. Lowering the limit does not immediately remove retained originals. Downloading and verifying a pinned resource do not activate AI. Use only eligible offered profiles; the current accepted native allowlist is empty. Keyword fallback remains useful. [Media and models](everyday/media-and-models.md) explains the limits.

## Remote client fails

Check host/app/provider uptime, exact saved hostname, dedicated `/mcp` origin, expiry and per-client scope. For 401/403, use the client's protected credential setting and correct raw-token/Bearer formatting. Serve is private to enrolled devices; hosted connectors need public access. For ngrok quotas check the dashboard; for Funnel check HTTPS/permissions; for Cloudflare check ingress and exact Host. Never rewrite Origin to bypass validation.

## Needs attention or reconciliation

Safe reads may remain blocked until recovery. Keep private state intact and use the offline diagnostic with the app stopped and the same data root. For an already-built development executable:

```sh
bin/wa-indexer doctor
```

Add your normal `--data-dir` if customized. Share only sanitized codes/status. Doctor is not an installation, native-quality or history-completeness test; do not use **Delete all local data** as a recovery shortcut.
