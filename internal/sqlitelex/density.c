//go:build cgo

#include "density.h"
#include "../sqlitevec/sqlite3.h"
#include <stdint.h>
#include <string.h>

/* Preserve the separately rounded SQLite arithmetic without a disallowed
 * cgo compiler flag. Fast-math/reassociation remains disabled by CFLAGS. */
#pragma STDC FP_CONTRACT OFF

#define WA_MAX_TEXT_BYTES (4 * 256 * 1024 + 1)
#define WA_MAX_TERMS 32
#define WA_MAX_TERM_BYTES 512
#define WA_MAX_TERM_RUNES 128
#define WA_POLL_BYTES 4096

typedef struct {
  const unsigned char *bytes;
  int size;
  int runes;
} wa_term;

static int wa_interrupted(sqlite3 *db) {
  return sqlite3_is_interrupted(db) != 0;
}

/* Validate the complete input, including bytes after a NUL. SQLite length(TEXT)
 * counts only the prefix before the first NUL; scoring uses that same prefix.
 * Every sequence is strict UTF-8 (no overlongs, surrogates or >U+10FFFF). */
static int wa_utf8_extent(const unsigned char *text, int size, sqlite3 *db,
                          int *prefix_bytes, int *prefix_runes,
                          int *full_runes, const wa_term *term, int *count) {
  uint16_t failure[WA_MAX_TERM_BYTES];
  int i = 0;
  int matched = 0;
  int found = 0;
  int prefix = 0;
  int total = 0;
  int saw_nul = 0;
  int since_poll = 0;
  *prefix_bytes = size;
  if (wa_interrupted(db)) return SQLITE_INTERRUPT;
  /* A validated single term can be counted while validating/rune-counting the
   * text. A null term preserves the validator-only path used by all other
   * calls. Build the same bounded KMP table and reset after each full match. */
  if (term != 0) {
    int j;
    failure[0] = 0;
    for (j = 1; j < term->size; j++) {
      while (matched > 0 && term->bytes[j] != term->bytes[matched]) {
        matched = failure[matched - 1];
      }
      if (term->bytes[j] == term->bytes[matched]) matched++;
      failure[j] = (uint16_t)matched;
    }
    matched = 0;
  }

  while (i < size) {
    unsigned char lead;
    int width;
    /* Leave room for the largest sequence: never inspect >4096 input bytes
     * between cancellation flag polls, even at multibyte boundaries. */
    if (since_poll > WA_POLL_BYTES - 4) {
      if (wa_interrupted(db)) return SQLITE_INTERRUPT;
      since_poll = 0;
    }
    lead = text[i];
    if (lead <= 0x7f) {
      width = 1;
    } else if (lead >= 0xc2 && lead <= 0xdf) {
      width = 2;
    } else if (lead >= 0xe0 && lead <= 0xef) {
      width = 3;
    } else if (lead >= 0xf0 && lead <= 0xf4) {
      width = 4;
    } else {
      return SQLITE_ERROR;
    }
    if (size - i < width) return SQLITE_ERROR;
    if (width > 1 && (text[i + 1] & 0xc0) != 0x80) return SQLITE_ERROR;
    if (width > 2 && (text[i + 2] & 0xc0) != 0x80) return SQLITE_ERROR;
    if (width > 3 && (text[i + 3] & 0xc0) != 0x80) return SQLITE_ERROR;
    if (lead == 0xe0 && text[i + 1] < 0xa0) return SQLITE_ERROR;
    if (lead == 0xed && text[i + 1] >= 0xa0) return SQLITE_ERROR;
    if (lead == 0xf0 && text[i + 1] < 0x90) return SQLITE_ERROR;
    if (lead == 0xf4 && text[i + 1] > 0x8f) return SQLITE_ERROR;

    if (lead == 0 && !saw_nul) {
      *prefix_bytes = i;
      saw_nul = 1;
    }
    if (!saw_nul) {
      prefix++;
      if (term != 0) {
        int j;
        for (j = 0; j < width; j++) {
          unsigned char byte = text[i + j];
          while (matched > 0 && byte != term->bytes[matched]) {
            matched = failure[matched - 1];
          }
          if (byte == term->bytes[matched]) matched++;
          if (matched == term->size) {
            found++;
            matched = 0;
          }
        }
      }
    }
    total++;
    i += width;
    since_poll += width;
  }
  *prefix_runes = prefix;
  *full_runes = total;
  if (count != 0) *count = found;
  return SQLITE_OK;
}

