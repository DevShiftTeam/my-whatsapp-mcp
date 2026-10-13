---
title: Desktop
description: Build and run My WhatsApp on a Mac.
---
# Desktop

The local app currently targets **macOS ARM64**. You need to build it from source; there is no signed app download yet. Follow [Local setup](../setup.md) for build tools and commands. Native Windows is not supported; other desktop platforms have no confirmed packaged installation.

## Try the demo

After building, run this from the source checkout:

```sh
bin/wa-indexer demo
```

The browser opens automatically. Save your choices, then use the WhatsApp connection card's button (**Link WhatsApp** or **Connection details**) and choose **Load synthetic messages**. Search the sample Hebrew and English conversations. The demo uses a separate archive and never connects to your WhatsApp account. Its sample transcripts and image text are examples.

Stop the demo with Ctrl+C before starting your own archive.

## Start your archive

```sh
bin/wa-indexer serve
```

Follow [First run](../first-run.md) to choose chats and dates, then link your phone. Keep the app process running; closing the browser tab does not stop it. Check [compatibility](../compatibility.md) before depending on the archive as a backup.

## Find your data

On macOS, the default directory is `~/Library/Application Support/WA Indexer`. A custom `--data-dir` must be an absolute path to private local storage. Use the same directory on later launches, keep it outside shared or cloud-synced folders, and protect it with disk encryption.

Use `--no-open` to skip automatic browser opening. If needed, open the app's private `owner-url` on the same computer. Keep that link private. Starting the same archive again normally opens its existing owner interface.

## Add optional features

Keyword search works without models. Follow [Voice transcription](../transcription.md) for the optional local speech service, or [Connect an AI client](../mcp.md) for Codex and Claude Code. Semantic search and image processing remain [experimental](../native-models.md).
