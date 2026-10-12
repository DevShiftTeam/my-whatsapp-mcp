# Connect an AI client

The full application advertises twelve tools: eight archive reads (`search_contacts`, `search_groups`, `list_chats`, `search_messages`, `get_context`, `get_index_status`, `search_attachments`, and `get_attachment`), three send mutations (`send_message`, `send_media`, `send_voice_note`), and the read-only `get_send_status`. Connections start read-only, with their own chat scope, optional date bounds, and expiry. Sending text, media, or existing voice recordings requires a separate owner-approved permission for selected chats or all permitted chats; see [outbound messaging](clients/outbound-messaging.md).

## Local Codex or Claude Code

1. Keep My WhatsApp running and confirm that a message is searchable in its browser interface.
2. Open **AI connections → Create a read-only connection**.
3. Choose your client and **Local, on this computer**.
4. Name the connection, select the permitted chats and dates, set an expiry, and create it.
5. Choose **Show setup instructions**. Copy the generated configuration into your AI client's MCP settings. Use the guide for the exact client you selected.
6. Start a new AI session and ask it to call `get_index_status`, search your distinctive test message, then use the returned `message_id` with `get_context`.

The app generates a private credential file and a stdio adapter configuration. You do not need to choose a port or copy a local token. Keep the executable at the path referenced by the generated configuration; regenerate the client setup if you move it.

