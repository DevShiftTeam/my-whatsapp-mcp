package modelstore

import _ "embed"

// builtinCatalogJSON is the offline, trusted catalog. Installation never fetches a catalog.
//
//go:embed default_catalog.json
var builtinCatalogJSON string

// DefaultCatalog returns the pinned feasibility candidates without selecting them for release.
func DefaultCatalog() (Catalog, error) {
	return ParseCatalog([]byte(builtinCatalogJSON))
}
