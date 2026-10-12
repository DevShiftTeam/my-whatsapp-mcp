---
title: Search saved messages
description: Find Hebrew and English text and read the original context.
---
# Search saved messages

Search messages that have actually reached your archive. Keyword search works without AI models. Enter a remembered name, word, phrase, place or number in **Find a message**; choose a result to read neighboring messages in **Conversation**.

## Hebrew and English

You can search Hebrew or English regardless of the text in nearby messages. Hebrew keyword normalization supports pointed and unpointed forms: `שלום` can match `שָׁלוֹם`. The displayed body retains its original text and order. Matching text does not prove translation or semantic equivalence across languages; native multilingual relevance remains unqualified.

Use **Filters** for a chat and inclusive **From**/**Through** dates. **Show more messages** retrieves another bounded page. If the app says the archive changed, rerun the search before continuing.

## Original and derived text

The original WhatsApp body/caption is distinct from **Text read from image (OCR)**, **Image description (generated)** and **Voice transcript**. Transcript timing is labeled when available. Derived text can be inaccurate; read the original context before relying on it. **Copy visible messages** copies the displayed body/caption and separately labeled extraction text.

Current native extraction quality is unqualified. Synthetic demo extraction is sample text, not proof of a working model. See [media and models](media-and-models.md).

## Know which control changes what

| Control | Effect |
| --- | --- |
| Archive settings | Retains or removes stored messages according to policy |
| Search filters | Narrows the view of already retained messages |
| AI connection scope | Restricts what one AI client may read |

Semantic search needs an eligible encoder and a current semantic index. If inference is unavailable or fails, keyword fallback remains the useful path. Installing a resource does not activate it.

## When a search is empty

Check selected chats, stored counts, dates and import state. An empty result can mean the message never arrived, was filtered out, was removed by retention, or is outside this view. A bounded result page is not an exhaustive-history claim. Use [first-run coverage checks](../first-run.md#check-observed-coverage) and [troubleshooting](../troubleshooting.md) before broadening archive policy or downloading models.
