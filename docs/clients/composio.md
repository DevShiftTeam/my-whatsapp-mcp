---
title: Composio Custom MCP
description: Use the documented experimental SDK registration and explicit bearer mapping.
---
# Composio Custom MCP

Status: **Not yet verified** for actual registration, outbound headers or My WhatsApp calls. Installed Composio CLI checked 2026-10-06: **0.3.1**. Its help does not establish a Custom MCP registration command or the beta dashboard's defaults.

## Public URL and explicit header mapping

The documented Platform registration route requires a **public HTTPS MCP endpoint**. Local and stdio-only servers are unsupported by this route. Use the dedicated bearer-only hostname, for example `https://bearer.example.com/mcp`; keep all local administration outside the tunnel.

The local adapter's private Unix socket and generated credential file are for local clients. For Composio, the app must configure a separate dedicated MCP listener published over HTTPS and a remote scoped credential.

## Manual Platform setup

First configure a [managed public route](../remote/index.md) or the [manual named Cloudflare tunnel](remote-access.md#tunnel-and-authentication), using your saved hostname. In My WhatsApp **AI connections**, create a remote Composio credential with the intended chat/date limits and expiry. Use **Reveal remote token → Copy token and close**; copying closes the reveal view automatically. You also need a Composio project you control and its project API key. This is the documented experimental Platform route; it does not establish the behavior of a separate For You beta form.

### 1. Prepare the SDK example

In a new directory you control, install the TypeScript/JavaScript SDK and copy [reviewed repository-source composio-setup.mjs](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/docs/clients/examples/composio-setup.mjs) into that directory. Use Node.js **22.22.3 or newer** and pin the inspected SDK release **0.22.0**, as specified by its [published package metadata](https://registry.npmjs.org/@composio%2Fcore/0.22.0):

```sh
npm init -y
npm install @composio/core@0.22.0
npm ls @composio/core
```

The example performs these documented SDK operations in order:

1. `experimental.customToolkits.upsert`: registers the public MCP URL with an explicit API_KEY header template.
2. `authConfigs.create`: creates an API_KEY auth config, enabled for Tool Router use.
3. `connectedAccounts.initiate`: stores the raw WA token as `generic_api_key` for your selected stable Composio user ID. `connectedAccounts.get` then fetches its status with a 30-second request deadline; the example requires ACTIVE before sync.
4. `experimental.customToolkits.sync`: discovers tools using that active connected account.
5. `sessions.create`: selects only this toolkit and pins that connected account for the same user.

The complete example uses this registration configuration:

```js
const toolkit = await composio.experimental.customToolkits.upsert({
  slug: 'WA_INDEXER',
  toolkitConfig: {
    name: 'My WhatsApp',
    appUrl: publicURL,
    authSchemes: [{
      mode: 'API_KEY',
      headers: { Authorization: 'Bearer {{generic_api_key}}' },
      apiKeyField: { displayName: 'My WhatsApp token',
        description: 'Paste the raw per-client token; omit the Bearer prefix.' },
    }],
  },
});
```

The SDK sends this to `POST https://backend.composio.dev/api/v3.1/custom/toolkits/upsert`; its project `apiKey` becomes the request's `x-api-key`. The returned toolkit slug has a `CUSTOM_` prefix. The schema and transformer preserve the configured Authorization template. [Custom MCP lifecycle](https://docs.composio.dev/docs/extending-sessions/custom-mcp), [SDK registration reference](https://docs.composio.dev/reference/sdk-reference/typescript/custom-toolkits), [header schema](https://raw.githubusercontent.com/ComposioHQ/composio/next/ts/packages/core/src/types/customToolkits.types.ts), [request transformer](https://raw.githubusercontent.com/ComposioHQ/composio/next/ts/packages/core/src/utils/transformers/customToolkits.ts).

The connected-account state is `authScheme: 'API_KEY'` with `val: {status: 'ACTIVE', generic_api_key: rawToken}`. The auth config's `credentials: {}` contains no WA token; the connected account stores it. ACTIVE does not prove that My WhatsApp accepted the token. [Auth-config create](https://docs.composio.dev/reference/sdk-reference/typescript/auth-configs), [connection initiation](https://docs.composio.dev/reference/sdk-reference/typescript/connected-accounts), [API-key state schema](https://raw.githubusercontent.com/ComposioHQ/composio/next/ts/packages/core/src/types/connectedAccountAuthStates.types.ts).

### 2. Supply your values without putting secrets in commands

Set the public URL to the exact URL configured in My WhatsApp. Choose your own stable Composio application user ID:

```sh
export WA_INDEXER_PUBLIC_URL='https://wa-mcp.example.com/mcp'
export WA_INDEXER_COMPOSIO_USER_ID='wa-indexer-owner'
```

Your secret manager can supply `COMPOSIO_API_KEY` and `WA_INDEXER_CLIENT_TOKEN` as protected process environment values. Alternatively, run this Bash block in an interactive terminal in the example's directory; both key prompts hide your input and the secret values stay out of shell history and argv:

```sh
bash -c '
  set -eu
  set +x
  test -t 0 || exit 1
  IFS= read -r -s -p "Composio project API key: " COMPOSIO_API_KEY
  printf "\n"
  IFS= read -r -s -p "Raw My WhatsApp token: " WA_INDEXER_CLIENT_TOKEN
  printf "\n"
  export COMPOSIO_API_KEY WA_INDEXER_CLIENT_TOKEN
  exec node composio-setup.mjs
'
```

Running this example **registers the toolkit and creates an auth config, connected account and session in your Composio project**. It performs no model inference or WA tool call. The example disables SDK logs/tracking/version checks, uses bounded requests and prints only selected IDs, a tool count and a registry version. Never print full account objects, SDK errors, token environment values, or `session.mcp` headers. [Official SDK logging controls](https://docs.composio.dev/docs/production-readiness#configure-sdk-logging), [constructor controls](https://raw.githubusercontent.com/ComposioHQ/composio/next/ts/packages/core/src/composio.ts).

Composio substitutes the account's raw WA token into `Bearer {{generic_api_key}}`. **Do not paste a value that already starts with Bearer.** The Composio project key authenticates management requests to Composio; the WA token authenticates outgoing calls to My WhatsApp. Tokens never belong in the public URL.

Keep the printed toolkit/auth-config/connected-account/session IDs privately for your integration and cleanup. If setup stops partway through, inspect the objects identified by those IDs before retrying; rerunning the whole example creates additional auth configs/connections. The registered URL and auth scheme cannot be changed in place: select a new toolkit slug or deliberately delete/re-register the old toolkit and its connections. The example was checked against the published **@composio/core 0.22.0** source and its schemas; its installed SDK/runtime and actual registration remain **Not yet verified**.

### Screenshot UI and beta availability

The current Platform guide documents API/experimental SDK management and says dashboard management is forthcoming. A separate For You beta form may differ. Its API Key dropdown, URL field or ACTIVE connection cannot establish the outgoing header. Use a UI route only when it can explicitly configure **Authorization** with **Bearer {{generic_api_key}}**, or after that route's actual header/prefix is verified. Otherwise, use the complete Platform sequence above and keep the beta UI path unverified.

## Registration, test and revoke gates

Real registration requires the owner's selected Composio project/account, permission to register the Custom MCP, a public HTTPS listener and a dedicated scoped token. No registration or account authentication was performed while preparing this guide.

After authorized registration, create an active API-key connection in the same toolkit/project and wait for authenticated tool sync. Execute `search_messages`, `get_context` using the returned ID, `list_chats`, and `get_index_status` against a synthetic permitted source. Verify the actual request has `Authorization: Bearer` without recording its value; confirm excluded scope and policy narrowing deny old context access.

Revoke the exact My WhatsApp credential ID shown in the connection guide. Calls through the existing Composio connection must fail, then revoke/remove the connected account and registration through its supported authorized API/UI. An active account or successful sync alone does not replace this test.

This implementation supplies the documented API-key mapping. Custom MCP OAuth registration is not advertised for this product route until separately verified. See [remote access](remote-access.md) for reachability, data transit and listener separation.

Sources checked 2026-10-07: [Custom MCP guide](https://docs.composio.dev/docs/extending-sessions/custom-mcp), [experimental upsert API](https://docs.composio.dev/reference/api-reference/toolkits/postCustomToolkitsUpsert), [first-party documentation source](https://raw.githubusercontent.com/ComposioHQ/composio/next/docs/content/docs/extending-sessions/custom-mcp.mdx), [SDK schema](https://raw.githubusercontent.com/ComposioHQ/composio/next/ts/packages/core/src/types/customToolkits.types.ts). The documentation source branch is mutable; the example also has a pinned published [SDK release](https://registry.npmjs.org/@composio%2Fcore/0.22.0) source check. These are documented capability checks, not outbound-request evidence.

Documentation reviewed 2026-10-12 for public instructions and links; historical installed-version/source checks above retain their original dates. No new client runtime test is claimed. See [compatibility](../compatibility.md) and [remote route choices](../remote/index.md). Repository-source examples may require access while the repository remains private.

## Managed saved endpoints

The local **Remote access** wizard now configures the [implemented provider previews](../remote/index.md). Copy its saved HTTPS endpoint ending in `/mcp`, then create the separate scoped remote credential in **AI connections**. Provider login and a browser owner password do not authenticate this client. Keep browser exposure off unless you deliberately enable [Phone access](../remote/phone-access.md).

A hosted connector requires public Funnel/ngrok/Cloudflare reachability; private Tailscale works only when the client itself runs on an enrolled device. The provider flow supplies no OAuth authorization server. Account-dependent header/OAuth combinations retain their unverified status. Actual client/version twelve-tool, scope, expiry, old-context and revocation checks remain **NOT_RUN**. Reconnect/restart must retain the saved URL; migrating accounts or domains requires updating this client deliberately.

## Full application catalog

The combined application advertises twelve tools: eight archive reads, `send_message`, `send_media`, `send_voice_note`, and read-only `get_send_status`. Verify contact/group and attachment scope alongside message/context reads; mutation annotations do not grant sending rights. New connections start read-only. Check default-denied sends with synthetic data; real delivery requires a separately authorized connected account. Preserve request IDs when status is uncertain. See [MCP access](../mcp.md), [attachments](../attachments.md), and [outbound messaging](outbound-messaging.md).
