---
title: Codex CLI and desktop
description: Configure the private stdio adapter or scoped remote bearer connection.
---
# Codex CLI and desktop

Status: **Not yet verified** for an actual My WhatsApp client connection. Checked 2026-10-06: PATH Codex CLI **0.149.1**; desktop runtime **26.930.51102**, build **13100**, with bundled CLI **0.160.0**. Help/schema checks establish available mechanisms, not successful My WhatsApp calls. Test CLI, bundled backend and desktop UI separately.

## Local stdio

In My WhatsApp, choose a local connection and the chats/dates that this client may read. Copy the configuration generated for that connection. My WhatsApp creates its private credential file automatically and the adapter connects to the running app through a **private Unix socket**. You do not need to choose a port, edit an endpoint, copy the file contents or set a token environment variable.

The generated credential file is a regular **0600** file in My WhatsApp's private **0700** directory. Its version 2 contents bind this connection to the app-owned socket; leave those contents private and let My WhatsApp manage them. The path shown in the client configuration is the only handoff you copy.

Copy the generated stanza into the explicitly selected Codex host's configuration:

The credential-file path must be absolute and already clean: paths containing `.` or `..` components, repeated separators, or a trailing separator are rejected by the adapter and this guide. Use the exact persisted file path; the renderer does not rewrite it.

```toml
[mcp_servers.wa_indexer]
command = "/absolute/path/to/wa-indexer"
args = ["mcp", "stdio", "--config", "/absolute/private/path/native-client.json"]
```

The adapter invocation is exactly:

```sh
'/absolute/path/to/wa-indexer' mcp stdio --config '/absolute/private/path/native-client.json'
```

Use generated strings for paths with spaces, apostrophes or Hebrew characters. The executable and arguments are separate configuration values. Never paste the native token into Codex settings, an argument or a preview. The adapter connects to the existing service; it must not start pairing, a second archive writer or a model worker.

## Docker stdio

For the source-built [Docker preview](../install/docker-compose.md), create the same scoped local connection in the owner UI and copy its generated configuration. The host client must have Docker access and the service must already be running. Docker access itself is powerful; restrict it to trusted host users. The native socket and 0600 credential file stay inside the private container volume. The host needs neither the app executable nor a copied token.

```toml
[mcp_servers.wa_indexer]
command = "docker"
args = ["exec", "-i", "wa-indexer", "/usr/local/bin/wa-indexer", "mcp", "stdio", "--config", "/var/lib/wa-indexer/data/clients/native-client.json"]
```

The private file path above is a shape example: **use the exact generated path**, not that placeholder filename. Use your configured container name if different from `wa-indexer`. Keep `-i` and do not add `-t`: MCP stdio is a pipe, not a terminal. The adapter does not start another archive writer or pair WhatsApp. The combined-source scoped Docker bridge awaits fresh SDK qualification; the exact Codex/Claude application remains **Not yet verified**. Complete the client-specific tests below before relying on it.

## Remote bearer

For remote access, use the app-configured public **bearer-only** HTTPS endpoint, with a distinct per-client credential restricted to the chosen chats/dates. This uses Streamable HTTP and requires the running app and tunnel:

```toml
[mcp_servers.wa_indexer]
url = "https://bearer.example.com/mcp"
bearer_token_env_var = "WA_INDEXER_CLIENT_TOKEN"
```

The variable contains the **raw** My WhatsApp token. Codex adds the Bearer scheme. It must be present in the actual client process. Do not use an OpenAI account key or put the token in the URL. The CLI's documented registration shape is:

```sh
codex mcp add wa_indexer --url 'https://bearer.example.com/mcp' --bearer-token-env-var 'WA_INDEXER_CLIENT_TOKEN'
```

For validation, use task-owned configuration and a structured child environment. Do not run that registration command against the owner's global settings. Desktop environment inheritance needs its own test; an export in an unrelated terminal does not prove that the desktop process has the variable. Prefer stdio until that desktop route passes.

## OAuth

Only select OAuth when the **actual runtime route** has passed discovery, registration, PKCE, callback, refresh and policy checks. Use its separate OAuth hostname, an HTTP stanza containing only `url`, and the client's verified login workflow. A configured route or signed fixture does not make OAuth available. Do not inject a static bearer into the OAuth configuration.

## Test and revoke

In the selected exact client, list the twelve tools (eight archive reads, three send mutations and send-status read). Search a synthetic permitted phrase with `search_messages`; pass the returned message ID to `get_context`; call `list_chats` and `get_index_status`. Confirm excluded chats/dates are absent. Narrow the grant and retry the prior context ID; the old access must disappear.

Revoke the guide's exact credential ID in My WhatsApp's connection controls. Keep the old connection long enough to repeat **all twelve tools** and verify denial, then remove its client registration. Removing a client stanza alone does not revoke the server credential.

Codex's native app-server protocol permits an isolated ephemeral thread and direct `mcpServer/tool/call` without a model turn. That planned backend smoke must not attach the desktop's existing daemon or saved chats. A backend result still does not prove desktop UI support.

Sources checked 2026-10-06: [OpenAI MCP documentation](https://learn.chatgpt.com/docs/extend/mcp?surface=cli), [configuration reference](https://developers.openai.com/codex/config-reference), [app-server protocol](https://developers.openai.com/codex/app-server). See [remote access](remote-access.md) for availability, authentication and data-transit requirements.

Documentation reviewed 2026-10-12 for public instructions and links; historical installed-version/source checks above retain their original dates. No new client runtime test is claimed. See [compatibility](../compatibility.md) and [remote route choices](../remote/index.md). Repository-source examples may require access while the repository remains private.

## Managed saved endpoints

The local **Remote access** wizard now configures the [implemented provider previews](../remote/index.md). Copy its saved HTTPS endpoint ending in `/mcp`, then create the separate scoped remote credential in **AI connections**. Provider login and a browser owner password do not authenticate this client. Keep browser exposure off unless you deliberately enable [Phone access](../remote/phone-access.md).

A hosted connector requires public Funnel/ngrok/Cloudflare reachability; private Tailscale works only when the client itself runs on an enrolled device. The provider flow supplies no OAuth authorization server. Account-dependent header/OAuth combinations retain their unverified status. Actual client/version twelve-tool, scope, expiry, old-context and revocation checks remain **NOT_RUN**. Reconnect/restart must retain the saved URL; migrating accounts or domains requires updating this client deliberately.

## Full application catalog

The combined application advertises twelve tools: eight archive reads, `send_message`, `send_media`, `send_voice_note`, and read-only `get_send_status`. Verify contact/group and attachment scope alongside message/context reads; mutation annotations do not grant sending rights. New connections start read-only. Check default-denied sends with synthetic data; real delivery requires a separately authorized connected account. Preserve request IDs when status is uncertain. See [MCP access](../mcp.md), [attachments](../attachments.md), and [outbound messaging](outbound-messaging.md).
