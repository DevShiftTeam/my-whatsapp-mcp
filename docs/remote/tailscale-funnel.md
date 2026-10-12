---
title: Tailscale Funnel
description: Publish an authenticated MCP route at a saved Tailscale hostname.
---
# Tailscale Funnel

Funnel gives your archive a public HTTPS endpoint for clients outside your Tailscale network. Your Tailscale account must allow Funnel. The app includes Tailscale for this setup.

## Before you start

You need a tailnet with MagicDNS, HTTPS certificates, and a policy granting the archive node Funnel permission. Funnel uses names in your tailnet domain and has provider bandwidth limits. The app uses HTTPS port 443. Check [official requirements](https://tailscale.com/docs/features/tailscale-funnel) and [account eligibility](https://tailscale.com/pricing). A tailnet administrator may need to approve the permission.

## Save and connect

1. In the local owner interface, open **Remote access → Set up remote access**.
2. Choose **A public MCP endpoint for an AI client**, then **Tailscale Funnel** and a stable **Saved device name**.
3. Keep browser access off unless you deliberately follow [Phone access](phone-access.md).
4. Select **Save and connect**. If needed, use **Get Tailscale sign-in link**, complete the official account sign-in/device approval, enable the required tailnet HTTPS/Funnel permission, then **Refresh status**.
5. Copy **Saved MCP endpoint** and configure your client with a separately created My WhatsApp scoped credential. Retain `/mcp` and the exact assigned hostname.

Funnel and private Tailscale access have different exposure. Verify which provider is selected before using a URL. HTTPS and a provider account do not replace application authorization.

## Verify and retain the identity

From outside your Tailscale network, [check your connection](../clients/remote-access.md#check-your-connection). Confirm that your client can search permitted messages and cannot read excluded chats. Use [Phone access](phone-access.md) for a remote browser.

Keep the same data directory or Docker volume across restarts so the endpoint stays the same. Live Funnel, AI-client, and phone tests are still pending; see [compatibility](../compatibility.md).

## Troubleshooting and retirement

For permission failure, inspect the tailnet's HTTPS settings, node approval and Funnel policy. For reachability failure, inspect provider DNS/connection state and account limits. Keep the archive host/app running. Do not create a new random hostname to conceal an error.

**Disable remote access** retains the identity for reconnect. Retire it by revoking the app's client credentials, disabling access, confirming **Remove saved provider**, and removing the corresponding node/Funnel permission in your Tailscale account. Remove client settings as appropriate; keep the archive volume. Migrating domains or accounts requires the explicit migration control and client updates.
