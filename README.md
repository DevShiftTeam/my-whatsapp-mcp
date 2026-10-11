# WA Indexer

A local WhatsApp archive with Hebrew and English keyword search and read-only MCP access. Link your phone by QR code, choose the chats and dates to keep, and search the messages WhatsApp delivers to this computer.

The basic app is one Go executable with an embedded browser interface and SQLite storage. It needs no Docker, Node.js, Python, model download, or paid API.

## Start locally

Install Go 1.27.1 and a C compiler, then run these commands from the source directory:

```sh
./scripts/app/build.sh
./bin/wa-indexer serve
```

The app opens an authenticated owner window at `http://127.0.0.1:64165`. Keep the terminal running. Follow the [step-by-step setup guide](docs/setup.md) to link your phone and verify an import.

To try synthetic Hebrew and English conversations first, run `./bin/wa-indexer demo`. The demo uses a separate archive and never connects to WhatsApp. Stop it with Ctrl+C before starting your own archive.

## What is available

- QR linking, chat and date filters, message search, and conversation context.
- Local MCP connections for Codex and Claude Code, with per-client chat scopes and expiry.
- Remote MCP over HTTPS with scoped bearer tokens; optional password-protected administration.
- Optional local semantic search, image OCR and descriptions, and voice transcription. These require a separately prepared native bundle and remain experimental; the ordinary source build runs text search with them off.

The source targets macOS 14 or newer and Linux. Windows is unsupported; Linux release testing is still pending.

## Limits and privacy

WhatsApp may deliver only part of your history. The app reports received coverage and sync gaps; it cannot promise a complete backup or exact current phone state. It uses the unofficial [whatsmeow](https://github.com/tulir/whatsmeow) library, so WhatsApp changes can interrupt linking or import.

Your archive and linked-device credentials stay in a private local data directory. SQLite files are **not encrypted by the app**. Use OS disk encryption and protect backups. Connecting an AI client shares its permitted results with that client; remote access also passes through your tunnel provider.

## Documentation

- [Local setup and troubleshooting](docs/setup.md)
- [MCP clients and remote access](docs/mcp.md)
- [Optional models and media](docs/native-models.md)
- [Code structure and development](docs/development.md)
- [Contributing](CONTRIBUTING.md) and [security](SECURITY.md)

Licensed under [GPL-3.0-only](LICENSE). Bundled and optional third-party components have their own notices in [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).
