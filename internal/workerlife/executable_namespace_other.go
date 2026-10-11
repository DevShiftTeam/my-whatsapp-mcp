//go:build !darwin && !linux

package workerlife

import "os"

func namespaceNoExtendedACL(*os.File) bool                          { return false }
func namespaceNewAnchor(*os.File, string) (*namespaceAnchor, error) { return nil, ErrUnavailable }
func namespaceVerifyAnchor(*namespaceAnchor) error                  { return ErrUnavailable }
func namespaceDuplicate(*os.File) (*os.File, error)                 { return nil, ErrUnavailable }
func namespaceStat(*os.File) (namespaceIdentity, error)             { return namespaceIdentity{}, ErrUnavailable }
func namespaceCreateDirectory(*os.File, string) (*os.File, bool, error) {
	return nil, false, ErrUnavailable
}
func namespaceOpenDirectory(*os.File, string) (*os.File, error)        { return nil, ErrUnavailable }
func namespaceOpenFile(*os.File, string, bool) (*os.File, error)       { return nil, ErrUnavailable }
func namespaceSameDirectory(*os.File, string, namespaceIdentity) error { return ErrUnavailable }
func namespaceSameFile(*os.File, string, namespaceIdentity) error      { return ErrUnavailable }
func namespaceRenameDirectory(*os.File, string, string) error          { return ErrUnavailable }
func namespaceUnlinkFile(*os.File, string) error                       { return ErrUnavailable }
func namespaceRemoveDirectory(*os.File, string) error                  { return ErrUnavailable }
func namespaceNames(*os.File, int) ([]string, error)                   { return nil, ErrUnavailable }
func namespaceClone(*os.File, *os.File, string) error                  { return ErrUnavailable }