/* KMP bounds work by text+term bytes, including adversarial repeated prefixes.
 * Reset after a full match to reproduce replace()'s non-overlapping removal.
 * All pointers and the bounded prefix table are callback-local; no allocation,
 * SQLite lookup, persistent cache, logging or normalization occurs here. */
static int wa_count(const unsigned char *text, int size, const wa_term *term,
                    sqlite3 *db, int *count) {
  uint16_t failure[WA_MAX_TERM_BYTES];
  int i;
  int matched = 0;
  int found = 0;
  if (wa_interrupted(db)) return SQLITE_INTERRUPT;
  failure[0] = 0;
  for (i = 1; i < term->size; i++) {
    while (matched > 0 && term->bytes[i] != term->bytes[matched]) {
      matched = failure[matched - 1];
    }
    if (term->bytes[i] == term->bytes[matched]) matched++;
    failure[i] = (uint16_t)matched;
  }
  matched = 0;
  for (i = 0; i < size; i++) {
    if ((i & (WA_POLL_BYTES - 1)) == 0 && wa_interrupted(db)) {
      return SQLITE_INTERRUPT;
    }
    while (matched > 0 && text[i] != term->bytes[matched]) {
      matched = failure[matched - 1];
    }
    if (text[i] == term->bytes[matched]) matched++;
    if (matched == term->size) {
      found++;
      matched = 0;
    }
  }
  *count = found;
  return SQLITE_OK;
}

static void wa_density_error(sqlite3_context *ctx, int rc) {
  if (rc == SQLITE_INTERRUPT) {
    sqlite3_result_error_code(ctx, SQLITE_INTERRUPT);
  } else {
    sqlite3_result_error(ctx, "sqlitelex_invalid_input", -1);
  }
}

