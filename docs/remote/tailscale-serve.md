---
title: Tailscale Serve
description: Set up private access for your enrolled devices with one saved node identity.
---
# Tailscale Serve

Choose **Tailscale private — recommended for my devices** for a phone or MCP client on your own Tailscale network. The app includes Tailscale for this setup, so you do not need a separate daemon on the archive host.

## Before you start

You need a Tailscale account/tailnet, permission to enroll the archive node, tailnet HTTPS enabled, and access rules allowing the remote device. Install Tailscale on your phone or other client device and enroll it in the intended tailnet. Review [Serve requirements and access rules](https://tailscale.com/docs/features/tailscale-serve) and [plan eligibility](https://tailscale.com/pricing).

## Save and connect

1. Open My WhatsApp's **local** owner interface, then **Remote access → Set up remote access**.
2. Choose **My phone or personal devices** and **Tailscale private**. Choose a stable **Saved device name**, such as `wa-archive`.
3. Leave browser access off for MCP only. For a phone browser, first follow [Phone access](phone-access.md).
4. Select **Save and connect**. If sign-in is required, select **Get Tailscale sign-in link**, open the displayed official Tailscale link and authorize the node in your account. Finish any device approval/HTTPS permission steps there, then **Refresh status**.
5. Copy the app's **Saved MCP endpoint** once available. Use that exact `https://…ts.net/mcp` address with a separate scoped credential from **AI connections**. Do not construct an address from a device name before the provider assigns it.

The saved endpoint contains no token. A sign-in link is for the local owner and must stay private. Provider sign-in does not authorize MCP calls. This integration does not consume inherited Tailscale auth-key/client-secret environment variables; remove those from the app's launch environment if the app reports incompatible provider configuration.

## Verify from another device

From an enrolled device, [check your connection](../clients/remote-access.md#check-your-connection) using the AI client you plan to use. Devices outside your Tailscale network cannot reach this private route. Hosted cloud connectors need a public route instead.

Keep the same data directory or Docker volume across restarts. The saved endpoint should stay the same. Live-provider connection and recovery tests are still pending; see [compatibility](../compatibility.md).

## Recover, disable and remove

A missing sign-in, HTTPS permission, revoked node or hostname mismatch requires fixing that account/identity. Reconnect to the saved identity; the app refuses silent replacement. Keep the host awake and preserve the data volume.

Use **Disable remote access** for a temporary stop. To retire it, revoke the relevant MCP credentials, disable access, then **Remove saved provider** and confirm. Remove the corresponding node from your Tailscale admin console and remove unused client settings. Provider removal does not reset the remote owner password or delete the archive. Use **Change domain or provider account** only when you intend to migrate and update clients.
