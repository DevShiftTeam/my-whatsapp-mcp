---
title: Phone access
description: Enable a separate password-protected owner interface at your saved HTTPS hostname.
---
# Phone access

You can explicitly enable a remote browser interface after setting its own owner password on the archive computer. This is an **implemented source preview**: combined-source HTTPS browser/authentication qualification is pending; an actual phone and live provider route have **not been tested**.

## Prepare a route and password

1. On the archive computer, open the local owner interface and **Remote access → Set up remote access**.
2. Choose [Tailscale private](tailscale-serve.md) for your enrolled devices, or deliberately select a [public route](index.md). For private access, install Tailscale on the phone, enroll it and ensure access rules permit the archive node.
3. Select **Set a remote owner password first**. Choose a separate password and **Save owner password**. The app accepts 12–128 UTF-8 bytes. Keep it in your password manager.
4. Select **Also open the archive in a remote browser**, finish the provider's hostname/account setup, then **Save and connect**.
5. On the phone, open the saved **HTTPS hostname without `/mcp`**. For example, an MCP endpoint `https://archive.example.com/mcp` has browser address `https://archive.example.com/`.

Browser access starts off. Do not copy the localhost owner-link/bootstrap fragment to the phone or forward the local owner port. Cloudflare needs the browser paths routed to its fixed managed origin as described in its guide. Free ngrok may show an HTML interstitial first; actual phone behavior remains unverified.

## Unlock, search and log out

Enter **Owner password**, then **Unlock archive**. Confirm the hostname is the one you saved. Search a known synthetic record when qualifying the route, open its context, then choose **Log out**. Revisit and reload the page: protected archive content must require login again. Check the same behavior after session expiry and from an unenrolled device for private Tailscale.

The owner browser can manage archive choices and other permitted owner operations, including pairing; it is broader access than an AI client's chat/date scope. Provider setup, owner-password changes, token reveal, native attach/bootstrap and archive deletion stay local. Do not use a provider authtoken or scoped MCP token as the owner password. Neither credential type substitutes for the other.

## Reconnect and recover

Keep the host awake, app running and the same private data volume. App restart or provider reconnection can end browser sessions; use the same saved hostname and sign in again. A changed remote owner password signs out all remote browsers. Reset the password through the **local** owner interface if forgotten.

If login is temporarily busy, wait and retry; repeated guesses are limited. If the page is unreachable, check provider connectivity and device enrollment from the host. A provider outage does not stop local archive/search work. A successful phone page load does not establish an AI client's MCP authentication.

## Turn it off

From the local owner interface, edit the saved setup, clear **Also open the archive in a remote browser**, and save. **Disable remote access** stops the entire route while preserving its identity. **Remove saved provider** is separate and does not delete the archive or reset the owner password; finish provider-account teardown in the selected guide.

No physical-phone, enrolled/unenrolled-device or live logout acceptance is claimed. The local synthetic browser fixture checks login, secure session behavior, search, logout/expiry denial, secret separation and a 390px layout; a mobile viewport does not prove a device journey. See [compatibility](../compatibility.md).
