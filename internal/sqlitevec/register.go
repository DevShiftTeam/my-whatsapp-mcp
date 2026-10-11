// Package sqlitevec registers the vendored stable sqlite-vec against the
// SQLite symbols provided by mattn/go-sqlite3. It compiles no second SQLite.
package sqlitevec

/*
#cgo CFLAGS: -DSQLITE_CORE -DSQLITE_VEC_STATIC
#cgo linux LDFLAGS: -lm
#include "sqlite3.h"
#include "sqlite-vec.h"
static int wa_register_vec(void) {
 return sqlite3_auto_extension((void (*)(void))sqlite3_vec_init);
}
*/
import "C"
import (
	"errors"
	"sync"
)

var once sync.Once
var registerError error

func Register() error {
	once.Do(func() {
		if C.wa_register_vec() != 0 {
			registerError = errors.New("sqlitevec_registration_failed")
		}
	})
	return registerError
}
