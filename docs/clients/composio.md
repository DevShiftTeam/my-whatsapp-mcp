---
title: Composio Custom MCP
description: Use the documented experimental SDK registration and explicit bearer mapping.
---
# Composio Custom MCP

This guide uses Composio's experimental Custom MCP API with a public HTTPS endpoint and a separate My WhatsApp token. Registration and authenticated calls still need testing in an actual Composio project.

## Public URL and explicit header mapping

The documented Platform registration route requires a **public HTTPS MCP endpoint**. Local and stdio-only servers are unsupported by this route. Use the dedicated bearer-only hostname, for example `https://bearer.example.com/mcp`; keep all local administration outside the tunnel.

The local adapter's private Unix socket and generated credential file are for local clients. For Composio, the app must configure a separate dedicated MCP listener published over HTTPS and a remote scoped credential.

## Manual Platform setup

First configure a [managed public route](../remote/index.md) or the [manual named Cloudflare tunnel](remote-access.md#tunnel-and-authentication), using your saved hostname. In My WhatsApp **AI connections**, create a remote Composio credential with the intended chat/date limits and expiry. Use **Reveal remote token → Copy token and close**; copying closes the reveal view automatically. You also need a Composio project you control and its project API key. This is the documented experimental Platform route; it does not establish the behavior of a separate For You beta form.

### 1. Prepare the SDK example

In a new directory you control, install the TypeScript/JavaScript SDK and copy [composio-setup.mjs](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/docs/clients/examples/composio-setup.mjs) into that directory. Use Node.js **22.22.3 or newer** and use the SDK version in this example **0.22.0**, as specified by its [published package metadata](https://registry.npmjs.org/@composio%2Fcore/0.22.0):

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

The toolkit uses `Authorization: Bearer {{generic_api_key}}` for outgoing My WhatsApp calls. See the [Custom MCP guide](https://docs.composio.dev/docs/extending-sessions/custom-mcp) for Composio's registration API.

Composio stores the raw My WhatsApp token on the connected account. An ACTIVE account still needs a successful archive query to confirm the token and route work.

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

Keep the printed toolkit/auth-config/connected-account/session IDs privately for your integration and cleanup. If setup stops partway through, inspect the objects identified by those IDs before retrying; rerunning the whole example creates additional auth configs/connections. The registered URL and auth scheme cannot be changed in place: select a new toolkit slug or deliberately delete/re-register the old toolkit and its connections. This example uses **@composio/core 0.22.0**; live registration and tool calls have not yet been confirmed.

### Using a dashboard form

If you use a dashboard form, it must let you configure the Authorization header with `Bearer {{generic_api_key}}`. An API Key dropdown alone does not specify that prefix. Otherwise, use the SDK example above.

## Check and remove access

Use your own Composio project and a separate scoped My WhatsApp credential.

After registration, create an active API-key connection in the same toolkit/project and wait for authenticated tool sync. Execute `search_messages`, `get_context` using the returned ID, `list_chats`, and `get_index_status` using a harmless phrase in a permitted chat. Verify the actual request has `Authorization: Bearer` without recording its value; confirm excluded scope and policy narrowing deny old context access.

Revoke the exact My WhatsApp credential ID shown in the connection guide. Calls through the existing Composio connection must fail, then revoke/remove the connected account and registration through its supported authorized API/UI. An active account or successful sync alone does not replace this test.

This implementation supplies the documented API-key mapping. This setup uses API-key authentication, not OAuth. See [remote access](remote-access.md) for reachability, data transit and listener separation.

For API details, see [Composio Custom MCP](https://docs.composio.dev/docs/extending-sessions/custom-mcp) and the [SDK reference](https://docs.composio.dev/reference/sdk-reference/typescript/custom-toolkits).