static void wa_density(sqlite3_context *ctx, int argc, sqlite3_value **argv) {
  sqlite3 *db = sqlite3_context_db_handle(ctx);
  const unsigned char *text;
  wa_term terms[WA_MAX_TERMS];
  int text_size;
  int prefix_bytes;
  int text_runes;
  int ignored_runes;
  int single_count = 0;
  int rc;
  int i;
  double total = 0.0;

  if (wa_interrupted(db)) {
    wa_density_error(ctx, SQLITE_INTERRUPT);
    return;
  }
  if (argc < 2 || argc > WA_MAX_TERMS + 1) {
    wa_density_error(ctx, SQLITE_ERROR);
    return;
  }
  for (i = 0; i < argc; i++) {
    if (sqlite3_value_type(argv[i]) != SQLITE_TEXT) {
      wa_density_error(ctx, SQLITE_ERROR);
      return;
    }
  }
  text = sqlite3_value_text(argv[0]);
  text_size = sqlite3_value_bytes(argv[0]);
  if (text == 0 || text_size < 0 || text_size > WA_MAX_TEXT_BYTES) {
    wa_density_error(ctx, SQLITE_ERROR);
    return;
  }
  if (argc != 2) {
    rc = wa_utf8_extent(text, text_size, db, &prefix_bytes, &text_runes,
                        &ignored_runes, 0, 0);
    if (rc != SQLITE_OK) {
      wa_density_error(ctx, rc);
      return;
    }
  }
  for (i = 1; i < argc; i++) {
    int term_prefix;
    int term_prefix_runes;
    wa_term *term = &terms[i - 1];
    term->bytes = sqlite3_value_text(argv[i]);
    term->size = sqlite3_value_bytes(argv[i]);
    if (term->bytes == 0 || term->size < 1 ||
        term->size > WA_MAX_TERM_BYTES) {
      wa_density_error(ctx, SQLITE_ERROR);
      return;
    }
    rc = wa_utf8_extent(term->bytes, term->size, db, &term_prefix,
                        &term_prefix_runes, &term->runes, 0, 0);
    if (rc != SQLITE_OK) {
      wa_density_error(ctx, rc);
      return;
    }
    if (term_prefix != term->size || term->runes < 1 ||
        term->runes > WA_MAX_TERM_RUNES) {
      wa_density_error(ctx, SQLITE_ERROR);
      return;
    }
  }
  if (argc == 2) {
    rc = wa_utf8_extent(text, text_size, db, &prefix_bytes, &text_runes,
                        &ignored_runes, &terms[0], &single_count);
    if (rc != SQLITE_OK) {
      wa_density_error(ctx, rc);
      return;
    }
  }

  for (i = 0; i < argc - 1; i++) {
    int count;
    int removed_runes;
    double piece;
    if (argc == 2) {
      count = single_count;
    } else {
      rc = wa_count(text, prefix_bytes, &terms[i], db, &count);
      if (rc != SQLITE_OK) {
        wa_density_error(ctx, rc);
        return;
      }
    }
    /* This product is <=text_runes<=WA_MAX_TEXT_BYTES, hence cannot overflow
     * int or lose integer precision in double. Preserve the old SQL's order:
     * (length(text)-length(replace(text,term,'')))*1.0/length(term), then
     * left-to-right term addition, then division by max(1,length(text)). */
    removed_runes = count * terms[i].runes;
    piece = ((double)removed_runes * 1.0) / (double)terms[i].runes;
    total = total + piece;
  }
  if (wa_interrupted(db)) {
    wa_density_error(ctx, SQLITE_INTERRUPT);
    return;
  }
  sqlite3_result_double(ctx, total / (double)(text_runes > 0 ? text_runes : 1));
}


/* Token scoring needs only complete UTF-8 validity, not prefix/rune counts.
 * Copy eight bytes into a local word to skip ASCII (including NUL) without
 * unaligned access or aliasing assumptions. Never read past the supplied size.
 * Count even a failed ASCII probe toward the cancellation inspection budget;
 * strict multibyte checks reserve four more bytes before reading a sequence.
 * No allocation/cache/tokenization occurs, and NUL never stops validation. */
static int wa_utf8_validate_only(const unsigned char *text, int size,
                                 sqlite3 *db) {
  int i = 0;
  int since_poll = 0;
  if (wa_interrupted(db)) return SQLITE_INTERRUPT;
  while (i < size) {
    unsigned char lead;
    int width;
    if (since_poll > WA_POLL_BYTES - 8) {
      if (wa_interrupted(db)) return SQLITE_INTERRUPT;
      since_poll = 0;
    }
    if (size - i >= 8) {
      uint64_t word;
      memcpy(&word, text + i, sizeof(word));
      since_poll += 8;
      if ((word & UINT64_C(0x8080808080808080)) == 0) {
        i += 8;
        continue;
      }
    }
    if (since_poll > WA_POLL_BYTES - 4) {
      if (wa_interrupted(db)) return SQLITE_INTERRUPT;
      since_poll = 0;
    }
    lead = text[i];
    if (lead <= 0x7f) {
      width = 1;
    } else if (lead >= 0xc2 && lead <= 0xdf) {
      width = 2;
    } else if (lead >= 0xe0 && lead <= 0xef) {
      width = 3;
    } else if (lead >= 0xf0 && lead <= 0xf4) {
      width = 4;
    } else {
      return SQLITE_ERROR;
    }
    if (size - i < width) return SQLITE_ERROR;
    if (width > 1 && (text[i + 1] & 0xc0) != 0x80) return SQLITE_ERROR;
    if (width > 2 && (text[i + 2] & 0xc0) != 0x80) return SQLITE_ERROR;
    if (width > 3 && (text[i + 3] & 0xc0) != 0x80) return SQLITE_ERROR;
    if (lead == 0xe0 && text[i + 1] < 0xa0) return SQLITE_ERROR;
    if (lead == 0xed && text[i + 1] >= 0xa0) return SQLITE_ERROR;
    if (lead == 0xf0 && text[i + 1] < 0x90) return SQLITE_ERROR;
    if (lead == 0xf4 && text[i + 1] > 0x8f) return SQLITE_ERROR;
    i += width;
    since_poll += width;
  }
  return SQLITE_OK;
}


