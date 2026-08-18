package dacost

import (
	"bytes"
	_ "embed"
	"sync"
)

//go:embed bundled_prices.json
var bundledPricesJSON []byte

var bundledCatalog = sync.OnceValue(func() *Catalog {
	catalog, err := DecodeCatalog(bytes.NewReader(bundledPricesJSON), CatalogOptions{})
	if err != nil {
		panic("dacost: invalid embedded pricing catalog: " + err.Error())
	}
	return catalog
})

// BundledCatalog returns the immutable maintainer-curated stopgap catalog.
// Authoritative or local catalogs should still be supplied separately to
// NewPricer so its documented precedence remains explicit.
func BundledCatalog() *Catalog { return bundledCatalog() }
