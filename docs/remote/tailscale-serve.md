---
title: Tailscale Serve
description: Set up private access for your enrolled devices with one saved node identity.
---
# Tailscale Serve

Choose **Tailscale private — recommended for my devices** for a phone or MCP client that runs on a permitted tailnet device. The source preview embeds Tailscale; a separate Tailscale daemon on the archive host is not part of this managed procedure. Actual provider/device acceptance is **NOT_RUN**.

## Before you start

You need a Tailscale account/tailnet, permission to enroll the archive node, tailnet HTTPS enabled, and access rules allowing the remote device. Install Tailscale on your phone or other client device and enroll it in the intended tailnet. Review [Serve requirements and access rules](https://tailscale.com/docs/features/tailscale-serve) and [plan eligibility](https://tailscale.com/pricing), reviewed **2026-10-12**.

## Save and connect

1. Open My WhatsApp's **local** owner interface, then **Remote access → Set up remote access**.
2. Choose **My phone or personal devices** and **Tailscale private**. Choose a stable **Saved device name**, such as `wa-archive`.
3. Leave browser access off for MCP only. For a phone browser, first follow [Phone access](phone-access.md).
4. Select **Save and connect**. If sign-in is required, select **Get Tailscale sign-in link**, open the displayed official Tailscale link and authorize the node in your account. Finish any device approval/HTTPS permission steps there, then **Refresh status**.
5. Copy the app's **Saved MCP endpoint** once available. Use that exact `https://…ts.net/mcp` address with a separate scoped credential from **AI connections**. Do not construct an address from a device name before the provider assigns it.

The saved endpoint contains no token. A sign-in link is for the local owner and must stay private. Provider sign-in does not authorize MCP calls. This integration does not consume inherited Tailscale auth-key/client-secret environment variables; remove those from the app's launch environment if the app reports incompatible provider configuration.

## Verify from another device

From an enrolled permitted device, use the exact intended client/version to run all [twelve tools and denial checks](../clients/remote-access.md#acceptance-test). An unenrolled device must not reach the private route. A hosted cloud connector is not a suitable private-tailnet test.

Record the endpoint privately; reconnect, restart the app and recreate its container with the **same volume**. Compare the endpoint exactly. Exercise provider revocation and recovery using a disposable qualification account while confirming local keyword search remains usable. Those live checks have not been run here.

## Recover, disable and remove

A missing sign-in, HTTPS permission, revoked node or hostname mismatch requires fixing that account/identity. Reconnect to the saved identity; the app refuses silent replacement. Keep the host awake and preserve the data volume.

Use **Disable remote access** for a temporary stop. To retire it, revoke the relevant MCP credentials, disable access, then **Remove saved provider** and confirm. Remove the corresponding node from your Tailscale admin console and remove unused client settings. Provider removal does not reset the remote owner password or delete the archive. Use **Change domain or provider account** only when you intend to migrate and update clients.
