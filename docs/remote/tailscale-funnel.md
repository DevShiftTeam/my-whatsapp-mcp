---
title: Tailscale Funnel
description: Publish an authenticated MCP route at a saved Tailscale hostname.
---
# Tailscale Funnel

Funnel makes your saved endpoint reachable from the public Internet. Choose it for a client outside your tailnet when your account permits Funnel. This managed source preview embeds Tailscale. Live Funnel, hosted-client and physical-phone acceptance remain **NOT_RUN**.

## Before you start

You need a tailnet with MagicDNS, HTTPS certificates, and a policy granting the archive node Funnel permission. Funnel uses names in your tailnet domain and has provider bandwidth limits. The app uses HTTPS port 443. Check [official requirements](https://tailscale.com/docs/features/tailscale-funnel) and [account eligibility](https://tailscale.com/pricing), reviewed **2026-10-12**. A tailnet administrator may need to approve the permission.

## Save and connect

1. In the local owner interface, open **Remote access → Set up remote access**.
2. Choose **A public MCP endpoint for an AI client**, then **Tailscale Funnel** and a stable **Saved device name**.
3. Keep browser access off unless you deliberately follow [Phone access](phone-access.md).
4. Select **Save and connect**. If needed, use **Get Tailscale sign-in link**, complete the official account sign-in/device approval, enable the required tailnet HTTPS/Funnel permission, then **Refresh status**.
5. Copy **Saved MCP endpoint** and configure your client with a separately created My WhatsApp scoped credential. Retain `/mcp` and the exact assigned hostname.

Funnel and private Tailscale access have different exposure. Verify which provider is selected before using a URL. HTTPS and a provider account do not replace application authorization.

## Verify and retain the identity

From outside the tailnet, execute the [actual-client acceptance checks](../clients/remote-access.md#acceptance-test), including anonymous, incorrect and expired credentials, excluded chats/dates and revocation. A DNS response or provider-online state is insufficient. Browser access needs a separate physical-phone test.

With the same private volume, compare the endpoint after reconnect, app restart and container recreation. Revoke access in a disposable provider account, check the actionable error and local keyword search, then recover the same route. No live result is claimed for these steps.

## Troubleshooting and retirement

For permission failure, inspect the tailnet's HTTPS settings, node approval and Funnel policy. For reachability failure, inspect provider DNS/connection state and account limits. Keep the archive host/app running. Do not create a new random hostname to conceal an error.

**Disable remote access** retains the identity for reconnect. Retire it by revoking the app's client credentials, disabling access, confirming **Remove saved provider**, and removing the corresponding node/Funnel permission in your Tailscale account. Remove client settings as appropriate; keep the archive volume. Migrating domains or accounts requires the explicit migration control and client updates.
