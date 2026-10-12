---
title: Claude remote connectors
description: Connect Claude web or Desktop using an eligible account and a public MCP URL.
---
# Claude web and Desktop remote connectors

Claude remote connectors connect from Anthropic's cloud, including when you use Claude Desktop. They need a public HTTPS MCP address. Localhost and private Tailscale Serve addresses do not work for this connection type.

This setup requires a Claude account that can configure custom **Request headers**. Availability depends on your account and organization. If that option is absent, this bearer-token route is unavailable. The remote setup does not provide OAuth.

## Set up the endpoint

For eligible accounts, use the [manual MCP-only route](remote-access.md#tunnel-and-authentication). After confirming that your Claude account supports request headers, add `--mcp-header-auth-available` to every app launch. This flag lets the app offer the matching connection instructions; it does not enable the feature in your Claude account.

The managed remote wizard currently does not issue Claude connector credentials. Keep browser access off when you only need MCP; a remote owner password does not authenticate Claude.

## Configure request headers

Use these settings with your own public endpoint:

```text
Remote MCP URL: https://bearer.example.com/mcp
Authentication: No sign-in
Request header: Authorization
Header value: Bearer <REDACTED>
Required: Yes
```

In Claude's protected header field, replace `<REDACTED>` with the raw token from the separate My WhatsApp connection. Include `Bearer ` once. Keep the token out of the URL.

An organization-wide connector using one fixed token gives all its users that token's access. Select only the chats and dates you intend to share.

## Check and remove access

Ask Claude to search for a harmless phrase in a permitted chat and show its context. Check that excluded chats cannot be read. Revoke the connection in My WhatsApp, confirm the old connector can no longer retrieve messages, then remove it in Claude.

This account-dependent setup has not been tested with a live Claude connector. See [compatibility](../compatibility.md), [Claude's connector requirements](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp), and [request-header setup](https://claude.com/docs/connectors/custom/add-unlisted#authenticate-with-request-headers).