/* Versioned token relevance: sum current-row phrase occurrences divided by
 * current-row indexed token count. No corpus statistics, content tokenization,
 * per-query cache, KMP count, or callback-owned allocation is used here.
 * Arity selects the existing role bounds: one field is a root/window (256KiB
 * normalized), two fields are an original body/caption (1MiB plus separator).
 * All fields are validated completely, including valid/invalid NUL suffixes.
 */
static void wa_token_rank(const Fts5ExtensionApi *fts, Fts5Context *cursor,
                           sqlite3_context *ctx, int argc,
                           sqlite3_value **argv) {
  sqlite3 *db = sqlite3_context_db_handle(ctx);
  int maximum = argc == 1 ? 4 * 64 * 1024 : WA_MAX_TEXT_BYTES;
  int64_t size_sum = argc == 2 ? 1 : 0;
  int phrases;
  int tokens;
  int64_t instances = 0;
  int64_t maximum_instances;
  int rc;
  int i;
  if (wa_interrupted(db)) {
    wa_density_error(ctx, SQLITE_INTERRUPT);
    return;
  }
  if ((argc != 1 && argc != 2) || fts->iVersion < 2) {
    wa_density_error(ctx, SQLITE_ERROR);
    return;
  }
  for (i = 0; i < argc; i++) {
    int size;
    if (sqlite3_value_type(argv[i]) != SQLITE_TEXT) {
      wa_density_error(ctx, SQLITE_ERROR);
      return;
    }
    size = sqlite3_value_bytes(argv[i]);
    if (size < 0 || size > maximum) {
      wa_density_error(ctx, SQLITE_ERROR);
      return;
    }
    size_sum += size;
  }
  if (size_sum > maximum) {
    wa_density_error(ctx, SQLITE_ERROR);
    return;
  }
  for (i = 0; i < argc; i++) {
    const unsigned char *text = sqlite3_value_text(argv[i]);
    if (text == 0) {
      wa_density_error(ctx, SQLITE_ERROR);
      return;
    }
    rc = wa_utf8_validate_only(text, sqlite3_value_bytes(argv[i]), db);
    if (rc != SQLITE_OK) {
      wa_density_error(ctx, rc);
      return;
    }
  }
  if (wa_interrupted(db)) {
    wa_density_error(ctx, SQLITE_INTERRUPT);
    return;
  }
  phrases = fts->xPhraseCount(cursor);
  if (phrases < 1 || phrases > WA_MAX_TERMS) {
    wa_density_error(ctx, SQLITE_ERROR);
    return;
  }
  rc = fts->xColumnSize(cursor, -1, &tokens);
  if (rc != SQLITE_OK || tokens < 0 || tokens > maximum) {
    wa_density_error(ctx, rc == SQLITE_OK ? SQLITE_ERROR : rc);
    return;
  }
  maximum_instances = (int64_t)tokens * phrases;
  /* The repository FTS tables retain default detail=full and columnsize=1.
   * In the pinned SQLite implementation, xPhraseFirst borrows the already
   * indexed position list; xPhraseNext reads at most three bounded varints.
   * Poll before each occurrence/advance instead of xInstCount's uninterruptible
   * materialized instance-array loop. Duplicate phrases intentionally count.
   */
  for (i = 0; i < phrases; i++) {
    Fts5PhraseIter iterator;
    int column, offset;
    if (wa_interrupted(db)) {
      wa_density_error(ctx, SQLITE_INTERRUPT);
      return;
    }
    rc = fts->xPhraseFirst(cursor, i, &iterator, &column, &offset);
    if (rc != SQLITE_OK) {
      wa_density_error(ctx, rc);
      return;
    }
    while (column >= 0) {
      if (wa_interrupted(db)) {
        wa_density_error(ctx, SQLITE_INTERRUPT);
        return;
      }
      if (offset < 0 || instances >= maximum_instances) {
        wa_density_error(ctx, SQLITE_ERROR);
        return;
      }
      instances++;
      fts->xPhraseNext(cursor, &iterator, &column, &offset);
    }
    if (column != -1) {
      wa_density_error(ctx, SQLITE_ERROR);
      return;
    }
  }
  if (wa_interrupted(db)) {
    wa_density_error(ctx, SQLITE_INTERRUPT);
    return;
  }
  sqlite3_result_double(ctx,
      (double)instances / (double)(tokens > 0 ? tokens : 1));
}

