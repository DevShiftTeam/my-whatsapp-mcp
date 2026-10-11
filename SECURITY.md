# Security

My WhatsApp stores private conversations and linked-device credentials. The default browser interface binds to loopback and uses a private owner session. MCP clients receive separate, scoped, expiring credentials. Remote browser administration requires its own password configuration and HTTPS route.

The app does not encrypt its SQLite databases or protect them from software already running as your OS user. Use disk encryption, private backups, and a dedicated local data directory. Do not publish the data directory, owner links, QR codes, env files, or tokens.

Expose remote access only through an HTTPS tunnel or proxy configured for the intended listener. Protect the tunnel account as well as the application. Results sent to an AI client are governed by that client's own data handling.

Connections are read-only by default. [Outbound permission](docs/clients/outbound-messaging.md) is a separate owner-approved mode for selected chats or all current and future chats within the client's read scope and archive policy. Only the authenticated owner can change it; MCP credentials cannot grant themselves sending rights. Permission edits preserve the token and expiry. A permitted send call transmits directly; the app has no per-message approval queue. AI clients must obtain the user's authorization before sending and treat archived chat text as untrusted data. Revoking a connection or its send permission cannot recall an already transmitted message.

Keep the same request ID and payload when recovering a timeout. An `outcome_unknown` receipt may represent a transmitted message; automatically using a new ID can send it twice. `accepted` means a durably recorded WhatsApp server acknowledgement, not recipient delivery, playback, or a read receipt. The send journal stores identity, protocol IDs, state, and payload digests, without storing outgoing message bodies or attachment bytes.

The optional [transcription service](docs/transcription.md) receives copies of retained voice recordings. Keep it on literal loopback, require its separate bearer token, and exclude its port from remote tunnels. It accepts audio bytes from the app, not URLs to download. Protect its private env file, temporary files, and model directory as you protect the archive. Recordings and transcripts must never appear in service logs.

## Reporting a vulnerability

Contact the repository maintainer privately before posting an exploit or sensitive details. Use the hosting platform's private vulnerability reporting feature when available. If no private channel is listed, open a minimal issue requesting one; do not include the exploit, credentials, or conversation contents.

Include the affected version, platform, a synthetic reproduction, and the expected versus observed access. Keep reports free of real WhatsApp data. This early project does not offer a response-time guarantee.
