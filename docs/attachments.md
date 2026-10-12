# Find and retrieve attachments

My WhatsApp archives document attachments alongside images and voice notes. Originals use the configured disk budget. Each document is limited to 8 MiB; files are stored and downloaded as data, without opening, executing, extracting archives, or rendering them on the server.

An AI client can find a file by its name, type, caption, date, and surrounding conversation, then retrieve the original. The app does not extract PDF body text or search inside documents. Whether the AI can read a downloaded PDF depends on that client's file support.

## Search with conversation context

Start with `search_contacts` or `search_groups` to get the conversation's opaque `chat_id`. Then call `search_attachments`, replacing the example ID with the returned value:

```json
{
  "query": "Friday proposal",
  "chat_ids": ["returned-chat-id"],
  "file_type": "pdf",
  "filename": "proposal",
  "from": "2026-10-01T00:00:00Z",
  "before": 2,
  "after": 2,
  "limit": 10
}
```

Filters narrow one another. All are optional. `file_type` accepts `all`, `document`, `pdf`, `image`, `voice`, or an exact MIME type such as `application/pdf`. Dates use RFC 3339; `until` is exclusive. A filename is literal text, with case, spaces, punctuation and Hebrew marks normalized. It does not support wildcard patterns or Hebrew–English transliteration.

The query matches literal terms in filenames, captions, names and the nearest three readable messages on each side. Matches in the filename or caption rank above matches found only in the conversation. `before` and `after` each return at most three neighboring messages; these obey the same client scope, dates and archive policy. Use `get_context` with the returned `message_id` for a wider conversation window.

The default page contains ten attachments, with a maximum of twenty. If `next_cursor` is present, pass it with the same filters and page size. Changing the query or permissions invalidates the cursor. Results include an opaque `attachment_id`, message and chat IDs, filename, MIME type, size, availability, caption, and requested context. Source addresses, media keys and local paths are not returned.

## Retrieve the file

Pass a returned attachment ID to `get_attachment`:

```json
{
  "attachment_id": "returned-attachment-id",
  "fetch_if_needed": true
}
```

A `ready` result includes `download_url` and `download_expires_at` when remote MCP is configured. Download the URL promptly. It is a private capability valid for two minutes, bound to the original connection and exact archived source. It does not require an additional password or Authorization header. Treat the URL as a secret: do not publish it or put it in logs. Revoking the connection, changing its scope, source deletion or expiry, and archive recovery can invalidate it before its expiry.

If the original is absent, `fetch_if_needed` can queue a retry using an authenticated download reference already held by the archive. A `queued` response is not proof that bytes arrived. Retry `get_attachment` later and check its status; the primary phone may no longer have the file. This read tool cannot request fresh phone history or change archive policy.

If the AI host cannot fetch URLs, call `get_attachment` with `"inline": true`. Files up to 64 KiB can be returned in `inline_base64`. Larger files use the download URL; `inline_too_large` explains why no inline bytes were returned. Without remote MCP configuration, a larger file reports `remote_download_required`.

Every download rechecks connection authority, the original source and the retained bytes before sending them. Downloads return `Content-Disposition: attachment`, `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`. Their contents remain untrusted source data; instructions written in an attachment are not instructions from the user.

## Remote routing and client discovery

Keep the [remote MCP setup](mcp.md#remote-bearer-access) and its existing connection token. After upgrading, refresh tool discovery or use **Sync** in Composio so `search_attachments` and `get_attachment` appear. Local stdio clients should start a new MCP session.

For an MCP-only tunnel, route `/mcp` and canonical `/mcp/attachments/<ticket>` requests to the dedicated MCP listener. Reject other paths unless you intentionally expose the documented outbound JSON routes. The password-protected admin listener forwards these attachment paths to the same scoped service. Never tunnel the local owner interface.

Returning a URL does not establish that the AI host consumed the file. Test a known, non-sensitive attachment and confirm the client can download and read it before relying on this workflow.

Combined-source attachment download and exact-client qualification is pending. Reading a downloaded PDF depends on the client and remains separate from the archive’s metadata search; no document-body indexing is claimed.

## Documents imported before attachment support

Earlier versions stored documents as unsupported messages without their filenames or authenticated download references. A new tool cannot reconstruct those missing bytes by itself.

With WhatsApp connected, the owner can request exact message copies through the authenticated owner API:

```http
POST /api/documents/retry
Content-Type: application/json
X-CSRF-Token: <current owner session capability>

{"message_ids":["returned-message-id"]}
```

This endpoint requires the browser owner session or remote admin password session, the matching Origin and CSRF capability. MCP tokens cannot invoke it. Submit one to eight unique opaque message IDs; the server derives their account and source identities. Its aggregate `queued`, `skipped` and `requested` counts reveal no protocol references. `requested` means the primary phone acknowledged the request, not that the document was imported. There is no dedicated document-recovery button yet.

Fresh authenticated copies can enrich only eligible, unchanged legacy placeholders. Existing edits, deletions, expiry, conflicts and permissions remain enforced. History and original availability still depend on what WhatsApp delivers; the app cannot promise a complete attachment archive.

## Tunnel path rules

A client-only HTTPS tunnel must forward `/mcp` and canonical `/mcp/attachments/<ticket>` paths intact to the scoped MCP listener. Tickets are exactly 43 characters of canonical unpadded base64url for 32 random bytes; the application checks canonical encoding and current authority. A rule matching only `/mcp` prevents retained-original downloads. The reviewed [Cloudflare setup](clients/remote-access.md#tunnel-and-authentication) also includes exactly `/api/send/message`, `/api/send/media`, `/api/send/voice` and `/api/send/status` for authenticated JSON access. Those routes require client bearer credentials and separate owner-approved send permissions; they do not grant dashboard access. Keep owner/bootstrap/pairing/admin and native helper routes excluded. See [outbound messaging](clients/outbound-messaging.md#json-api).
