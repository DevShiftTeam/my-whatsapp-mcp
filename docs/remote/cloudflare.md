---
title: Cloudflare Tunnel
description: Connect a named tunnel to the app's fixed managed origin.
---
# Cloudflare Tunnel

Use Cloudflare when you control a domain with Cloudflare DNS and want a fixed public hostname. The app includes the tunnel helper.

## Before you start

You need permission to create a remotely managed named tunnel and published application route in your Cloudflare account. Choose a hostname such as `archive.example.com`. Domain registration/renewal and optional paid services are separate costs. Follow [Cloudflare's dashboard tunnel guide](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-remote-tunnel/).

## Create the route and save the token

1. In Cloudflare, create a **named, remotely managed tunnel**. Retrieve its tunnel token privately; do not run a token-bearing command copied from the dashboard.
2. Add a published application route for your chosen hostname. Set the service to **HTTP** with address **`127.0.0.1:8124`**. In its HTTP origin settings, set **HTTP Host Header** to the exact saved hostname, such as `archive.example.com`. Keep request Origin headers unchanged. See [origin parameters](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/origin-parameters/).
3. For client access only (MCP, retained originals and the four authenticated JSON send/status routes), restrict the route path to `^(/mcp(/attachments/[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048])?|/api/send/(message|media|voice|status))$`. For deliberately enabled browser access, the same hostname must route the app's browser paths too; leave the path unrestricted and follow [Phone access](phone-access.md). The app still restricts which remote endpoints exist and requires authorization.
4. In My WhatsApp's **local** owner interface, choose **Remote access → Set up remote access → Use my existing Cloudflare domain**.
5. Enter `https://archive.example.com` in **Saved HTTPS hostname** and paste the tunnel token into **Provider token**. Keep browser access off unless its separate password is configured, then select **Save and connect**.
6. Copy **Saved MCP endpoint** and create a separately scoped credential for your AI client. The hostname is saved; `/mcp` is appended by the app.

The bundled helper and app share a container/network namespace. Do not publish the origin port, use the localhost owner port, or point a host-side helper at container loopback. Helper diagnostics are on private loopback port **8125**; never publish or route that port. No extra Compose port is needed for this managed route.

## Understand “route unverified”

The app reports **Helper running; connection and hostname unverified** after starting the helper. The helper may still be offline or retrying authentication. Its running state does not confirm that your hostname is reachable. Check connectivity, provider credentials and the saved dashboard route, then test the public endpoint separately. This is separate from archive pairing and each MCP token's authorization.

Verify externally using the [connection checks](../clients/remote-access.md#check-your-connection). Check Host, path and service settings if requests fail. Do not rewrite Origin headers to work around validation. A Cloudflare Access login policy can add another client authentication requirement; that extra authentication is not covered here, and the app does not supply an OAuth server.

## Restart, recover and retire

Keep the same data volume, tunnel and hostname after app/container restart or recreation. Compare the exact MCP URL each time. For token revocation or provider outage, inspect the saved dashboard route/token, correct that setup locally and reconnect; local keyword search remains independent. Fresh provider recovery has not been exercised here.

**Disable remote access** stops the helper and retains identity. To retire access, revoke MCP credentials, disable, confirm **Remove saved provider**, then remove the exact published route/DNS record and unused tunnel/token in your Cloudflare account. Remove client settings and preserve your archive volume. Changing domain/account requires explicit migration.

The [manual MCP-only procedure](../clients/remote-access.md#tunnel-and-authentication) remains an alternative for an already-built native POSIX executable with a separately supervised helper on the same host. Supply its exact listener/public-URL flags every launch. It does not provide managed browser access, supervision or automatic saved configuration.
