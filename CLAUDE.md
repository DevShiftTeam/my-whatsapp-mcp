# Repository instructions

Follow [AGENTS.md](AGENTS.md) for the documentation, privacy, validation, and publication rules.

The product is **My WhatsApp**; MCP is its optional AI connection feature. Keep the existing logo and compatibility identifiers, including the `wa-indexer` executable and original Go module path.

Read [setup](docs/setup.md), [MCP](docs/mcp.md), [development](docs/development.md), and [security](SECURITY.md) when working in those areas. Read [native models](docs/native-models.md) for media or inference changes, and [transcription](docs/transcription.md) for the optional voice service.

Read [attachments](docs/attachments.md) when changing document retention, scoped attachment search, or original downloads.

Read [Stats](docs/stats.md) for dashboard behavior and appearance, including the default system theme and browser-local light or dark preference.

MCP connections are read-only by default. Read [outbound messaging](docs/clients/outbound-messaging.md) when changing owner-approved send permissions, provider calls, or durable retry behavior.

Read the [roadmap](docs/roadmap.md) for proposed work and [business messaging](docs/business-messaging.md) for the distinction between local presets and official Business Platform templates. Keep experimental, planned, and available capabilities explicit.

Keep the relevant Markdown guides and [Changelog → Unreleased](CHANGELOG.md#unreleased) current when changing behavior, setup, or APIs. Use synthetic test data, preserve concurrent work, run the relevant checks, and state unverified limitations plainly.
