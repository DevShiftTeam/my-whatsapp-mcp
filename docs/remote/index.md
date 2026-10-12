---
title: Remote access
description: Choose a saved HTTPS address for an AI client or your own devices.
---
# Remote access

Remote access is an **implemented source preview**, off until you enable it. Open the local owner interface and choose **Remote access → Set up remote access**. Keep the archive host and app running. No provider account, domain or permission is created automatically.

## Choose your route

| Your goal | Route | Before you start |
| --- | --- | --- |
| My phone or a client on my own devices | [Tailscale Serve](tailscale-serve.md) | A tailnet, HTTPS permission and enrolled remote devices |
| A public MCP address without your own domain | [Tailscale Funnel](tailscale-funnel.md) | A tailnet with HTTPS and Funnel permission |
| A public MCP address with an assigned domain | [ngrok](ngrok.md) | An ngrok account, assigned domain and agent authtoken |
| Use an existing domain | [Cloudflare](cloudflare.md) | Cloudflare DNS, a named tunnel and dashboard routing |

Serve is private: a hosted Claude or Composio connector cannot join your tailnet merely because your phone can reach it. Funnel, ngrok and Cloudflare provide public reachability. Each MCP route uses one saved HTTPS hostname plus `/mcp`; the app never substitutes a random URL after a failed reconnect.

## Connect an AI client

Leave **Also open the archive in a remote browser** off for an MCP-only route. After provider setup, copy **Saved MCP endpoint**, then create a separate remote credential in **AI connections** with the intended chats, dates and expiry. Follow the [client guide](../clients/remote-access.md#choose-one-mode-and-the-intended-client). Provider credentials establish the route; My WhatsApp credentials authorize archive tools.

A connected provider does not mean the archive is paired, recovered or ready for MCP. Cloudflare additionally reports **route unverified** because its dashboard route cannot be established from the local helper's connection. Verify from the exact client before using a route for real data.

## Open the archive on your phone

Follow [Phone access](phone-access.md) to set a separate remote owner password locally, then explicitly enable browser access. This publishes a password-protected owner interface at the saved HTTPS hostname. It grants broader archive control than a scoped MCP credential. It does not forward the localhost bootstrap link. Browser access stays off unless selected.

## Keep the address across restarts

Retain the same private data directory or Docker volume. The app reloads saved provider identity and reconnects after restart. **Disable remote access** stops it while retaining that identity; **Remove saved provider** is a separate confirmed action. Domain/account migration is explicit and requires updating AI clients. Never delete the volume to repair provider login.

During a provider outage, use the local owner interface and keyword search. Inspect the app's fixed recovery message, provider account permissions and saved hostname. Correct the existing account/route, reconnect, and verify the same endpoint. Disable, removal and account-side teardown are detailed in each guide.

## Qualification and cost

The wizard, routing, authorization and recovery boundaries have local synthetic tests. **Fresh provider accounts, actual remote clients and physical phones: NOT_RUN.** See [compatibility](../compatibility.md) for immutable builds and measured local results. Setup duration and one-click account provisioning are not claimed.

Provider limits and eligibility can change. Check the official plan links in the provider guide. Domain purchase/renewal, paid provider services and AI-client subscriptions are separate. Remote results traverse the selected route and receiving AI service. No OAuth server is supplied by this setup.
