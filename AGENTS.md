# Working in this repository

My WhatsApp MCP is a local WhatsApp archive with scoped MCP access, read-only by default. Optional sending requires a separate owner-approved mode: selected chats or all chats within the client's read scope and archive policy. Permission edits preserve the existing token, read scope, and expiry. The executable remains `wa-indexer`; keep existing data paths and credentials compatible unless a change explicitly includes migration.

Before editing, read the relevant source and these guides:

- [Setup](docs/setup.md) for build, launch, pairing, and user workflows.
- [MCP](docs/mcp.md) for clients, scopes, authentication, and remote access.
- [Attachments](docs/attachments.md) for document retention, scoped file search, and private original downloads.
- [Outbound messaging](docs/clients/outbound-messaging.md) for send permissions, media limits, durable request IDs, and uncertain outcomes.
- [Development](docs/development.md) for package ownership and checks.
- [Security](SECURITY.md) for private data and reporting.
- [Native models](docs/native-models.md) when changing media or inference.
- [Transcription](docs/transcription.md) for the optional voice service, configuration, and generated transcripts.
- [Roadmap](docs/roadmap.md) for proposed priorities and the distinction between available and planned capabilities.
- [Business messaging](docs/business-messaging.md) when discussing local message presets or a future official Business Platform adapter.

Keep documentation beside the code changes. Update the authoritative guide and add a concise entry under [Changelog → Unreleased](CHANGELOG.md#unreleased) for behavior, setup, or API changes. Describe the resulting behavior and limitations in plain language; do not invent validation or release claims.

Preserve other work in progress. Use synthetic fixtures and separate test profiles; never print or commit chat contents, linked-device secrets, owner links, tokens, or private env files. Preserve scope, source-invalidation, and response checks when changing reads.

Run checks appropriate to the change using [development](docs/development.md), and report what ran. Keep this repository private until the owner explicitly requests public visibility. Do not publish a package or release without a user request to do so.
