---
title: My WhatsApp user guide
description: Use the development owner interface and understand archive and client limits.
---
# My WhatsApp user guide

Use this guide after [installing from source](setup.md) or [starting Docker Compose](install/docker-compose.md). There is no downloadable desktop app yet; see [compatibility](compatibility.md) for platform and testing limits.

## Start locally

The current build target is macOS ARM64. These commands assume an already-built development executable at `bin/wa-indexer` in the source checkout. See [Desktop](install/desktop.md) for availability and [the repository README development section](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/README.md#development) for build prerequisites. End-user installation does not require a developer toolchain.

For a first look without connecting WhatsApp:

```sh
bin/wa-indexer demo
```

The browser opens a private owner session. Save archive choices, open **Manage connection**, then choose **Load synthetic messages**. Search the sample Hebrew and English conversations. Demo has its own data directory and never displays a real pairing code. The demo transcripts and image text are samples. Stop the process with Ctrl-C before starting your real archive.

Start the real-account workflow with:

```sh
bin/wa-indexer serve
```

Keep the app process running while using the browser or an AI connection. Closing a browser tab does not stop the app. Starting the same archive again normally opens its existing owner interface rather than starting another archive writer.

By default, data is stored outside the source checkout in your OS application-data directory (`~/Library/Application Support/My WhatsApp` on macOS). `--data-dir /absolute/local/directory` selects a different private data root; use an empty/new directory or an existing My WhatsApp root. Keep it on local storage outside cloud synchronization. Use the same data directory on later launches.

`--no-open` suppresses automatic browser opening. The app manages a private owner link in its data directory; keep that link private. `--listen` accepts only a literal loopback address, such as `127.0.0.1:0`; the owner interface is for this computer.

## Choose what to keep, then link WhatsApp

1. Open **Archive settings** and choose **Last 30 days**, **Last 90 days**, **A date I choose**, or **All available history**. The start date is inclusive.
2. Choose **All chats** or **Only chats I select**, then **Save choices**. With no selected chat, selective mode saves no message bodies. Chat discovery retains metadata while discarding bodies until their chat is selected.
3. Open **Manage connection → Show linking code**.
4. On your phone, open **WhatsApp → Linked devices → Link a device** and scan the private QR code. Use an account you own or have permission to archive. If the code expires, request a new one.
5. Watch **WhatsApp**, **History received**, and **Keyword search** in the status strip. Search becomes useful as allowed messages arrive.

**All available history** means the history WhatsApp supplies to this linked device. The oldest received message is not proof that import is complete. Messages discarded before you selected a chat may be unavailable later. Older-history/backfill and retry wiring is still under development; do not rely on expanding a filter to restore removed or never-received messages.

Saving fewer chats or a later import date removes saved messages outside those choices. **Optional ongoing retention** is a separate deletion rule: `0` keeps imported messages; a positive number keeps messages within that many days. Retention continues to constrain search and AI-client access.

## Search and read context

Enter a remembered name, place, word, phrase, or number in **Find a message**. Hebrew and English are supported. For example, `שלום` can match `שָׁלוֹם`; the displayed message keeps its original text and order.

Use **Filters** to select a chat and inclusive **From**/**Through** dates. Choose a result to read neighboring messages in **Conversation**. **Copy visible messages** copies the displayed original body/caption and separately labeled extraction text, when present. **Show more messages** requests another bounded page; rerun the search if the app says the archive changed.

These controls have different purposes:

| Control | What it changes |
| --- | --- |
| Archive settings | Which messages are retained in your archive |
| Search filters | Which already-retained messages this search displays |
| AI connection scope | Which retained messages one client may read |

Keyword search works without AI models. Semantic search requires an eligible encoder and a current semantic index; unavailable or failed inference uses keyword fallback. Results cover only the messages retained in your archive.

OCR appears as **Text read from image (OCR)**, generated captions as **Image description (generated)**, and speech output as **Voice transcript**. They are separate from the original WhatsApp body/caption; transcript timing is labeled when available. Native image processing is experimental; generated text may contain errors.

## Original media storage

Open **Models and media → Original media storage**. The app shows charged files/reservations and the persisted storage limit. Choose a preset or a whole number of MiB/GiB, then **Save media storage limit**.

The default is **2 GiB**; the allowed range is **64 MiB to 1 TiB**. This is the shared budget for original images and voice media across the archive. It is not the total application disk footprint or a RAM limit: models, the archive database, and temporary decoded work have separate storage needs.

Original files are retained when WhatsApp makes them available and admission succeeds within the configured limits. Retaining originals allows eligible processing to reuse the file later. It does not guarantee every attachment is recoverable, and the current interface does not promise an original-media viewer or player.

Lowering the limit does not immediately delete files or cancel accounting for work still using them. New originals can wait while safe cleanup releases space; saved message text remains searchable. At the limit, check storage pressure or choose a larger limit. A larger total budget does not override per-file or decoder limits.

The CLI can explicitly change the same persisted budget at launch:

```sh
bin/wa-indexer serve --originals-budget 8GiB
```

Stop the existing process before applying this launch override. Omit the flag on later launches to preserve the stored choice; omission does not reset it to 2 GiB. If another owner window changes the budget, refresh and review the latest value before saving again.

## Models and local AI

Open **Models and media** to see pinned resource sizes, installation state, and available processing options. **Download pinned resource** is an explicit network action. **Verify local resource** checks the selected installed resource. Downloading or verifying does not enable processing.

Under **Local AI**, the saved choice starts **Off**. Select only components the app offers for the selected mode, then **Apply local AI choice**. Off stops AI work after safe cleanup and keeps saved messages, media, and search data. An unavailable saved choice stays saved; install/verify eligible resources and explicitly retry it when available.

**Current availability:** there is no supported native AI bundle yet. Ordinary source builds use keyword search. Downloading a model does not enable a disabled option. Use only the resources offered by the app.

The model catalog includes experimental options for semantic search, OCR, image descriptions, and speech. Some candidates still have unresolved memory and output checks. Hebrew quality and use on an 8 GiB computer have not been confirmed. See [media and models](everyday/media-and-models.md) and [compatibility](compatibility.md).

With the app stopped, the CLI also supports `wa-indexer models install PROFILE_ID` and `wa-indexer models verify PROFILE_ID`. Use the exact ID shown in the catalog, the same `--data-dir`, and stop the running app first. Installation may download missing pinned resources. These commands manage resources; they do not activate local AI. Remove an in-use resource through the owner controls after turning off its work and allowing cleanup to finish.

## Connect an AI client

An AI connection exposes four bounded read-only MCP tools: `search_messages`, `get_context`, `list_chats`, and `get_index_status`. It cannot change archive settings or send WhatsApp messages. The receiving AI app may send returned text to its own service.

Open **AI connections**, choose the exact client and connection mode, name the connection, select its chats/dates, and set a future expiry. **Only chats I select** requires at least one included chat. **All archived chats, current and future** explicitly includes every chat you archive now or later, within the selected dates. Blank date fields permit all dates that your archive keeps; they never expand the archive itself.

Create a separate connection for each client. Choose **Show setup instructions** and copy the generated configuration for that client. Paste the generated settings into that client yourself. Actual AI-client connections still need testing; start with a harmless query in a permitted chat.

### Local connection

Choose **Local, on this computer** for a supported local client. The app creates a protected version 2 credential file and manages its private Unix socket. The generated command is:

```text
wa-indexer mcp stdio --config ABSOLUTE_PRIVATE_CONFIG_PATH
```

Copy the actual generated executable path and separate arguments into your chosen client's configuration. Keep the app running. The adapter forwards to that running app; it does not start another WhatsApp session, archive, or model worker. No local port choice or token copying is needed. Leave the generated file contents and permissions unchanged.

Use the [Codex guide](clients/codex.md) or [Claude Code guide](clients/claude-code.md) for that exact host. A client configuration being accepted is not a successful tool call; CLI and desktop surfaces need separate verification.

### Remote bearer connection and manual tunnel

Remote access is optional and self-hosted. It needs your running computer/app, a dedicated MCP listener, a public HTTPS hostname ending in `/mcp`, and a tunnel you configure manually. **Expose only the dedicated MCP listener. Keep the private owner interface, QR pairing, and administration outside the tunnel.**

Stop the existing app first. In a source build, replace the example hostname with your own canonical public HTTPS hostname and start:

```sh
bin/wa-indexer serve \
  --mcp-listen 127.0.0.1:8124 \
  --mcp-public-url https://wa-mcp.example.com/mcp
```

Both flags are required together. The listener is a fixed literal loopback address; the public URL has no credentials, query string, or fragment. Include your usual `--data-dir` if you use a custom archive location. Changing these flags requires restarting; a second launch does not change the running route.

Separately configure your chosen HTTPS tunnel to reach that dedicated loopback origin. Follow your provider's setup for your own domain/account; My WhatsApp does not create, activate, or change it. The app records the public URL but does not provision or prove tunnel reachability.

In **AI connections**, choose **Remote, configured by this app**, create a separate scoped connection, and choose **Reveal remote token**. In **Copy remote access token**, use **Copy token and close**; paste it only into the selected client's protected credential setting. Closing the dialog clears its token field. Tokens never belong in URLs, screenshots, command arguments, or shared configuration. Keep the generated native file out of remote setup; local and remote credentials are separate.

For Codex and Claude Code, follow the generated guide's dedicated `WA_INDEXER_CLIENT_TOKEN` variable in the actual client process. It contains the raw My WhatsApp token; the guide supplies the appropriate Bearer formatting. An environment export in an unrelated terminal does not configure a desktop process.

Hosted Claude header support requires the owner's actual account capability and the explicit `--mcp-header-auth-available` launch choice; that flag only records the owner's declaration. Actual hosted-header and Composio outbound-request behavior, and configured-client tool calls, remain unverified. This launch path configures bearer mode; it does not provide a qualified OAuth route. See [remote requirements](clients/remote-access.md) and the client-specific guides before manual setup.

### Check access and revoke

In the chosen client, try searching a phrase from a chat/date you allowed and open the returned context. Confirm excluded chats/dates are unavailable. Exercise all twelve tools before treating the connection as tested.

Use the connection's **Revoke access** control in My WhatsApp when finished, and check that the old connection can no longer read through any tool. Then remove its client configuration. Removing a configuration or closing the token dialog alone does not revoke server access. Expired connections also stop granting access.

## Pause, disconnect, or delete

**Pause import** stops import while keeping saved messages searchable, provided the archive is safe to read. **Resume import** resumes the configured connection. **Disconnect WhatsApp** keeps the local archive. These actions do not establish that offline changes or missing history have been recovered.

**Archive settings → Delete all local data** requires typing `DELETE LOCAL DATA`. It removes the local archive, linked session, retained media, models, and client material after the app stops their use. It does not delete messages in WhatsApp. Treat it as deletion, not as a way to repair missing history.

If the app reports **Needs attention** or reconciliation required, reads can remain blocked until safe recovery is established. Do not remove private state markers or modify database files to bypass it.

For a local diagnostic report, stop the running app and use the same data directory:

```sh
bin/wa-indexer doctor
```

`doctor` checks the saved archive while the app is stopped. Share only sanitized codes/status when reporting a problem; keep message bodies, QR content, private owner links, tokens, and session files private.

## License

My WhatsApp is licensed under [GPL-3.0-only](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/LICENSE). Bundled and optional components have their own [third-party notices](https://github.com/DevShiftTeam/my-whatsapp-mcp/blob/main/THIRD_PARTY_NOTICES).
