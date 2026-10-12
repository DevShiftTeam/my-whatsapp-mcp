# SQLite keyword scoring

This package registers the native SQLite functions used by the archive's keyword ranking. `internal/archive` calls `Register()` before opening its databases.

Registration uses `sqlite3_auto_extension` with the SQLite instance from mattn/go-sqlite3. It supplies no separate SQLite library or per-row Go callback. Builds without cgo refuse registration.

The scorer accepts bounded UTF-8 input and returns fixed errors for malformed operands. It uses stack storage and polls SQLite's interruption flag while scanning. See `density.c` for the input checks and scoring formulas, and `register.go` for registration.

Archive queries must check account, archive policy, client scope, source validity, expiry, and applicable media access inside a lazy SQL CASE before invoking native validation. WHERE clause order does not provide that guarantee. Preserve each message or extraction role's tighter input limit before scoring.

The package tests compare scores with SQLite's length/replace formula and cover ordering, pagination, Hebrew and Unicode input, malformed data, limits, registration across connections, cancellation, and memory reuse. The archive's query tests cover integration and denied sources. Benchmark the target build and dataset before publishing performance results.
