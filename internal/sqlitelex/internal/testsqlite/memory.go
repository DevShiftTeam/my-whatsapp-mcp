//go:build cgo

// Package testsqlite exposes only SQLite's aggregate allocation counter for
// sqlitelex tests. Production registration/scoring does not import it. It
// links the same bundled SQLite and never obtains a database or body handle.
package testsqlite

/*
#cgo CFLAGS: -DSQLITE_CORE
#include "../../../sqlitevec/sqlite3.h"
*/
import "C"

func MemoryUsed() int64 { return int64(C.sqlite3_memory_used()) }
