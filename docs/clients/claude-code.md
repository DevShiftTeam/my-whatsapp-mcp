---
title: Claude Code
description: Keep executable arguments literal and remote authorization explicit.
---
# Claude Code

Status: **Not yet verified** for an actual My WhatsApp connection. Installed version checked 2026-10-06: **2.1.291**. The generated JSON uses Claude Code's transport names and explicit header format; a parsed JSON file alone does not prove native client compatibility.

## Local stdio

Choose a local connection and its chat/date limits in My WhatsApp, then copy the generated Claude Code configuration. The app creates a separate private credential file and the adapter connects to the running app through a **private Unix socket**. No port selection, token copying or token environment variable is needed.

My WhatsApp manages the version 2 credential file as a regular **0600** file under its private **0700** directory. Copy only the generated path in the client configuration and keep the file contents private.

Save the generated client JSON as an explicitly selected file, for example `wa-indexer.mcp.json`:

The credential-file path must be absolute and already clean: paths containing `.` or `..` components, repeated separators, or a trailing separator are rejected by the adapter and this guide. Use the exact persisted file path; the renderer does not rewrite it.

```json
{
  "mcpServers": {
    "wa_indexer": {
      "type": "stdio",
      "command": "/absolute/path/to/wa-indexer",
      "args": ["mcp", "stdio", "--config", "/absolute/private/path/native-client.json"]
    }
  }
}
```

The absolute executable and each argument remain separate, including paths with spaces, apostrophes and Hebrew characters. Claude Code expands `${...}` in commands and arguments, so this renderer refuses literal executable/config paths containing that marker; choose paths without it. The adapter forwards to the running service; it does not pair WhatsApp, own an archive or start inference.

## Docker stdio

For the source-built [Docker preview](../install/docker-compose.md), create the same scoped local connection in the owner UI and copy its generated configuration. The host client must have Docker access and the service must already be running. Docker access itself is powerful; restrict it to trusted host users. The native socket and 0600 credential file stay inside the private container volume. The host needs neither the app executable nor a copied token.

```json
{
  "mcpServers": {
    "wa_indexer": {
      "type": "stdio",
      "command": "docker",
      "args": ["exec", "-i", "wa-indexer", "/usr/local/bin/wa-indexer", "mcp", "stdio", "--config", "/var/lib/wa-indexer/data/clients/native-client.json"]
    }
  }
}
```

The private file path above is a shape example: **use the exact generated path**, not that placeholder filename. Use your configured container name if different from `wa-indexer`. Keep `-i` and do not add `-t`: MCP stdio is a pipe, not a terminal. The adapter does not start another archive writer or pair WhatsApp. The combined-source scoped Docker bridge awaits fresh SDK qualification; the exact Codex/Claude application remains **Not yet verified**. Complete the client-specific tests below before relying on it.

## Remote bearer

For remote access, use the app-configured public bearer-only HTTPS endpoint and an independent per-client token. This is Streamable HTTP and requires the running app and tunnel:

```json
{
  "mcpServers": {
    "wa_indexer": {
      "type": "http",
      "url": "https://bearer.example.com/mcp",
      "headers": {"Authorization": "Bearer ${WA_INDEXER_CLIENT_TOKEN}"}
    }
  }
}
```

Provide the **raw** My WhatsApp token in the dedicated variable of this client's process. Claude Code expands `${WA_INDEXER_CLIENT_TOKEN}`; the JSON explicitly supplies the `Bearer ` prefix. Do not reuse `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or cloud-account credentials. `AWS_BEARER_TOKEN_BEDROCK`, `HTTPS_PROXY` and `NPM_TOKEN` also expand to empty in remote headers and are rejected as bearer variables. Do not omit `type`: a URL without an HTTP type is not a valid remote entry.

Load the selected JSON with:

```sh
claude --mcp-config './wa-indexer.mcp.json' --strict-mcp-config
```

For acceptance testing, use a separate task-owned configuration and synthetic data. Connection status alone does not prove that a tool call succeeded. Claude Code has no verified direct no-turn command for calling these downstream tools. A local Messages API stub would drive a synthetic client turn and needs separate review; that route has not run.

## OAuth

Select OAuth only after the deployed authorization route has passed real compatibility checks. Use its **separate OAuth hostname** with `type: "http"` and `url`, without a static Authorization header. Complete the exact Claude Code consent flow and verify its loopback callback, registration method and refresh behavior. Hosted Claude callback settings are not interchangeable with Claude Code's loopback flow.

## Test and revoke

From the exact installed native client, list tools and execute `search_messages`, `get_context` using the returned ID, `list_chats`, and `get_index_status` against a synthetic permitted source. Confirm excluded chats/dates stay absent, then narrow the grant and repeat the previous context lookup.

Revoke the exact credential ID shown in the generated guide. Every tool must deny access through the old connection before its JSON/registration is removed. For OAuth, also revoke access/refresh credentials and verify refresh denial. A server list or successful OAuth login alone is insufficient.

Sources checked 2026-10-06: [Claude Code MCP reference](https://code.claude.com/docs/en/mcp), [CLI reference](https://code.claude.com/docs/en/cli-reference), [environment reference](https://code.claude.com/docs/en/env-vars). See [remote access](remote-access.md).

Documentation reviewed 2026-10-12 for public instructions and links; historical installed-version/source checks above retain their original dates. No new client runtime test is claimed. See [compatibility](../compatibility.md) and [remote route choices](../remote/index.md). Repository-source examples may require access while the repository remains private.

## Managed saved endpoints

The local **Remote access** wizard now configures the [implemented provider previews](../remote/index.md). Copy its saved HTTPS endpoint ending in `/mcp`, then create the separate scoped remote credential in **AI connections**. Provider login and a browser owner password do not authenticate this client. Keep browser exposure off unless you deliberately enable [Phone access](../remote/phone-access.md).

A hosted connector requires public Funnel/ngrok/Cloudflare reachability; private Tailscale works only when the client itself runs on an enrolled device. The provider flow supplies no OAuth authorization server. Account-dependent header/OAuth combinations retain their unverified status. Actual client/version twelve-tool, scope, expiry, old-context and revocation checks remain **NOT_RUN**. Reconnect/restart must retain the saved URL; migrating accounts or domains requires updating this client deliberately.

## Full application catalog

The combined application advertises twelve tools: eight archive reads, `send_message`, `send_media`, `send_voice_note`, and read-only `get_send_status`. Verify contact/group and attachment scope alongside message/context reads; mutation annotations do not grant sending rights. New connections start read-only. Check default-denied sends with synthetic data; real delivery requires a separately authorized connected account. Preserve request IDs when status is uncertain. See [MCP access](../mcp.md), [attachments](../attachments.md), and [outbound messaging](outbound-messaging.md).
