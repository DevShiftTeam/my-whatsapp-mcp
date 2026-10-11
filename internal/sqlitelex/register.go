//go:build cgo

// Package sqlitelex registers the exact document-local density scalar against
// the bundled SQLite symbols provided by mattn/go-sqlite3, plus the versioned
// document-local FTS5 token auxiliary when FTS5 is compiled in. It compiles no
// second SQLite and exposes no database handle or per-row Go callback.
package sqlitelex

/*
#cgo CFLAGS: -DSQLITE_CORE -fno-fast-math
#include "density.h"
*/
import "C"

import (
	"errors"
	"sync"
)

var once sync.Once
var registerError error

// Register must succeed before opening any database that uses the scorer.
// The static auto-extension installs wa_doc_density_v1 on every subsequent
// connection, plus wa_doc_token_rank_v1 on FTS5-enabled connections, including
// reopened and query-only readers. Connection-local
// installation failures cause SQLite open to fail. Callers must put every
// invocation behind a lazy complete-authority CASE; input validation itself
// is not an authorization boundary.
func Register() error {
	once.Do(func() {
		if C.wa_sqlitelex_register() != 0 {
			registerError = errors.New("sqlitelex_registration_failed")
		}
	})
	return registerError
}
