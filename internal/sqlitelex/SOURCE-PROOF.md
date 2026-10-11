# Exact lexical scorer: source packet

This package is new and remains unimported by the existing production archive.
Only `internal/sqlitelex/**` was written. Existing archive/sqlitevec/module/Git
inputs were not edited. Root permits source-only work and pinned gofmt; no Go,
C compiler, SQLite query, native execution, test, benchmark or profiler ran.
Tests were written before the implementation, but runtime RED/GREEN is
**NOT_RUN** and must not be invented afterward. Current common-all 100k keyword
p95 remains **217.8 ms FAIL** against 100 ms.

Authority is the source/security-approved proposal
`task-11-keyword-next-optimization-source-proposal-20261007-v2.md`, SHA-256
`ba3a12fa4de18b32991931eb50fe75fa2b1a27d4e4bfb07ba6d46a7ba7df7657`.
Existing CODE/SEC seats review this exact source packet. Source approval and
gofmt do not establish runtime correctness, cancellation latency or a speedup.

## Contract and caller boundary

`Register()` statically registers `wa_doc_density_v1(text, term1, ..., termN)`
through `sqlite3_auto_extension`, following existing sqlitevec registration.
It links the one mattn/go-sqlite3 SQLite; this package supplies no SQLite
implementation, dynamically loaded library, SQL handle or per-row Go callback.
Registration must succeed before opening either writer or reader. Subsequent
connections install the function during SQLite open, and installation errors
fail that open. Non-cgo builds fail registration closed.

The scalar accepts 2..33 arguments, all TEXT and strict complete UTF-8. Text
has a 1,048,577-byte hard cap. Each of 1..32 terms is nonempty, NUL-free,
at most 512 bytes and 128 Unicode code points. Duplicates remain significant.
Input is used verbatim: no lowercase, normalization, FTS tokenization or trim.
The first input NUL ends its scoring prefix, as SQLite `length(TEXT)` does;
UTF-8 validation and extent checks still include the complete suffix.

The fixed native malformed-input error is `sqlitelex_invalid_input`.
`Register` failures are `sqlitelex_registration_failed`; archive integration
must map them to its existing fixed availability error. Interrupted callbacks
return SQLITE_INTERRUPT and never a partial score.

Function flags are UTF8 | DETERMINISTIC | INNOCUOUS | DIRECTONLY. The production
caller **must** wrap every invocation in a lazy CASE containing the complete
current branch authority, including all relationship, account, policy, scope,
state, clock, root-member and media checks. WHERE textual order is insufficient.
Role-specific original/root/window extent/type checks belong inside that gate
before native invocation; malformed authorized operands may use the approved
NULL sentinel to produce the fixed error. Denied operands must not invoke native
validation. MATCH remains in its indexed WHERE position. This package's generic
CASE test is not proof of the separate archive statements; Reader owns those
integration tests and the held Store.Open/search/root/candidate edits.

## Bounds and exact arithmetic proof obligations

The original body+caption ingress cap is 256 KiB. Existing Go lexical mapping
outputs at most one rune per input rune, with at most four UTF-8 bytes per output
rune; input rune count cannot exceed original bytes. Thus joined normalized
body+caption needs at most `4*256KiB+1` bytes, including the unchanged single
space. A 64 KiB root/window role similarly needs at most `4*64KiB`; integration
must preserve the proven cap for each actual document role. The scalar's generic
cap must not replace those tighter role checks.

Strict valid UTF-8 and NUL-free terms ensure a BINARY full-term match removes
exactly its rune count within the first-NUL prefix. KMP resets after a match,
matching SQLite replace's nonoverlapping removal. For each term, code evaluates
`removed_runes*1.0/term_runes`, then adds terms in input order, then divides by
`max(1,text_runes)`. Each product is no larger than the prefix rune count,
below 1,048,578; the sum is below 2^26. Integers therefore neither overflow int
nor lose precision in double. Positive-zero and final division follow the old
SQL. Accepted cgo `-fno-fast-math` disables reassociation/reciprocal rewriting;
source-local `#pragma STDC FP_CONTRACT OFF` expresses the contraction rule
without a cgo allowlist override. These
source arguments support the design; exact bundled-SQL float-bit/order tests
remain mandatory runtime evidence.

Scorer work is O(text bytes + sum(term bytes) + N*prefix bytes), with N<=32.
The 32 pointer/length records and one reused 512-entry uint16 KMP table are on
the callback stack. The scorer allocates no native heap, retains no body/term
pointer or auxdata, and reads no corpus statistic, SQL, file or network. SQLite
owns supplied values/encoding conversions and result/error machinery. Allocation
of the auto-extension registry/function definition is registration work, not
per-row body allocation. The separate test-only utility exposes only SQLite's
aggregate allocation counter and is never imported by registration/scoring.

