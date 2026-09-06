package pressbooks

// perHostConcurrency is this package's single answer to "how many requests
// may this client keep in flight against one Pressbooks host at once,"
// matching design section 8's stated ceiling of four. The catalog crawl
// (crawlConcurrency, catalog.go) and ExportFormats (exports.go) both bound
// their own fan-out through this one constant rather than each choosing a
// fresh number at its own call site — which is exactly how ExportFormats
// came to fire eight concurrent HEADs in the first place, exceeding the
// ceiling this project already apologised for exceeding once, at the
// network-crawl level, after the rate-limiting incident that shaped
// crawlConcurrency, backendPace and crawlPace.
//
// This is the domain package's side of that answer. The cli package owns
// the separate, coarser question of how many *different backends* a sweep
// may crawl at once (see internal/cli/backend.go): that is a fan-out
// concern across whole crawls, not a per-request concern within one host,
// so it lives one layer up rather than here.
const perHostConcurrency = 4
