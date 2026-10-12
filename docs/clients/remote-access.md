---
title: Manual remote MCP setup
description: Configure a named MCP-only tunnel with private owner access and scoped clients.
---
# Remote access setup

Status: **Not yet verified** for hosted client/account/OAuth combinations. These setup guides are inert configuration instructions. They do not create a tunnel, register an AI client, authenticate an account or publish the archive.

For managed persistent access, use the local **Remote access** wizard and [provider guides](../remote/index.md). The manual MCP-only procedure below is an alternative for a native POSIX executable and a separately supervised helper on the same host. It must replay the same flags each launch, does not enable the managed wizard or remote browser, and is not a Docker host-to-container loopback recipe.

## Choose one mode and the intended client

For local mode, choose your chat/date limits in My WhatsApp and copy the generated client configuration. It runs `wa-indexer mcp stdio --config <generated-private-file-path>` and connects to the running app through a **private Unix socket**. My WhatsApp creates and manages the version 2 credential file as a regular **0600** file in its **0700** directory. No port, secret copying or token environment variable is needed. Local credentials work only through the local adapter; remote clients receive separate credentials.

The app manages the local socket and file path. Copy the generated client settings as shown; connection guides only provide instructions.

Remote mode uses authenticated Streamable HTTP at the dedicated `/mcp` listener. Each bearer client gets its own token and chat/date restrictions. Those restrictions are enforced by the server on search, context, listings, status and cached results, not merely included as client filters.

Use only the selected mode's configuration. The renderer rejects mixed local/remote fields, malformed or credential-bearing URLs, and identical bearer/OAuth hostnames. It requires HTTPS and `/mcp`, rejects userinfo, query strings, fragments and literal private/loopback addresses, rejects numeric IPv4 aliases such as `127.1` or `0x7f.0.0.1`, and normalizes hostname case/default port. DNS reachability still needs an actual remote test.

## Client access and licensing

Use the AI client installation and account you are licensed to use. Copying a connection guide does not install or license that client, sign you in, or enable a paid model service. Local transport keeps the adapter handoff on this computer; results returned to an AI app are available to that app. Remote results also pass through the selected tunnel provider.

## Tunnel and authentication

This manual setup publishes the dedicated bearer MCP listener. The tunnel provides HTTPS reachability; My WhatsApp enforces each client's token and chat/date limits. Keep the computer, app and tunnel running while remote clients connect. Results pass through Cloudflare and the receiving AI service.

### 1. Start the dedicated MCP listener

Choose a hostname on a domain you control, such as `wa-mcp.example.com`. Stop the existing app first. These commands assume an already-built development executable at `bin/wa-indexer` in the source checkout. Stop its existing process, then launch with both flags:

```sh
bin/wa-indexer serve \
  --mcp-listen 127.0.0.1:8124 \
  --mcp-public-url https://wa-mcp.example.com/mcp
```

Use your own hostname and preserve your usual `--data-dir` if applicable. These flags must be supplied together; changing a running configuration requires restart. Port **8124** is this example's dedicated MCP port. **Never use the private owner interface or its address as the tunnel origin.** No qualified packaged executable is available; see [desktop availability](../install/desktop.md).

### 2. Install cloudflared and create your own named tunnel

Your domain must already use Cloudflare DNS. On macOS with Homebrew installed:

```sh
brew install cloudflared
cloudflared --version
cloudflared tunnel login
cloudflared tunnel create wa-indexer-mcp
```