The callback obtains `sqlite3_context_db_handle` only to read the live
`sqlite3_is_interrupted` flag. No handle is exported, cast to Go or retained.
Polling occurs at entry, before each validator/KMP scan, at <=4096 validation
bytes (allowing room for a four-byte sequence), at each 4096 KMP input bytes,
and before publishing the double. KMP prefix preparation is bounded by 512
bytes. Runtime cancellation/connection reuse is still NOT_RUN.

## Finite source tests

- `TestDensityExactSQLiteFloatBits`: actual old length/replace SQL oracle;
  ASCII/case, Hebrew/marks/quotes, accents, supplementary characters, overlaps,
  duplicates, body/caption boundary, empty input, first-NUL variants and max term.
- `TestDensityNormalizationWidthAndAcceptedBoundaries`: real shared lexical
  mapping's UTF-8 growth at maximum accepted original/root input; generic text
  hard boundary and 32 duplicate terms.
- `TestDensityPreservesSQLiteCompleteOrderingAndPages`: native versus old SQL
  scores and paged score/date/id/is_root ordering, including identical IDs in
  original/root roles.
- `TestDensityRejectsInvalidTypesUTF8ExtentsAndArity`: fixed errors for NULL,
  integer/float/BLOB, malformed UTF-8 including after NUL, invalid terms,
  oversized operands and invalid argument counts; no truncation.
- `TestDensityLazyCaseSkipsDeniedMalformedInputs`: lazy native-boundary CASE
  suppresses denied validation errors, while authorized score remains exact.
- `TestDensityDirectOnlyAndMultipleReadOnlyConnectionsReopen`: per-connection
  auto-registration, reopen, query-only scoring/read-only preservation and
  DIRECTONLY view denial.
- `TestDensityCancellationAndConnectionReuse`: cancel a bounded worst-case
  scanner workload, require cancellation instead of a score, then reuse the
  connection for an exact oracle read.
- `TestDensityReleasesStatementAndConnectionMemory`: varying large body/term
  bindings do not reuse stale values; repeated statement/connection closure
  returns SQLite allocation usage near its warmed baseline. Static no-malloc
  proof complements this engine counter; it is not a whole-process RSS test.

## Code cohort

Code-cohort SHA-256 is
`da72589404f22760b3dca855153b8908e8808d5f772d628bdb3cd8fd8a30d4c8`.
It hashes sorted package-relative `path + NUL + sha256 + LF` records for these
seven files. This human proof document is excluded to avoid a self-hash cycle.

| Path | SHA-256 |
| --- | --- |
| density.c | 21d77f16fc011cd6bbceecb5e9bffd1ca98a2df77d82e327c9f17322e3aebcac |
| density.h | a81761918c1cab0954054cdc6bc008baa005188b9a90de24e95071f53b4e3c58 |
| density_test.go | bbf600b852ee6d4b933955ae190f52c354ba8a6bdc6c46185a8628140d86a381 |
| internal/testsqlite/memory.go | 90acd0fe64811d1b5a206bef416934120c9ad91d24eed88e844aa1fbe7fd1519 |
| memory_test.go | 4fea1f189df3453576f917f0c17cb309b1d47edcb4a0eda7917914f6e036051c |
| register.go | 8eceb8a966c7c25ae47a097877383a8315d22170cdc8c081d38a75c2fde0053c |
| register_nocgo.go | ef22a1ad4be5f10ededc6021c9de28c9125c478df41d25a4e8754bb4e5e3bb22 |

Current readiness: **SOURCE_ONLY / CODE_STATIC_APPROVE / SEC_STATIC_APPROVE /
RUNTIME_NOT_RUN**. Prior code cohort `f627a9be...` was superseded by the CODE
finding that Go rejects `-ffp-contract=off` before C compilation. This refreshed
cohort removes the flag and uses CODE's source-local contraction pragma, while
retaining accepted `-fno-fast-math`. No CGO_CFLAGS_ALLOW override or environment
relaxation is introduced. Both existing CODE/SEC seats independently rehashed
and approved the exact seven-file `da725894...` cohort; CODE closed the prior
allowlist finding. Pinned compiler acceptance and honoring, SQL float-bit
oracle, cancellation and performance remain NOT_RUN.
The complete lazy authority
CASE and role caps remain a separate mandatory archive integration review.
The cancellation timer test covers cancellation/reuse, not a deterministic
proof of a particular in-flight poll or a measured wall-clock bound.
No integration, compile success, test pass, speedup or release acceptance is
claimed. Future execution requires root's dedicated lane grant and complete
immutable source/dependency closure.
