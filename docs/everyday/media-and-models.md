---
title: Media and models
description: Keep originals separate and activate local AI explicitly.
---
# Media and models

Open **Models and media** to manage retained originals and pinned resources. Original WhatsApp text and files remain separate from OCR, image descriptions and voice transcripts. Generated output may be wrong; preserve the distinction when copying or sharing results.

## Set an original-media budget

The default limit is **2 GiB**, with an allowed range of **64 MiB to 1 TiB**. Choose a preset or a whole number of MiB/GiB and select **Save media storage limit**. The saved limit persists across restarts. It covers original image and voice storage, not total app disk use or RAM; databases, models and temporary decoded work need additional space.

Lowering the budget does not immediately delete retained files. The UI shows charged files and reservations; a full budget can prevent new originals from being retained. WhatsApp availability and the archive policy also limit which media can be downloaded. Keep originals on private local storage.

## Download, verify, then choose processing

**Download pinned resource** explicitly uses the network. **Verify local resource** checks the installed resource. Neither activates AI. Under **Local AI**, the saved default is **Off**; choose only offered components, then **Apply local AI choice**. Off stops AI work after safe cleanup and preserves saved messages and search data.

An unavailable saved choice stays saved. Install and verify eligible resources, then explicitly retry activation. Do not substitute arbitrary runtime paths or system decoder binaries. Remove an in-use resource only through owner controls after turning its work off and waiting for cleanup.

## Current native limits

No qualified native bundle exists and the accepted profile allowlist is empty. Ordinary source builds offer keyword search; an installed model cannot make a disabled option eligible. Preview needs a trusted eligible bundle and remains experimental.

The catalog contains multilingual encoders, Hebrew/English OCR, image-description and speech candidates. FP32 memory and experimental INT8 numerical-reference failures remain open. Hebrew/media quality, complete worker resources and 8 GiB suitability are unverified. [Compatibility](../compatibility.md) summarizes these limits.

## Resource commands for an existing executable

With the app stopped, a built executable supports `wa-indexer models install PROFILE_ID` and `wa-indexer models verify PROFILE_ID`. Replace `PROFILE_ID` with the exact catalog ID and use the same `--data-dir`. These manage pinned resources; they do not activate AI. Keep [a full backup](../maintenance/backups.md) before significant changes.
