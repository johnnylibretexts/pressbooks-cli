package pressbooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// redirectCache points the catalog cache at a temp directory for one test. No
// test may write to the real user cache directory.
func redirectCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	original := userCacheDir
	userCacheDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { userCacheDir = original })
	return dir
}

func TestCatalogCacheRoundTrips(t *testing.T) {
	redirectCache(t)
	want := Catalog{
		Host: "ncstate.pressbooks.pub", Name: "NC State", TotalBooks: 2,
		SyncedPages: 1, TotalPages: 1, SyncedAt: time.Now().UTC(),
		Books: []Book{{URL: "https://ncstate.pressbooks.pub/delftia/", Title: "The Delftia Book"}},
	}
	writeCatalogCache(want)

	got, ok := readCatalogCache("ncstate.pressbooks.pub", DefaultCacheTTL)
	if !ok {
		t.Fatal("a catalog just written should read back")
	}
	if len(got.Books) != 1 || got.Books[0].Title != "The Delftia Book" || !got.Complete() {
		t.Fatalf("round trip lost data: %#v", got)
	}
}

func TestCatalogCacheExpires(t *testing.T) {
	dir := redirectCache(t)
	writeCatalogCache(Catalog{Host: "example.test", TotalPages: 1, SyncedPages: 1, SyncedAt: time.Now().UTC()})

	stale := time.Now().Add(-8 * 24 * time.Hour)
	path := filepath.Join(dir, "pressbooks-pp-cli", "catalogs", "example.test.json")
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCatalogCache("example.test", DefaultCacheTTL); ok {
		t.Fatal("a catalog older than the TTL should read as a miss")
	}
}

func TestZeroTTLDisablesTheCache(t *testing.T) {
	redirectCache(t)
	writeCatalogCache(Catalog{Host: "example.test", TotalPages: 1, SyncedPages: 1, SyncedAt: time.Now().UTC()})
	if _, ok := readCatalogCache("example.test", 0); ok {
		t.Fatal("a zero TTL should disable cache reads, which is what --no-cache needs")
	}
}

// Caching must never break a command: an unwritable cache directory costs a
// refetch, not an error.
func TestCacheFailuresAreMisses(t *testing.T) {
	original := userCacheDir
	userCacheDir = func() (string, error) { return "", os.ErrPermission }
	t.Cleanup(func() { userCacheDir = original })

	writeCatalogCache(Catalog{Host: "example.test"})
	if _, ok := readCatalogCache("example.test", DefaultCacheTTL); ok {
		t.Fatal("an unreachable cache directory should read as a miss")
	}
}

// One host's catalog must never satisfy another's, or a search would report a
// network's books under a different network's name.
func TestCatalogsAreKeyedByHost(t *testing.T) {
	redirectCache(t)
	writeCatalogCache(Catalog{Host: "a.example", TotalPages: 1, SyncedPages: 1, SyncedAt: time.Now().UTC(),
		Books: []Book{{Title: "From A"}}})
	if _, ok := readCatalogCache("b.example", DefaultCacheTTL); ok {
		t.Fatal("a catalog cached for one host was returned for another")
	}
}

// A host is a filename here. A traversal in it must not escape the cache dir.
func TestCachePathRejectsHostileHosts(t *testing.T) {
	redirectCache(t)
	if _, err := catalogCachePath("../../etc/passwd"); err == nil {
		t.Fatal("a host containing a path traversal should be rejected")
	}
}
