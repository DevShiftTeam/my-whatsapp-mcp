# Security

My WhatsApp MCP stores private conversations and linked-device credentials. The default browser interface binds to loopback and uses a private owner session. MCP clients receive separate, scoped, expiring credentials. Remote browser administration requires its own password configuration and HTTPS route.

The app does not encrypt its SQLite databases or protect them from software already running as your OS user. Use disk encryption, private backups, and a dedicated local data directory. Do not publish the data directory, owner links, QR codes, env files, or tokens.

Expose remote access only through an HTTPS tunnel or proxy configured for the intended listener. Protect the tunnel account as well as the application. Results sent to an AI client are governed by that client's own data handling.

The optional [transcription service](docs/transcription.md) receives copies of retained voice recordings. Keep it on literal loopback, require its separate bearer token, and exclude its port from remote tunnels. It accepts audio bytes from the app, not URLs to download. Protect its private env file, temporary files, and model directory as you protect the archive. Recordings and transcripts must never appear in service logs.

## Reporting a vulnerability

Contact the repository maintainer privately before posting an exploit or sensitive details. Use the hosting platform's private vulnerability reporting feature when available. If no private channel is listed, open a minimal issue requesting one; do not include the exploit, credentials, or conversation contents.

Include the affected version, platform, a synthetic reproduction, and the expected versus observed access. Keep reports free of real WhatsApp data. This early project does not offer a response-time guarantee.