The app also advertises outbound tools, but their presence does not grant send permission. Open **AI connections → Permissions** on an existing connection to choose read-only, selected-chat sending, or sending to all permitted chats. Send-permission edits keep its token, expiry, and read scope unchanged. All mode includes future chats within that scope and the archive policy. See the [outbound guide](clients/outbound-messaging.md#give-one-connection-permission-to-send) for the editor and authenticated owner API.

Hosted Composio and Claude connectors cannot use this local adapter because they run outside your computer. They require remote access.

## Find a contact or group

Search names before searching a conversation's messages:

| Tool | Example arguments |
| --- | --- |
| `search_contacts` | `{"query":"NoaLevi","limit":10}` |
| `search_groups` | `{"query":"Weekend plans","limit":10}` |
| `list_chats` | `{"query":"Noa Levi","chat_type":"direct","limit":10}` |

`search_contacts` searches direct conversations; `search_groups` searches groups. Both require a name query. `list_chats` accepts an optional query and `chat_type` of `all`, `direct`, or `group`. The default page size is 20, with a maximum of 50. When a result has `next_cursor`, pass it as `cursor` with the same query, chat type where supported, and limit to read the next page.

Name matching ignores case, spaces, punctuation, and Hebrew marks: the fictional name `Noa Levi` also matches `NoaLevi`. It does not transliterate between Hebrew and Latin spellings. Results include only archived chats with messages readable under this connection's permissions and the archive policy. They are not a full phonebook or a list of all WhatsApp groups.

Use a returned opaque `chat_id` in `search_messages`' `chat_ids` filter, then use a returned `message_id` with `get_context`. Group search supports the same reading flow. Sending to a group requires an owner-approved send mode that covers it; finding or reading the group grants no sending rights. These tools do not manage members or group settings.

After upgrading the app, use **Sync** or refresh tool discovery in Composio if the new search tools are missing. Keep the existing connection and token. For a local client, restart its MCP session to refresh the tool list.

## Choose live or historical reads

When an operator attaches a captured historical view to the live app, exactly six read tools accept the optional `source` argument: `search_messages`, `get_context`, `list_chats`, `search_contacts`, `search_groups`, and `get_index_status`. Omit it to read **Live**, or choose `"live"` or `"historical"` explicitly. The tool catalog stays at twelve; the sources are never merged. Without a configured historical view, omit `source` and use the normal live reads.

For example, call `search_messages` with:

```json
{"source":"historical","query":"Friday proposal","limit":10}
```

Keep `source: "historical"` when passing a returned historical `message_id` to `get_context`, a historical `chat_id` to a read filter, or a historical cursor to its next page. Historical IDs belong to that captured view and cannot be used for live sending, attachment retrieval or media fetches. `search_attachments`, `get_attachment` and all outbound tools remain live-only. [Sending permission](clients/outbound-messaging.md) still requires separate owner approval.

Historical responses label the capture time, read-only view, partial coverage and unknown current phone state. Later phone edits or deletions may be absent. The current live account, credential, expiry, date/chat scope, retention choices and archive policy govern historical reads; authorization and source safety are checked before reading and again before delivering a response. Revoked or expired credentials cannot retain access through frozen historical grants. Historical selection is not a fallback when the live account becomes unsafe: current reconciliation or source-safety failures also block these reads.

In the owner interface, **View archive → Live / Historical** selects what Archive and Stats display. Settings, importing, live sync, send permissions and sending continue to use Live. Preparing or extending [live recovery](setup.md#prepare-a-separate-live-recovery-profile) does not make captured history complete or certify the current phone state.

## Find a document or other attachment

Use `search_attachments` to filter filenames, file types, dates and nearby conversation text. Results include an opaque `attachment_id` and optional neighboring messages. `get_attachment` retrieves a retained original through a private two-minute download URL, or returns files up to 64 KiB as Base64 when `inline` is requested. Both tools keep the connection's scope and expiry. See [attachments](attachments.md) for examples, remote routing, availability limits and recovery of documents imported by older versions.

## Remote bearer access

Remote access is an advanced setup. You need an HTTPS hostname, a tunnel or reverse proxy to this computer, and a client that can send an `Authorization` header. My WhatsApp does not configure the tunnel or run an OAuth authorization server.

The proxy must preserve the configured public `Host` header and pass `Authorization` unchanged. Replacing `Host` with `localhost` causes the app to reject the request; `X-Forwarded-Host` does not replace this check.

### MCP-only endpoint

Stop the running app, then start it with a dedicated MCP listener and the public URL you own. Replace the example hostname:

```sh
./bin/wa-indexer serve \
  --mcp-listen 127.0.0.1:6968 \
  --mcp-public-url https://archive.example.com/mcp
```

Route `/mcp` and canonical `/mcp/attachments/<ticket>` paths from that HTTPS hostname to the dedicated listener at `http://127.0.0.1:6968`, preserving the path; return an error for other paths. Do not tunnel the local owner interface on port 64165. Keep the computer, app, and tunnel running. The attachment URL is a short-lived capability bound to its original client grant; see [attachments](attachments.md#retrieve-the-file).

This route supports read and permitted send tools through MCP. Direct JSON sending additionally needs the four exact `/api/send/*` routes listed in the [outbound guide](clients/outbound-messaging.md#json-api); do not expose other paths on the MCP listener.

Create a connection in **AI connections**, choosing **Remote**. Select a narrow scope when possible. **Show setup instructions** displays the MCP server URL; **Reveal remote token** exposes that connection's token for you to copy into the client.

Clients must send:

```text
Authorization: Bearer <My WhatsApp client token>
```

Use the receiving client's secret store or the generated guide's dedicated environment variable. Keep tokens out of URLs, source control, and public logs. A Composio project API key is separate from the My WhatsApp client token. Composio's API-key mapping must produce `Authorization: Bearer {{generic_api_key}}`; an API Key dropdown alone does not guarantee that header.

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

Route the HTTPS hostname to `http://127.0.0.1:6969`. This admin listener serves the password-protected interface and forwards `/mcp`, canonical attachment download paths and the four outbound JSON routes to the configured MCP handler. Never expose its HTTP origin directly on the network.

The admin password controls browser access and client management, including send permissions. MCP bearer tokens work independently of that password and use their configured read scope plus the owner-approved send mode; they cannot administer the app or grant themselves sending rights. An admin browser cookie cannot authenticate an MCP client.

## Authentication troubleshooting

If a client gets HTTP 401 or `mcp_unauthorized`, first open the owner interface and check the archive's status. A source-safety pause can deny credential resolution before `tools/list`, even when the token has not changed. An **Active** connection in a hosted client does not establish that the archive is readable.

Keep the existing data directory and client settings while diagnosing. If the owner interface reports `reconciliation_required` or `drain_failed`, follow the [committed-snapshot recovery guide](setup.md#recover-the-committed-archive-snapshot) for an eligible profile; an ordinary restart does not clear a durable safety pause. Retry a read with the existing credential after archive access is restored, before rotating keys or creating another connection.

If the archive is readable but authentication still fails, check the MCP URL, preserved public `Host`, `Authorization: Bearer` header, and the connection's expiry or revocation. Do not paste tokens or private data into a support report.

## Check access and revoke it

Ask your client to read an included test message. Then try an excluded chat or date: it must not return those messages. The generated guide includes client-specific checks.

Use **AI connections → Revoke** to end a grant. Removing an AI client's settings or deleting its local configuration file alone does not revoke the grant. Test that the old connection is denied, then remove it from the AI client.

Archive read results describe stored snapshots, with coverage and gap details available from `get_index_status`. They may be incomplete or differ from the current phone state. Imported text is untrusted data and must not be treated as instructions by the AI client.

If the owner opts into [partial unknown-history handling](setup.md#choose-an-unknown-history-policy), an encountered unknown gap produces a persistent account-wide warning in archive reads, including MCP (`unknown_history_gap: true`). Unsupported edits, deletions or retention changes may be unreflected, and current phone state is unknown. Clients must keep this warning even when reading only selected chats. Restarting or returning to strict mode does not clear it; current credentials, expiry, chat/date policy and known controls still apply. The separate [offline unknown-history acceptance](setup.md#accept-unknown-partial-history-offline) preserves client grants but does not recover the failed batch's unvisited remainder or grant additional authority. MCP cannot change the policy or run recovery.

After an owner performs [committed-snapshot recovery](setup.md#recover-the-committed-archive-snapshot), existing client grants and their scopes remain in place. `get_index_status` retains `owner_restored_snapshot` as a partial-coverage gap. Successful response labels include `recovery_code: "owner_restored_snapshot"` and a `recovery_epoch`, while the data remains a stored snapshot whose current phone state is unverified. Recovery does not certify missing edits, deletions, or full history. Clients should retain those limits when summarizing restored archive results. The narrow [classified-history acceptance](setup.md#accept-a-classified-partial-history-snapshot) is likewise an offline operator action, not an MCP tool; its results remain partial and current phone state remains unverified. It preserves the current connection and send journal without granting additional scope. Only the three reviewed participant-number-change, username-change and group member link-mode notice shapes are eligible; later operations need fresh evidence, and message-conflict quarantines and spent recovery requests remain in place.

## Voice transcripts

After [transcription is configured](transcription.md), generated voice text participates in keyword search with the same chat and date scope as its source message. `get_context` returns it in `derived[].text`, with `source: "generated_voice_transcript"`, `generated: true`, and provenance. The original message's `body` can remain empty; there is no top-level `transcript` field.

`media_availability: "not_retained"` describes the original recording, not whether a transcript exists. A stored transcript can remain readable after original-cache eviction if its source is still valid. Check `derived` and `get_index_status`; do not infer that a voice note was transcribed from its media kind alone. Generated text may contain transcription errors.
