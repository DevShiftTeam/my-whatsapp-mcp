# Working in this repository

My WhatsApp MCP is a local WhatsApp archive with scoped, read-only MCP access. The executable remains `wa-indexer`; keep existing data paths and credentials compatible unless a change explicitly includes migration.

Before editing, read the relevant source and these guides:

- [Setup](docs/setup.md) for build, launch, pairing, and user workflows.
- [MCP](docs/mcp.md) for clients, scopes, authentication, and remote access.
- [Development](docs/development.md) for package ownership and checks.
- [Security](SECURITY.md) for private data and reporting.
- [Native models](docs/native-models.md) when changing media or inference.
- [Transcription](docs/transcription.md) for the optional voice service, configuration, and generated transcripts.

Keep documentation beside the code changes. Update the authoritative guide and add a concise entry under [Changelog → Unreleased](CHANGELOG.md#unreleased) for behavior, setup, or API changes. Describe the resulting behavior and limitations in plain language; do not invent validation or release claims.

Preserve other work in progress. Use synthetic fixtures and separate test profiles; never print or commit chat contents, linked-device secrets, owner links, tokens, or private env files. Preserve scope, source-invalidation, and response checks when changing reads.

Run checks appropriate to the change using [development](docs/development.md), and report what ran. Keep this repository private until the owner explicitly requests public visibility. Do not publish a package or release without a user request to do so.