/* Register a DIRECTONLY scalar placeholder before the FTS overload. Pinned
 * SQLite's overload_function leaves an existing FuncDef intact, and virtual
 * table overloading copies its flags. The placeholder also rejects calls
 * without an actual FTS table context using the existing fixed error.
 */
static void wa_token_context_error(sqlite3_context *ctx, int argc,
                                    sqlite3_value **argv) {
  (void)argc;
  (void)argv;
  wa_density_error(ctx, SQLITE_ERROR);
}

static int wa_token_rank_register(sqlite3 *db) {
  sqlite3_stmt *statement = 0;
  fts5_api *fts = 0;
  int final_rc;
  int rc = sqlite3_prepare_v2(db, "SELECT fts5(?1)", -1, &statement, 0);
  if (rc == SQLITE_OK) {
    rc = sqlite3_bind_pointer(statement, 1, &fts, "fts5_api_ptr", 0);
  }
  if (rc == SQLITE_OK) {
    rc = sqlite3_step(statement);
    if (rc == SQLITE_ROW && fts != 0 && fts->iVersion >= 2) {
      rc = SQLITE_OK;
    } else if (rc == SQLITE_ROW || rc == SQLITE_DONE) {
      rc = SQLITE_ERROR;
    }
  }
  final_rc = sqlite3_finalize(statement);
  if (rc == SQLITE_OK && final_rc != SQLITE_OK) rc = final_rc;
  if (rc != SQLITE_OK) return rc;
  rc = sqlite3_create_function_v2(
      db, "wa_doc_token_rank_v1", -1,
      SQLITE_UTF8 | SQLITE_INNOCUOUS | SQLITE_DIRECTONLY,
      0, wa_token_context_error, 0, 0, 0);
  if (rc == SQLITE_OK) {
    rc = fts->xCreateFunction(fts, "wa_doc_token_rank_v1", 0, wa_token_rank, 0);
  }
  return rc;
}

static int wa_sqlitelex_init(sqlite3 *db, char **error,
                            const sqlite3_api_routines *api) {
  int rc;
  (void)api;
  rc = sqlite3_create_function_v2(
      db, "wa_doc_density_v1", -1,
      SQLITE_UTF8 | SQLITE_DETERMINISTIC | SQLITE_INNOCUOUS | SQLITE_DIRECTONLY,
      0, wa_density, 0, 0, 0);
  if (rc == SQLITE_OK && sqlite3_compileoption_used("ENABLE_FTS5")) {
    rc = wa_token_rank_register(db);
  }
  if (rc != SQLITE_OK && error != 0) {
    *error = sqlite3_mprintf("sqlitelex_registration_failed");
  }
  return rc;
}

int wa_sqlitelex_register(void) {
  return sqlite3_auto_extension((void (*)(void))wa_sqlitelex_init);
}
