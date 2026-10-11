//go:build !cgo

package sqlitelex

import "errors"

// Register fails closed when the native scalar cannot be linked.
func Register() error { return errors.New("sqlitelex_registration_failed") }
