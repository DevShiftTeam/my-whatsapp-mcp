# Optional models and media

The ordinary source build provides text import and keyword search. Semantic search, image OCR, image descriptions, and native whisper.cpp transcription are off unless a trusted native bundle is prepared and the owner explicitly enables them.

Hebrew voice transcription has a separate, optional [faster-whisper setup](transcription.md). It works with the ordinary source build and does not require the native bundle or semantic search. Native preview settings do not control that service.

Downloading model weights alone does not provide the required native runtimes. No supported one-command native bundle installer is included. Use text search for the normal local setup.

## Components

| Feature | Implementation | Current limitation |
| --- | --- | --- |
| Semantic search | ONNX Runtime, tokenizers, multilingual-e5-small | Experimental profiles; memory and numerical checks have not produced a selected release profile. |
| Image text | Tesseract with Hebrew and English language data | Needs the bundled OCR worker and verified language files. |
| Image descriptions | llama.cpp with a small vision model | Generated descriptions may omit details or be wrong. |
| Native voice transcription | whisper.cpp with separate Hebrew and English profiles | Hebrew and mixed-language quality need human evaluation; the English-only profile does not support Hebrew. For faster-whisper, see the [separate guide](transcription.md). |
| Audio decoding | Restricted FFmpeg build | Only the declared codecs and container formats are available. |

The model catalog records pinned revisions, sizes, hashes, and licenses. Model operations are explicit owner actions. The app does not start native executables discovered on the system's `PATH`.

For a prepared bundle, use **Models and media** to review available profiles and enable an experimental preview. Generated OCR, descriptions, and transcripts are stored separately from original message text and marked with their source. Search can fall back to keyword mode when semantic processing is unavailable.

## Storage

Original media has a configurable disk budget, initially 2 GiB. Change it in **Models and media → Original media storage**, or set it at launch:

```sh
./bin/wa-indexer serve --originals-budget 2GiB
```

The permitted range is 64 MiB to 1 TiB. Omitting the argument preserves the saved limit. Lowering the budget does not delete files immediately; downloads may wait for cleanup. Stored message text remains independently searchable.

## Working on native support

See [development](development.md) and [macOS packaging](../packaging/macos/README.md). The manifests under `packaging/` are inputs and provenance records, not complete release artifacts. Preserve upstream notices and review the licenses for the exact binaries and weights you distribute.
