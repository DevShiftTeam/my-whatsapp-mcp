---
title: Desktop
description: Use an existing development executable on the current macOS ARM64 target.
---
# Desktop

The current target is **macOS ARM64**. A qualified `.app`, signed download and clean-machine installation have not been produced. Native Windows support, Linux desktop packaging and macOS AMD64 are unqualified. If you need an installable release, check [compatibility](../compatibility.md) before proceeding.

## Use a built development executable

These examples assume a developer has already built `bin/wa-indexer` in the source checkout. They do not install or download an app. From that checkout, explore a separate synthetic archive:

```sh
bin/wa-indexer demo
```

The private browser session opens automatically. Save choices, then choose **Manage connection → Load synthetic messages**. Search Hebrew and English samples. Demo never pairs a real WhatsApp account; sample extraction text does not establish native AI quality. Stop with Ctrl-C before switching to a real archive.

Launch the implemented real-account workflow:

```sh
bin/wa-indexer serve
```

Continue with [first run](../first-run.md). Real pairing, history coverage and restart behavior remain unverified. Keep the app process running; closing a browser tab does not stop it.

## Find your private data

On macOS the default root is `~/Library/Application Support/My WhatsApp`. A custom `--data-dir` must be an absolute private local directory; reuse the same root on later launches. Keep it outside shared or cloud-synced folders. App data includes the linked session and client material; use disk encryption.

`--no-open` suppresses automatic browser opening. Open the app-managed private `owner-url` on this computer if needed; never share its contents. The owner listener accepts literal loopback addresses only. Starting the same archive again normally attaches to its existing owner interface.

## Local AI and client limits

Keyword search works without downloaded models. No native AI bundle is qualified and the accepted profile allowlist is empty. [Media and models](../everyday/media-and-models.md) explains preview eligibility. Local client configuration is implemented, but exact-client calls need separate verification. A future packaged app is intended to run without a developer toolchain.
