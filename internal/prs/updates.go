package prs

// CatalogUpdate/DetailUpdate describe a cached or refreshed snapshot. Cached
// indicates unverified old data; fresh cache hits within the TTL are not stale.
type CatalogUpdate struct {
	Catalog Catalog
	Cached  bool
}

type DetailUpdate struct {
	Detail Detail
	Cached bool
	// FromCache distinguishes a TTL-valid disk hit from a live response. Its
	// prose is usable, but must not replace more recently observed PR status.
	FromCache bool
}
