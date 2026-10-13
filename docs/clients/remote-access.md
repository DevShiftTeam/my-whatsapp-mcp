---
title: Manual remote MCP setup
description: Configure a named MCP-only tunnel with private owner access and scoped clients.
---
# Remote access setup

Use this guide to set up a Cloudflare tunnel manually for a native installation. Remote access is in preview; real-account, live-provider, and AI-client tests are still pending.

For managed persistent access, use the local **Remote access** wizard and [provider guides](../remote/index.md). The manual MCP-only procedure below is an alternative for a native POSIX executable and a separately supervised helper on the same host. It must replay the same flags each launch, does not enable the managed wizard or remote browser, and is not a Docker host-to-container loopback recipe. Use one route per launch: a launch with `--mcp-listen` does not start the managed Remote access manager, and the managed Cloudflare route uses the fixed origin `127.0.0.1:8124`, the same port as the example below. Stop one route before starting the other.

## Choose one mode and the intended client

For local mode, choose your chat/date limits in My WhatsApp and copy the generated client configuration. It runs `wa-indexer mcp stdio --config <generated-private-file-path>` and connects to the running app through a **private Unix socket**. My WhatsApp creates and manages the version 2 credential file as a regular **0600** file in its **0700** directory. No port, secret copying or token environment variable is needed. Local credentials work only through the local adapter; remote clients receive separate credentials.

The app manages the local socket and file path. Copy the generated client settings as shown; connection guides only provide instructions.

Remote mode uses authenticated Streamable HTTP at the dedicated `/mcp` listener. Each bearer client gets its own token and chat/date restrictions. Those restrictions are enforced by the server on search, context, listings, status and cached results, not merely included as client filters.

Use the configuration for your chosen connection type. Remote URLs must use HTTPS and end in `/mcp`, without tokens, query strings, or fragments. Use your public hostname rather than a local IP address.

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

Use your own hostname and preserve your usual `--data-dir` if applicable. These flags must be supplied together; changing a running configuration requires restart. Do not run this listener while managed remote access is active on the same host. Port **8124** is this example's dedicated MCP port. **Never use the private owner interface or its address as the tunnel origin.** No packaged executable is available; see [desktop availability](../install/desktop.md).

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

Copy [the example configuration](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/docs/clients/examples/cloudflared.yml) to a new file you control, for example `/Users/YOU/.cloudflared/wa-indexer-mcp.yml`. Replace the UUID, credentials path and hostname; use the dedicated MCP port from step 1:

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

This route uses bearer tokens. It does not provide OAuth. Hosted Claude also requires an account with request-header support; see [Claude remote connectors](claude-connectors.md).

## Credentials and previews

Never place tokens in URLs, argv, logs or copied diagnostic text. Codex bearer mode reads a dedicated environment variable; Claude Code explicitly adds `Bearer ` in its header template. Eligible Claude cloud accounts enter the full literal header value. Composio uses the explicit `Authorization: Bearer {{generic_api_key}}` mapping. Provider/project credentials are separate from My WhatsApp tokens.

The app keeps tokens out of generated setup previews and diagnostic output. Local configuration contains a private credential-file path; leave the file contents private.

## Check your connection

1. From the intended client, search for a harmless phrase in a permitted chat and open its context.
2. Check that excluded chats and dates cannot be read.
3. Change the connection's scope and confirm an old result cannot reveal newly excluded messages.
4. Restart the app and tunnel with the same data directory, then confirm the saved endpoint still works.
5. When retiring the connection, revoke it in My WhatsApp before removing its client configuration.

New connections are read-only. Grant [sending](outbound-messaging.md) separately when needed, and keep the same request ID when a send result is uncertain.

A successful tunnel login or a client connection indicator is not enough: confirm an actual search result. This setup has not yet been tested through a live provider and AI client. See [compatibility](../compatibility.md).
