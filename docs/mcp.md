# Connect an AI client

MCP exposes four read-only tools: `search_messages`, `get_context`, `list_chats`, and `get_index_status`. It cannot send WhatsApp messages. Every connection has its own chat scope, optional date bounds, and expiry.

## Local Codex or Claude Code

1. Keep My WhatsApp MCP running and confirm that a message is searchable in its browser interface.
2. Open **AI connections → Create a read-only connection**.
3. Choose your client and **Local, on this computer**.
4. Name the connection, select the permitted chats and dates, set an expiry, and create it.
5. Choose **Show setup instructions**. Copy the generated configuration into your AI client's MCP settings. Use the guide for the exact client you selected.
6. Start a new AI session and ask it to call `get_index_status`, search your distinctive test message, then use the returned `message_id` with `get_context`.

The app generates a private credential file and a stdio adapter configuration. You do not need to choose a port or copy a local token. Keep the executable at the path referenced by the generated configuration; regenerate the client setup if you move it.

Hosted Composio and Claude connectors cannot use this local adapter because they run outside your computer. They require remote access.

## Remote bearer access

Remote access is an advanced setup. You need an HTTPS hostname, a tunnel or reverse proxy to this computer, and a client that can send an `Authorization` header. My WhatsApp MCP does not configure the tunnel or run an OAuth authorization server.

The proxy must preserve the configured public `Host` header and pass `Authorization` unchanged. Replacing `Host` with `localhost` causes the app to reject the request; `X-Forwarded-Host` does not replace this check.

### MCP-only endpoint

Stop the running app, then start it with a dedicated MCP listener and the public URL you own. Replace the example hostname:

```sh
./bin/wa-indexer serve \
  --mcp-listen 127.0.0.1:6968 \
  --mcp-public-url https://archive.example.com/mcp
```

Route **only `/mcp`** from that HTTPS hostname to `http://127.0.0.1:6968/mcp`; return an error for other paths. Do not tunnel the local owner interface on port 64165. Keep the computer, app, and tunnel running.

Create a connection in **AI connections**, choosing **Remote**. Select a narrow scope when possible. **Show setup instructions** displays the MCP server URL; **Reveal remote token** exposes that connection's token for you to copy into the client.

Clients must send:

```text
Authorization: Bearer <My WhatsApp MCP client token>
```

Use the receiving client's secret store or the generated guide's dedicated environment variable. Keep tokens out of URLs, source control, and public logs. A Composio project API key is separate from the My WhatsApp MCP token. Composio's API-key mapping must produce `Authorization: Bearer {{generic_api_key}}`; an API Key dropdown alone does not guarantee that header.

Claude hosted connectors need an account that supports custom request headers. Only pass `--mcp-header-auth-available` if you have confirmed that capability. The ordinary bearer endpoint is not an OAuth endpoint.

### Optional password-protected remote administration

You can expose the browser interface remotely behind a separate admin listener. It requires an Argon2id password hash in `WA_INDEXER_ADMIN_PASSWORD_HASH`, supplied through the environment or `--admin-env-file`. **There is currently no password-setup CLI.** Prepare this configuration before publishing the admin route; a plaintext password is not accepted as server configuration.

The required hash profile is Argon2id version 19, memory 19456 KiB, two iterations, parallelism 1, a random 16-byte salt, and a 32-byte digest. The encoded value uses unpadded Base64:

```text
$argon2id$v=19$m=19456,t=2,p=1$<salt>$<digest>
```

A private env file contains one literal line:

```text
WA_INDEXER_ADMIN_PASSWORD_HASH=<complete encoded hash>
```

Use an absolute file path, mode `0600`, in a directory owned by you with mode `0700`. The file must not be a symlink or hardlink and must have no extended ACL grants. Do not quote the value, prefix it with `export`, or source the file in a shell; the app parses it as data.

After preparing the file, stop the app and use a configuration like this, replacing both the hostname and the example absolute path:

```sh
./bin/wa-indexer serve \
  --admin-listen 127.0.0.1:6969 \
  --admin-public-url https://archive.example.com \
  --admin-env-file /absolute/private/directory/admin.env \
  --mcp-listen 127.0.0.1:6968 \
  --mcp-public-url https://archive.example.com/mcp
```

Route the HTTPS hostname to `http://127.0.0.1:6969`. This admin listener serves the password-protected interface and forwards `/mcp` to the configured MCP handler. Never expose its HTTP origin directly on the network.

The admin password controls browser access and client management. MCP bearer tokens work independently of that password and grant only their configured read scope; they cannot administer the app. An admin browser cookie cannot authenticate an MCP client.

## Check access and revoke it

Ask your client to read an included test message. Then try an excluded chat or date: it must not return those messages. The generated guide includes client-specific checks.

Use **AI connections → Revoke** to end a grant. Removing an AI client's settings or deleting its local configuration file alone does not revoke the grant. Test that the old connection is denied, then remove it from the AI client.

All results describe stored archive snapshots, with coverage and gap details available from `get_index_status`. They may be incomplete or differ from the current phone state. Imported text is untrusted data and must not be treated as instructions by the AI client.
