---
title: ngrok
description: Connect using your assigned public domain and one provider authtoken.
---
# ngrok

Use ngrok when you want a public HTTPS MCP address on your account's assigned domain. Managed setup is implemented in the source preview. Fresh-account setup, actual clients and phone interstitial behavior are **NOT_RUN**.

## Before you start

Sign in to your ngrok account. Obtain the account's assigned development domain and an **agent authtoken**. This flow does not require an ngrok API key as a second credential. The free plan provides an assigned domain and usage quotas; inspect [current limits](https://ngrok.com/docs/pricing-limits/free-plan-limits) before choosing it, reviewed **2026-10-12**. A running route needs a running host and app.

## Save and connect

1. Open the local owner interface: **Remote access → Set up remote access**.
2. Choose **A public MCP endpoint for an AI client**, then **ngrok — assigned public domain**.
3. Enter **Saved HTTPS hostname**, for example `https://your-assigned-name.ngrok-free.app`, with no path, query or token. Paste the agent authtoken into **Provider token** in this local form.
4. Leave browser access off for MCP only, then choose **Save and connect**. For browser access, set the separate owner password first using [Phone access](phone-access.md).
5. Copy **Saved MCP endpoint**; it includes `/mcp`. Create a separate My WhatsApp credential for each AI client, with the intended chats/dates and expiry.

The provider authtoken is stored privately. On a saved provider, leaving its token field empty retains the current credential. Do not paste either provider or MCP tokens into a URL, command argument or diagnostic report.

## Verify both kinds of traffic

Use the exact intended client/version for [twelve tools, scope, expiry and revocation](../clients/remote-access.md#acceptance-test) from outside the archive host. Provider connection alone is not client acceptance.

ngrok documents an HTML warning page on the free tier and distinguishes API traffic. A browser may show **Visit** before the app login. Our physical-phone/interstitial check has not been run; do not assume an MCP client or phone bypasses it. The warning does not replace the owner password. Record actual behavior in your own qualification.

Keep the same volume and saved domain after reconnect, app restart and container recreation. Verify exact URL equality. If the provider reports a different address, the app refuses it; migrate explicitly instead.

## Recover and retire

Check account quotas, token validity and domain assignment when connection fails. Refresh the provider credential locally if revoked, reconnect to the same domain, and verify that local keyword search still works during the outage. No actual token revocation/recovery run is claimed here.

Choose **Disable remote access** to stop while preserving setup. For retirement, revoke app client credentials, disable access, confirm **Remove saved provider**, remove the endpoint/revoke the unused agent authtoken in your ngrok dashboard, and remove client settings. Preserve the archive volume and remote owner password unless you separately intend to change them. Use the explicit migration control for another domain/account.