Login opens your browser: sign in to your own Cloudflare account and select your domain. Record the new tunnel UUID and credentials-file path. Keep that file and the login certificate private. For other operating systems, use [Cloudflare's official downloads](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/downloads/). These are manual account and tunnel operations you perform. [Official named-tunnel steps](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/local-management/create-local-tunnel/).

### 3. Save the tunnel configuration

Copy [the reviewed repository-source example configuration](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/docs/clients/examples/cloudflared.yml) to a new file you control, for example `/Users/YOU/.cloudflared/wa-indexer-mcp.yml`. Replace the UUID, credentials path and hostname; use the dedicated MCP port from step 1:

```yaml
tunnel: REPLACE_TUNNEL_UUID
credentials-file: '/Users/YOU/.cloudflared/REPLACE_TUNNEL_UUID.json'
ingress:
  - hostname: wa-mcp.example.com
    path: '^(/mcp(/attachments/[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048])?|/api/send/(message|media|voice|status))$'
    service: http://127.0.0.1:8124
    originRequest:
      httpHostHeader: wa-mcp.example.com
  - service: http_status:404
```

`httpHostHeader` must be the exact public authority configured in My WhatsApp, normally `wa-mcp.example.com` for HTTPS port 443. It sets the Host header sent to the local MCP service. Keep client Origin headers unchanged; do not rewrite them to bypass validation. [Cloudflare origin parameters](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/origin-parameters/).

The anchored path rule publishes `/mcp`, canonical `/mcp/attachments/<ticket>` downloads and exactly `/api/send/message`, `/api/send/media`, `/api/send/voice` and `/api/send/status`. These JSON routes retain client bearer and separate sending permission checks; owner cookies grant no authority. The final rule returns 404 for other paths/hostnames. Cloudflare forwards the matching path intact. Keep the example's literal `127.0.0.1` service; no owner/pairing/admin route belongs here. [Cloudflare ingress configuration](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/local-management/configuration-file/).

### 4. Validate, route and run

Replace `YOU` in the file path before running:

```sh
cloudflared tunnel --config '/Users/YOU/.cloudflared/wa-indexer-mcp.yml' ingress validate
cloudflared tunnel --config '/Users/YOU/.cloudflared/wa-indexer-mcp.yml' ingress rule https://wa-mcp.example.com/mcp
cloudflared tunnel route dns wa-indexer-mcp wa-mcp.example.com
cloudflared tunnel --config '/Users/YOU/.cloudflared/wa-indexer-mcp.yml' run wa-indexer-mcp
```

The DNS command creates the public hostname route; the last command keeps the tunnel running in this terminal. Stop it with Ctrl-C. Stopping the process leaves its account/DNS configuration in place; remove your route/tunnel in Cloudflare when retiring it. An ingress check verifies routing rules, not authenticated MCP calls.

### 5. Create a remote client credential

In the private My WhatsApp owner interface, open **AI connections**, create the selected remote connection with explicit chat/date limits and expiry, then use **Reveal remote token → Copy token and close**; copying closes the reveal view automatically. Follow the [Codex](codex.md), [Claude Code](claude-code.md), [Claude connectors](claude-connectors.md), or [Composio](composio.md) guide. Use `https://wa-mcp.example.com/mcp` as the client URL and a separate credential for each client. Tokens do not belong in the tunnel YAML.

This guide configures bearer authentication. OAuth compatibility needs a separately verified runtime flow; this tunnel setup does not establish it. Hosted Claude request-header support also depends on the owner's account capability. Keep those combinations **Not yet verified** until real calls pass.

## Credentials and previews

Never place tokens in URLs, argv, logs or copied diagnostic text. Codex bearer mode reads a dedicated environment variable; Claude Code explicitly adds `Bearer ` in its header template. Eligible Claude cloud accounts enter the full literal header value. Composio uses the explicit `Authorization: Bearer {{generic_api_key}}` mapping. Provider/project credentials are separate from My WhatsApp tokens.

Generated guides and setup diagnostics redact raw tokens. Diagnostic JSON omits all endpoint URL fields, including malformed URLs before validation. Native configuration references only the credential file's path, whose contents must remain private. Token creation/persistence, one-time presentation and scoped revocation belong to the credential owner, not this renderer.

## Acceptance test

A successful initializer, tool list, login, JSON parse or SDK test alone is insufficient. The exact intended client/version must verify the exact twelve-tool catalog: `search_messages`, `get_context`, `list_chats`, `get_index_status`, `search_contacts`, `search_groups`, `search_attachments`, `get_attachment`, `send_message`, `send_media`, `send_voice_note`, and `get_send_status`. Use synthetic read fixtures and verify default-denied send calls; authorized fake sending is a separate state-machine check. Use only synthetic sources for isolated development checks. Confirm excluded chats/dates stay absent, narrow the policy and retry a previous context ID, then revoke and repeat every tool using the old connection. Also prove denial of anonymous, wrong and expired credentials. Preserve the same HTTPS URL through reconnect, app restart and container recreation with the same volume. Provider revocation must produce an actionable failure while local keyword search continues.

For OAuth, additionally prove denied identity, expired/invalid signed assertion, access/refresh/client revocation and actual refresh denial as applicable. For hosted routes, verify reachability from the receiving provider, actual credential header/principal handling and a real connector query. Record source/build/client versions, endpoint identity, command, result code, synthetic IDs/counts and body-free evidence.

All generated guides remain **Not yet verified** until those exact client calls pass. OAuth/account/header-beta inputs stay explicit in the release ledger. A successful Codex bundled backend smoke does not establish desktop UI support, and CLI connection listings do not establish actual tool calls.

Client/OAuth sources checked 2026-10-06: [OpenAI MCP configuration](https://learn.chatgpt.com/docs/extend/mcp?surface=cli), [Claude cloud network requirements](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp), [Claude OAuth requirements](https://claude.com/docs/connectors/building/authentication), [Cloudflare Tunnel setup](https://developers.cloudflare.com/tunnel/get-started/), [Access Managed OAuth](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/managed-oauth/). The tunnel/OAuth references identify implementation gates; no external deployment or flow is claimed here.

Cloudflare named-tunnel, ingress and Host-header instructions checked against the linked official documentation on **2026-10-07**. No installation, login, DNS change or tunnel activation was performed while preparing this guide.

Documentation reviewed 2026-10-12 for public instructions and links; historical installed-version/source checks above retain their original dates. No new client runtime test is claimed. See [compatibility](../compatibility.md) and [remote route choices](../remote/index.md). Repository-source examples may require access while the repository remains private.

The named-tunnel creation, ingress routing and origin-authority instructions were re-reviewed against the linked official Cloudflare documentation on **2026-10-12**. The manual route remains documentation reviewed; no provider installation, login, DNS change or tunnel activation was performed.

## Full application catalog

The combined application advertises twelve tools: eight archive reads, `send_message`, `send_media`, `send_voice_note`, and read-only `get_send_status`. Verify contact/group and attachment scope alongside message/context reads; mutation annotations do not grant sending rights. New connections start read-only. Check default-denied sends with synthetic data; real delivery requires a separately authorized connected account. Preserve request IDs when status is uncertain. See [MCP access](../mcp.md), [attachments](../attachments.md), and [outbound messaging](outbound-messaging.md).
