package pressbooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestMain defaults crawlPace to a negligible value for this package's whole
// test binary: every test here drives an in-process httptest server, not
// the real university server crawlPace exists to protect, and should not pay
// real wall-clock time for a delay that only matters against one. Without
// this, every existing crawl test that fetches more than one page pays
// crawlPace per page — the full package suite went from ~2s to 53s with
// -race when crawlPace was simply added at its production value with no
// override. TestCatalogPacesPageDispatchWithinACrawl below, which actually
// measures the mechanism, overrides this default locally to a small, real
// value and restores it via t.Cleanup.
func TestMain(m *testing.M) {
	crawlPace = time.Microsecond
	os.Exit(m.Run())
}

// withCrawlPace temporarily overrides crawlPace for a test's duration and
// restores TestMain's negligible default afterward — the same pattern
// Client.backoff and the cli package's backendPace already use.
func withCrawlPace(t *testing.T, pace time.Duration) {
	t.Helper()
	original := crawlPace
	crawlPace = pace
	t.Cleanup(func() { crawlPace = original })
}

// testCrawlPace is what the test below uses in place of TestMain's
// negligible default: it is measuring the pacing mechanism itself, so
// unlike every other crawl test in this package it must keep a real,
// nonzero pace — just a much smaller one than production needs, so the
// assertion stays fast.
const testCrawlPace = 20 * time.Millisecond

// This is the test that would have caught the missing half of the sweep
// fix: crawlConcurrency alone bounds how many pages are in flight at once,
// but not how quickly new ones start, so without crawlPace every freed
// semaphore slot's request fires the instant a response arrives. This
// proves successive page dispatches within one crawl are spaced by at
// least crawlPace.
func TestCatalogPacesPageDispatchWithinACrawl(t *testing.T) {
	redirectCache(t)
	withCrawlPace(t, testCrawlPace)

	const total = 60 // 6 pages of 10, more than crawlConcurrency
	var mu sync.Mutex
	var dispatches []time.Time

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-WP-Total", strconv.Itoa(total))
		if perPage == 1 {
			_, _ = w.Write([]byte(`[]`)) // the LiveBookCount probe that precedes the crawl
			return
		}
		mu.Lock()
		dispatches = append(dispatches, time.Now())
		mu.Unlock()
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	if _, err := client.Catalog(context.Background(), hostOf(t, server), nil); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	const wantPages = total / booksPerPage
	if len(dispatches) != wantPages {
		t.Fatalf("got %d page dispatches, want %d", len(dispatches), wantPages)
	}
	sort.Slice(dispatches, func(i, j int) bool { return dispatches[i].Before(dispatches[j]) })
	const tolerance = testCrawlPace / 2
	for i := 1; i < len(dispatches); i++ {
		gap := dispatches[i].Sub(dispatches[i-1])
		if gap < testCrawlPace-tolerance {
			t.Fatalf("gap between successive page dispatches = %v, want at least ~%v (testCrawlPace)", gap, testCrawlPace)
		}
	}
}
