# Compare the options

Checked 2026-10-11. This is a comparison of documented capabilities, not a performance benchmark. The linked project READMEs are fixed snapshots of the sources for the competitor columns; “Not described” means the cited README does not document the feature, not that it is unavailable.

| Capability | My WhatsApp | [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp/blob/7d6a06dcdce1f01dfb24f60e1030d5efba9f3b88/README.md) | [simonseifert/whatsapp-mcp-pro](https://github.com/simonseifert/whatsapp-mcp-pro/blob/1efe9a860e0fdde6953b9252f32d9915c0b9a10c/README.md) |
| --- | --- | --- | --- |
| Local archive | SQLite, selected chats and dates, configurable original-media budget | Local SQLite | Local SQLite |
| Search | Keyword search, contact/group names, filters and conversation context | Contacts, chats, filtered messages and conversation context | Keyword search and multilingual semantic recall |
| Voice transcripts | Optional local faster-whisper service; searchable transcripts; multilingual and Hebrew profiles | Not described | Optional local mlx-whisper or faster-whisper, or cloud Groq; searchable transcripts |
| Semantic status | Experimental native profiles; off by default; no selected release profile | Not described | Optional embedding recall with a background indexer |
| Remote MCP | Streamable HTTP with bearer credentials; HTTPS tunnel or proxy configured by the owner | Remote setup not described; local Python MCP configuration documented | Shared Streamable HTTP with bearer tokens; local stdio also available |
| Permissions | Per-client read chats, dates and expiry; separate send mode: none, selected chats, or all within read scope and archive policy | Per-client scopes not described | Full/read-only tokens and a send-chat allowlist |
| User interface | Browser archive, Stats, client permissions; optional password-protected remote administration | Owner dashboard not described | Optional chat web client and admin panel; README warns the chat client has no login of its own and should stay on localhost or a VPN |
| Sending and media | Permission-controlled text, media and voice sends; document search by metadata, caption and nearby messages; original downloads up to 8 MiB per document | Individual/group sends, files, voice messages and media downloads returning a local path | Sending, groups, media downloads and an optional web client with scheduled sends |

## Choose for your workflow

My WhatsApp focuses on an owner-managed archive with separate access for each AI client. Start with [local setup](setup.md): ordinary message import and keyword search need no Python runtime or models. The optional [transcription service](transcription.md) adds CPU speech recognition and about 1.6 GB of model files, plus its Python environment and installation cache. [Native semantic and image models](native-models.md) remain experimental.

The lharries project documents a Go bridge and a Python MCP server for reading, searching and sending through a local AI client. The pro fork documents a broader automation toolkit, shared HTTP service, semantic recall, local or cloud transcription, and optional chat, assistant and dispatch interfaces. Review each project's setup and security guidance for the workflow you need.

Official WhatsApp also provides [keyword, date, media-type and group search](https://faq.whatsapp.com/1131773267485499/?cms_platform=web), plus [on-device voice transcripts](https://faq.whatsapp.com/241617298315321/?cms_platform=iphone) whose availability and languages depend on the device and OS. Those help pages do not describe a user-hosted MCP archive interface.

## Boundaries that matter

Local processing does not make WhatsApp sync or a remote AI client offline. My WhatsApp can search retained text and run its configured speech service locally after installation; model downloads, WhatsApp sync and remote access need network connectivity. Data returned to an AI client is subject to that client's handling.

The archive's SQLite databases are not encrypted by the app. Linked-device history is limited to what WhatsApp delivers, and generated transcripts can contain errors. See [security](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/SECURITY.md) and [transcription limits](transcription.md#storage-and-resource-limits).

Document retrieval is available today, but document-body text extraction and indexing are planned. Current [attachment search](attachments.md) uses filenames, types, captions and nearby messages, then returns the original; it does not search inside a PDF. See the [roadmap](roadmap.md) for file parsing and future work.

If an accompanying infographic is included, treat it as an illustrated snapshot of this dated table, not a benchmark or a claim that any feature is unique.
