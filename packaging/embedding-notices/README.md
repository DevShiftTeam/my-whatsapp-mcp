# Embedding component notices

The manifests in `packaging/` pin experimental embedding profiles. They do not
provide a complete native bundle, and model initialization does not download
artifacts.

* ONNX Runtime 1.23.2: MIT, with the original LICENSE and ThirdPartyNotices.txt
  copied from the checksum-verified Microsoft Darwin ARM64 release archive.
* daulet/tokenizers Go binding v1.27.0: MIT. The included LICENSE is from that
  exact upstream Git tag. Its prebuilt Rust/native release library embeds Hugging
  Face tokenizers 0.22; the HF tokenizer implementation is Apache-2.0 and the
  official HF 0.22.1 Apache-2.0 license is also included. These are distinct
  components; the Go binding's MIT license does not replace the native notices.
* intfloat/multilingual-e5-small revision
  614241f622f53c4eeff9890bdc4f31cfecc418b3: publisher's model card declares MIT:
  https://huggingface.co/intfloat/multilingual-e5-small/blob/614241f622f53c4eeff9890bdc4f31cfecc418b3/README.md
  The repository has no standalone LICENSE file at this revision. Do not invent
  one or claim the downstream application's full license audit is complete.
* Go bindings are pinned by go.mod/go.sum. Preserve their upstream notices when
  distributing them. SQLite/sqlite-vec notices are in internal/sqlitevec.

The static Rust library's complete transitive dependency and license inventory
has not been audited. These component notices do not establish that a complete
native bundle is ready for redistribution. The delivered app does not require
Python or developer tools for native inference.

The experimental INT8 profile was derived from the pinned FP32 model. Its
numerical comparison checks failed, so it is not a selected release profile.
Its manifest preserves the historical recipe path and checksum for provenance;
that preparation script is not included in this source release.
Cross-platform native execution and human Hebrew quality remain unverified.
