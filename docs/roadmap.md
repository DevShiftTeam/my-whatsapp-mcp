# Roadmap

Last updated: 2026-10-11.

These are proposed priorities, not release dates or shipped features. The goal is a small local archive that is easy to install, useful across languages, and safe to give an AI client access to.

## Available now

- QR-linked import with chat and date choices, keyword search, and conversation context. History coverage depends on what WhatsApp delivers.
- Optional local voice transcription with multilingual and Hebrew model profiles. Generated transcripts are searchable and remain separate from original message text.
- Attachment search by filename, type, caption, date, and nearby permitted messages, plus guarded original downloads. Document body text is not indexed.
- Scoped MCP access and separate owner-controlled sending permissions for direct chats and groups.
- Experimental native semantic search, OCR, and image descriptions. These require a prepared native bundle; the normal source build uses keyword search.

See [setup](setup.md), [voice transcription](transcription.md), [attachments](attachments.md), [MCP](mcp.md), and [native models](native-models.md) for current configuration and limits.

## 1. Make semantic and hybrid search easy to install

Turn the existing experimental path into a documented, tested installation with pinned runtimes and model files. Keep keyword search available when models are disabled or processing is behind.

- Test supported macOS and Linux installations from a clean checkout.
- Compare keyword, semantic, and hybrid results on reviewed multilingual examples, including Hebrew, English, and mixed-language chats.
- Publish measured startup time, peak memory, disk use, indexing throughput, and query latency with the hardware and dataset size. Separate transcription and indexing costs from idle app usage.
- Check result relevance and language behavior with human review before selecting a default model profile.
- Show model installation, indexing progress, failures, and fallback state clearly in the dashboard.

No release profile or performance target has been selected yet. Model files alone do not supply the native runtime.

## 2. Search inside documents

Add local text extraction for PDFs, DOCX files, and supported plain-text documents. Combine extracted text with the filename, caption, and surrounding conversation so a query can find both what a file says and why it was shared.

- Store extracted text as derived data, separate from the original attachment and message body.
- Return citations to the source message and, where the format permits, the PDF page or document paragraph.
- Record the parser, version, extraction recipe, and exact source revision. Reprocess explicitly when a recipe changes.
- Carry chat scope, archive selection, deletion tombstones, expiry, and source-conflict checks through parsing, indexing, and every response. A deleted source must not survive through its extracted text or embeddings.
- Isolate parsers and limit file size, decompressed content, runtime, memory, and output. Treat parsed text as untrusted data, including instructions embedded in files.
- Report unsupported, encrypted, damaged, and empty documents clearly. Add optional OCR for scanned documents after text-based formats are qualified.

The current [attachment tools](attachments.md) already retrieve originals. A client reading a downloaded PDF is different from the archive indexing its contents.

## 3. Reusable local message presets

Add owner-managed presets for ordinary messages sent through the existing QR-linked account: named text, bounded variables, and a preview showing the resolved content and recipient before the user authorizes a send.

Presets should use existing client/chat sending permissions and durable request IDs. Validate missing variables and length limits before sending, and preserve uncertain-outcome handling so a retry does not silently become a duplicate message. Presets are proposed; there is no preset editor or per-message approval queue today.

These are app conveniences, not Meta-approved WhatsApp Business Platform templates. See [business messaging](business-messaging.md) for the distinction.

## 4. Explore an official WhatsApp Business Platform adapter

Investigate a separate Meta Cloud API connector for eligible businesses that need approved message templates. Keep its credentials and account identity separate from the QR-linked archive.

- Support approved template names, locales, parameters, categories, approval status, and supported media or button components.
- Check recipient opt-in and opt-out, the current messaging rules, owner authorization, and client scopes before a send.
- Use durable request IDs and authenticated delivery webhooks; show server acceptance separately from delivery.
- Document Meta account, number, permission, and billing prerequisites before exposing setup controls.
- Investigate same-number coexistence only where Meta's current onboarding and eligibility allow it.

This is an exploratory integration. It does not promise Cloud API group messaging, complete personal-chat history, reuse of QR credentials, or support for every WhatsApp Business app account. The [business guide](business-messaging.md) links the primary references and explains the prerequisites.

## Contributing

Before implementing a roadmap item, propose its scope, resource budget, supported platforms, and validation examples. Use synthetic conversations and documents in tests. Keep the authoritative guide and [changelog](../CHANGELOG.md#unreleased) current as behavior changes; see [development](development.md) and [contributing](../CONTRIBUTING.md).
