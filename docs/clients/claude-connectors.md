---
title: Claude remote connectors
description: Understand public reachability and account-specific header or OAuth gates.
---
# Claude web and Desktop remote connectors

Status: **Not yet verified** for this account and My WhatsApp endpoint. Claude Desktop version checked 2026-10-06: **2.19675.1**. Request-header eligibility is an account/organization capability, not something established by the installed version.

## Reachability and mode

Remote connectors connect from **Anthropic's cloud**, including when the user runs Claude Desktop. The server must therefore have a public HTTPS MCP URL reachable from that infrastructure. A localhost URL cannot work through this route. Claude Desktop local stdio/configuration or a desktop extension is a separate mechanism; this guide does not claim it is a remote connector.

The local adapter's private Unix socket stays on your computer and is not a public connector address. Use the app-configured remote Streamable HTTP listener for this route.

Use the authenticated `/mcp` route and leave optional browser exposure off for this client. Browser owner access, if separately enabled, requires its own password and grants broader permissions. See [remote access](remote-access.md) before activating a tunnel.

## Fixed request headers: eligible accounts only

The current Request headers feature is a **beta for a limited set of organizations**. Only offer this guide after the owner confirms that the selected account can see and use it. Team/Enterprise ownership or role restrictions also apply to adding the connector.

For a bearer-only endpoint, configure:

```text
Remote MCP URL: https://bearer.example.com/mcp
Authentication: No sign-in
Request header: Authorization
Header value: Bearer <REDACTED>
Required: Yes
```

Enter the full `Bearer ` scheme followed by the raw per-client My WhatsApp token in the actual protected credential field. The preview remains redacted. Claude sends the literal value and does not add a prefix. Do not put the token in the URL. A shared organization fixed credential gives everyone using the connector that credential's scoped access; it does not identify individual members.

If the Request headers section is absent, this route is unsupported for that account. Do not suggest bypassing an OAuth hostname with a standalone token.

## OAuth: broad intended compatibility

Accounts without header beta need an **actually verified** OAuth route. Configuration alone or signed fixture JWT tests do not establish that route. The complete gate includes protected-resource/authorization-server discovery, registration, S256 PKCE/resource binding, consent/callback, refresh, signed identity verification, policy mapping, denied identity and revocation, followed by a real Claude connector query.

Use `https://oauth.example.com/mcp` on a separate hostname from bearer mode. Choose the client registration method that actually passed with the authorization server. Hosted callback: `https://claude.ai/api/mcp/auth_callback`. Claude Code uses a loopback callback and needs separate verification. CIMD or DCR requirements must match the chosen route.

Do not configure Authorization as a request header on an OAuth connection. An email identity alone does not prove which OAuth client called; separate client scopes/revocation require a verifiable client identity or separate applications/endpoints.

## Test and revoke

After the owner's authorized connection/consent, enable only this connector and execute all eight archive read tools on a synthetic permitted source. Confirm scoped chat/date absence and denial of a previous context ID after policy narrowing. Record the exact account capability, client surface and endpoint without tokens or message bodies.

Revoke the My WhatsApp credential/policy grant first and prove the existing connector can no longer call any tool. For OAuth, also revoke its access and refresh credentials and prove refresh denial. Then disconnect/remove the connector. Deleting a connector in Claude alone does not replace server-side revocation testing.

Sources checked 2026-10-06: [connector network requirements](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp), [request-header setup and beta limits](https://claude.com/docs/connectors/custom/add-unlisted#authenticate-with-request-headers), [OAuth requirements](https://claude.com/docs/connectors/building/authentication). Cloud account, tunnel and consent checks remain pending external gates; no account was authenticated during implementation.

Documentation reviewed 2026-10-12 for public instructions and links; historical installed-version/source checks above retain their original dates. No new client runtime test is claimed. See [compatibility](../compatibility.md) and [remote route choices](../remote/index.md). Repository-source examples may require access while the repository remains private.

## Managed saved endpoints

The local **Remote access** wizard now configures the [implemented provider previews](../remote/index.md). Managed transport does not confirm an account’s Request headers capability, so managed routes refuse Claude connector credential issuance and bearer setup guides. Eligible accounts currently use the [manual MCP-only route](remote-access.md#tunnel-and-authentication) with the explicit `--mcp-header-auth-available` owner declaration on every launch, after checking that account/organization capability. This declaration does not verify public reachability or client acceptance. Codex and Claude Code can still use separately scoped managed remote credentials. Provider login and a browser owner password do not authenticate this client. Keep browser exposure off unless you deliberately enable [Phone access](../remote/phone-access.md).

A hosted connector requires public Funnel/ngrok/Cloudflare reachability; private Tailscale works only when the client itself runs on an enrolled device. The provider flow supplies no OAuth authorization server. Account-dependent header/OAuth combinations retain their unverified status. Actual client/version twelve-tool, scope, expiry, old-context and revocation checks remain **NOT_RUN**. Reconnect/restart must retain the saved URL; migrating accounts or domains requires updating this client deliberately.

## Full application catalog

The combined application advertises twelve tools: eight archive reads, `send_message`, `send_media`, `send_voice_note`, and read-only `get_send_status`. Verify contact/group and attachment scope alongside message/context reads; mutation annotations do not grant sending rights. New connections start read-only. Check default-denied sends with synthetic data; real delivery requires a separately authorized connected account. Preserve request IDs when status is uncertain. See [MCP access](../mcp.md), [attachments](../attachments.md), and [outbound messaging](outbound-messaging.md).
