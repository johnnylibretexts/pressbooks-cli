package pressbooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultCacheTTL bounds how long a cached catalog is reused. A network gains
// or loses a book rarely, and a full crawl of the largest one is 308 requests,
// so a week-old listing is worth far more than it costs.
const DefaultCacheTTL = 7 * 24 * time.Hour

// userCacheDir is a variable so tests can redirect the cache to a temp dir.
var userCacheDir = os.UserCacheDir

func catalogCachePath(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	// The host becomes a filename, so anything that could climb out of the
	// cache directory is refused rather than sanitised into something else.
	if host == "" || strings.ContainsAny(host, `/\`) || strings.Contains(host, "..") {
		return "", fmt.Errorf("unusable cache key %q", host)
	}
	dir, err := userCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pressbooks-pp-cli", "catalogs", host+".json"), nil
}

// readCatalogCache returns a cached catalog younger than ttl. Every failure is
// a cache miss: caching must never break a command.
func readCatalogCache(host string, ttl time.Duration) (Catalog, bool) {
	if ttl <= 0 {
		return Catalog{}, false
	}
	path, err := catalogCachePath(host)
	if err != nil {
		return Catalog{}, false
	}
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > ttl {
		return Catalog{}, false
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 {
		return Catalog{}, false
	}
	var out Catalog
	if err := json.Unmarshal(body, &out); err != nil {
		return Catalog{}, false
	}
	return out, true
}

// writeCatalogCache stores a catalog via a temp file and a rename, so a
// concurrent reader never sees a half-written listing. Errors are ignored: a
// machine with no writable cache directory should still work.
func writeCatalogCache(catalog Catalog) {
	path, err := catalogCachePath(catalog.Host)
	if err != nil {
		return
	}
	body, err := json.Marshal(catalog)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}
