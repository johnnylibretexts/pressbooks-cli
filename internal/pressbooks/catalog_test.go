package pressbooks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// booksServer serves `total` books ten to a page, the way a real network does,
// and counts the requests it receives.
func booksServer(t *testing.T, total int, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if perPage <= 0 || perPage > 10 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"code":"rest_invalid_param","message":"Invalid parameter(s): per_page"}`)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page <= 0 {
			page = 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-WP-Total", strconv.Itoa(total))
		w.Header().Set("X-WP-TotalPages", strconv.Itoa((total+perPage-1)/perPage))

		var items []string
		for i := (page - 1) * perPage; i < page*perPage && i < total; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"link":"https://books.test/book-%02d/","metadata":{
				"name":"Book %02d","alternativeHeadline":"Subtitle %02d","inLanguage":"en","copyrightYear":2020,
				"wordCount":%d,"image":"https://books.test/cover-%02d.jpg",
				"author":[{"@type":"Person","name":"Author %02d"}],
				"license":{"url":"https://creativecommons.org/licenses/by/4.0/","name":"CC BY (Attribution)"},
				"network":{"host":"books.test","name":"Books Test Network"}}}`,
				i+1, i+1, i+1, i+1, (i+1)*100, i+1, i+1))
		}
		_, _ = fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	}))
	t.Cleanup(server.Close)
	return server
}

// hostOf turns an httptest URL into the host:port the crawl is given.
func hostOf(t *testing.T, server *httptest.Server) string {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

func TestCatalogCrawlsEveryPage(t *testing.T) {
	redirectCache(t)
	var hits atomic.Int32
	server := booksServer(t, 24, &hits)

	client := New(0)
	client.baseScheme = "http"
	var lastCurrent, lastTotal int
	got, err := client.Catalog(context.Background(), hostOf(t, server), func(current, total int) {
		lastCurrent, lastTotal = current, total
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Books) != 24 || got.TotalBooks != 24 {
		t.Fatalf("crawled %d of %d books", len(got.Books), got.TotalBooks)
	}
	if !got.Complete() {
		t.Fatalf("a crawl that fetched every page should be complete: %#v", got)
	}
	if got.Books[0].Title != "Book 01" || got.Books[23].Title != "Book 24" {
		t.Fatalf("books arrived out of order: %q … %q", got.Books[0].Title, got.Books[23].Title)
	}
	if got.Books[0].Authors[0] != "Author 01" || got.Books[0].LicenseName != "CC BY (Attribution)" {
		t.Fatalf("book metadata lost: %#v", got.Books[0])
	}
	if got.Books[0].CopyrightYear != "2020" {
		t.Fatalf("numeric copyrightYear was dropped: %#v", got.Books[0])
	}
	if lastCurrent != lastTotal || lastTotal != 3 {
		t.Fatalf("progress ended at %d/%d, want 3/3", lastCurrent, lastTotal)
	}
}

func TestCatalogIsServedFromCacheOnTheSecondCall(t *testing.T) {
	redirectCache(t)
	var hits atomic.Int32
	server := booksServer(t, 12, &hits)
	client := New(0)
	client.baseScheme = "http"
	host := hostOf(t, server)

	if _, err := client.Catalog(context.Background(), host, nil); err != nil {
		t.Fatal(err)
	}
	first := hits.Load()
	if _, err := client.Catalog(context.Background(), host, nil); err != nil {
		t.Fatal(err)
	}
	// The second call costs exactly one request: the per_page=1 staleness
	// check whose X-WP-Total matches the cached count.
	if got := hits.Load() - first; got != 1 {
		t.Fatalf("second call made %d requests, want 1 staleness check", got)
	}
}

func TestCatalogRecrawlsWhenTheLiveCountChanged(t *testing.T) {
	redirectCache(t)
	var hits atomic.Int32
	server := booksServer(t, 12, &hits)
	client := New(0)
	client.baseScheme = "http"
	host := hostOf(t, server)

	if _, err := client.Catalog(context.Background(), host, nil); err != nil {
		t.Fatal(err)
	}
	stale, _ := readCatalogCache(host, DefaultCacheTTL)
	stale.TotalBooks = 5 // the network has published books since
	writeCatalogCache(stale)

	afterFirstCrawl := hits.Load()
	got, err := client.Catalog(context.Background(), host, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Books) != 12 {
		t.Fatalf("a changed live count should force a recrawl, got %d books", len(got.Books))
	}
	// One staleness probe, whose count is reused as the crawl's total, plus
	// the two pages a 12-book catalog needs. Fetching the live count a second
	// time once it's found to differ from the cache would double the cost of
	// every staleness-detected recrawl.
	if got, want := hits.Load()-afterFirstCrawl, int32(3); got != want {
		t.Fatalf("second call made %d requests, want %d (one probe reused, not two)", got, want)
	}
}

// TestCatalogFallsBackToCacheOnAPermanentProbeErrorWithOneRequest exercises
// the other fall-through from the same staleness-check block: a probe that
// fails permanently (host now blocked) must also cost exactly one logical
// probe, not one for the check and a second identical one right after.
//
// NOTE: "one logical probe" is forbiddenRetryLimit (2) requests, not 1, since
// this fix round gives a single 403 one retry before it is reported as
// ErrBlocked; this test only guards against a second, separate probe call,
// which forbiddenRetryLimit does not change.
func TestCatalogFallsBackToCacheOnAPermanentProbeErrorWithOneRequest(t *testing.T) {
	redirectCache(t)
	var hits atomic.Int32
	var blocked atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if blocked.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page <= 0 {
			page = 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-WP-Total", "12")
		var items []string
		for i := (page - 1) * perPage; i < page*perPage && i < 12; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"link":"https://books.test/book-%02d/","metadata":{"name":"Book %02d"}}`, i+1, i+1, i+1))
		}
		_, _ = fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	client.backoff = time.Millisecond // the probe now retries once; keep the test fast
	host := hostOf(t, server)
	if _, err := client.Catalog(context.Background(), host, nil); err != nil {
		t.Fatal(err)
	}

	blocked.Store(true)
	afterFirstCrawl := hits.Load()
	got, err := client.Catalog(context.Background(), host, nil)
	if err != nil {
		t.Fatalf("a blocked host with a complete cache should fall back to it, not error: %v", err)
	}
	if len(got.Books) != 12 {
		t.Fatalf("expected the cached catalog back, got %d books", len(got.Books))
	}
	if got := hits.Load() - afterFirstCrawl; got != int32(forbiddenRetryLimit) {
		t.Fatalf("a permanently-blocked staleness probe made %d requests, want exactly forbiddenRetryLimit (%d)", got, forbiddenRetryLimit)
	}
}

func TestSearchBooksMatchesTitleSubtitleAndAuthor(t *testing.T) {
	books := []Book{
		{Title: "Applied Ecology", Authors: []string{"Nick Haddad"}},
		{Title: "Skiing and Snowboarding", Subtitle: "A Field Guide to Ecology"},
		{Title: "The Delftia Book", Authors: []string{"Ecology Lab"}},
		{Title: "American Government"},
	}
	if got := SearchBooks(books, "ecology"); len(got) != 3 {
		t.Fatalf("got %d matches, want 3: %#v", len(got), got)
	}
	if got := SearchBooks(books, "GOVERNMENT"); len(got) != 1 || got[0].Title != "American Government" {
		t.Fatalf("search should be case-insensitive: %#v", got)
	}
	if got := SearchBooks(books, ""); len(got) != len(books) {
		t.Fatalf("an empty query should match everything, got %d", len(got))
	}
}

// WordPress stores post titles HTML-encoded, and the Pressbooks API returns
// them that way verbatim. This is common enough (roughly 6% of titles sampled
// across bundled networks) that it must be decoded once, where wire data
// becomes domain data, rather than left for every downstream reader to
// rediscover. The network's own display name comes from the same WordPress
// encoding and needs the same treatment; a URL or license identifier does
// not, and is deliberately left alone.
func TestWireBookDecodesHTMLEntitiesInTitleSubtitleAuthorsAndNetworkName(t *testing.T) {
	w := wireBook{
		Link: "https://books.test/polymer-science/",
		Metadata: wireMetadata{
			Name:                "Polymer Science &amp; Engineering: An Interactive Introduction",
			AlternativeHeadline: "Editor&#039;s Cut",
			Author:              []wirePerson{{Name: "O&#039;Brien &amp; Associates"}},
			Network:             wireNetwork{Host: "books.test", Name: "Books &amp; Journals Test Network"},
			License:             wireLicense{Name: "CC BY (Attribution)", URL: "https://creativecommons.org/licenses/by/4.0/"},
		},
	}
	book := w.book("books.test")
	if book.Title != "Polymer Science & Engineering: An Interactive Introduction" {
		t.Fatalf("title was not decoded: %q", book.Title)
	}
	if book.Subtitle != "Editor's Cut" {
		t.Fatalf("subtitle was not decoded: %q", book.Subtitle)
	}
	if len(book.Authors) != 1 || book.Authors[0] != "O'Brien & Associates" {
		t.Fatalf("author name was not decoded: %#v", book.Authors)
	}
	if book.NetworkName != "Books & Journals Test Network" {
		t.Fatalf("network name was not decoded: %q", book.NetworkName)
	}
	// License identifiers are not prose and must be left exactly as the API
	// sent them.
	if book.LicenseName != "CC BY (Attribution)" {
		t.Fatalf("license name should not be touched: %q", book.LicenseName)
	}
}

// This is the test that would have caught the original defect: a query typed
// the way a person actually types it must still match a title the upstream
// API sent HTML-encoded. Without decoding at the wireBook boundary, the
// stored title keeps its "&amp;" and a plain "&" in the query can never match
// it, silently hiding a genuinely present book from a search that comes back
// looking merely empty.
func TestSearchMatchesAnUpstreamEncodedTitleWithAPlainAmpersandQuery(t *testing.T) {
	w := wireBook{
		Link:     "https://books.test/polymer-science/",
		Metadata: wireMetadata{Name: "Polymer Science &amp; Engineering: An Interactive Introduction"},
	}
	books := []Book{w.book("books.test")}
	got := SearchBooks(books, "polymer science & engineering")
	if len(got) != 1 {
		t.Fatalf("a plain-ampersand query should match an upstream-encoded title: %#v", got)
	}
}

// TestClientCacheTTLZeroBypassesTheCacheEndToEnd verifies the consumer side of
// SetCacheTTL: a zero TTL must actually stop Catalog from short-circuiting on
// a cached, complete, still-accurate listing. Without this, --no-cache would
// look wired up (SetCacheTTL exists, cacheTTL is stored) while doing nothing.
func TestClientCacheTTLZeroBypassesTheCacheEndToEnd(t *testing.T) {
	redirectCache(t)
	var hits atomic.Int32
	server := booksServer(t, 12, &hits)
	client := New(0)
	client.baseScheme = "http"
	host := hostOf(t, server)

	if _, err := client.Catalog(context.Background(), host, nil); err != nil {
		t.Fatal(err)
	}
	afterFirstCrawl := hits.Load()

	client.SetCacheTTL(0)
	if _, err := client.Catalog(context.Background(), host, nil); err != nil {
		t.Fatal(err)
	}
	// A warm cache would cost exactly one staleness-check request. A zero TTL
	// must force a full recrawl instead: one LiveBookCount probe plus every
	// page (2, for a 12-book catalog).
	if got, want := hits.Load()-afterFirstCrawl, int32(3); got != want {
		t.Fatalf("SetCacheTTL(0) made %d requests on the second call, want %d (a full recrawl, not a cache hit)", got, want)
	}
}

// TestCatalogKeepsTheLongestCompletePrefixOnAMidCrawlFailure exercises the
// "longest complete prefix" rule directly: a crawl that fails partway through
// must keep every page that arrived before the failure and report itself
// incomplete, rather than either discarding the good pages or silently
// presenting a partial network as whole. It also pins the fix for the
// Critical finding this project's own review caught: an ordinary mid-crawl
// page failure (a 500, here) must return a non-nil error (ErrPartialCatalog)
// alongside the partial catalog — before this fix, ctx.Err() was returned
// instead, which is nil for an ordinary page failure, so a caller saw a nil
// error and a Books slice that looked like a small, complete network rather
// than a truncated one.
func TestCatalogKeepsTheLongestCompletePrefixOnAMidCrawlFailure(t *testing.T) {
	redirectCache(t)
	const total = 300  // 30 pages of 10
	const failPage = 5 // 1-based; fails on every attempt
	var hits atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if perPage <= 0 || perPage > 10 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page <= 0 {
			page = 1
		}
		if perPage == 10 && page == failPage {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-WP-Total", strconv.Itoa(total))
		var items []string
		for i := (page - 1) * perPage; i < page*perPage && i < total; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"link":"https://books.test/book-%02d/","metadata":{"name":"Book %02d"}}`, i+1, i+1, i+1))
		}
		_, _ = fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	client.backoff = time.Millisecond // the failing page retries; keep the test fast

	got, err := client.Catalog(context.Background(), hostOf(t, server), nil)
	if err == nil {
		t.Fatal("a mid-crawl failure must return a non-nil error alongside the partial catalog, so a caller cannot mistake it for a small, complete network")
	}
	if !errors.Is(err, ErrPartialCatalog) {
		t.Fatalf("error = %v, want it to wrap ErrPartialCatalog", err)
	}
	if got.SyncedPages != failPage-1 {
		t.Fatalf("synced pages = %d, want %d", got.SyncedPages, failPage-1)
	}
	if len(got.Books) != (failPage-1)*10 {
		t.Fatalf("got %d books, want %d (pages 1-%d only)", len(got.Books), (failPage-1)*10, failPage-1)
	}
	if got.Complete() {
		t.Fatal("a catalog missing pages must report itself incomplete, not pass off a partial network as whole")
	}
}

// TestPartialCatalogStillCarriesTheNetworkName pins the Minor fix alongside
// the Critical one: out.Name was previously only set on the fully-synced
// path, so a partial catalog — one with books already synced, cached and
// served back on a later hit — carried an empty Name even though it already
// had books whose own NetworkName field said what the network was called.
func TestPartialCatalogStillCarriesTheNetworkName(t *testing.T) {
	redirectCache(t)
	const total = 20   // 2 pages of 10
	const failPage = 2 // fails on every attempt, after page 1 already landed

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page <= 0 {
			page = 1
		}
		if perPage == 10 && page == failPage {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-WP-Total", strconv.Itoa(total))
		var items []string
		for i := (page - 1) * perPage; i < page*perPage && i < total; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"link":"https://books.test/book-%02d/","metadata":{"name":"Book %02d","network":{"host":"books.test","name":"Test Network"}}}`, i+1, i+1, i+1))
		}
		_, _ = fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	client.backoff = time.Millisecond

	got, err := client.Catalog(context.Background(), hostOf(t, server), nil)
	if !errors.Is(err, ErrPartialCatalog) {
		t.Fatalf("error = %v, want ErrPartialCatalog", err)
	}
	if len(got.Books) == 0 {
		t.Fatal("test setup: expected at least page 1's books to have synced")
	}
	if got.Name != "Test Network" {
		t.Fatalf("partial catalog Name = %q, want %q", got.Name, "Test Network")
	}
}

// TestCatalogStopsDispatchingAfterAnEarlyFailure pins the low-water-mark
// mechanism's effect on a live crawl. It does not assume which page number
// happens to win a concurrency-limit slot first — an earlier version of this
// test hardcoded "page 1 fails" and was empirically flaky (up to 15% of 100
// runs, with counts as high as all 30 pages), because Go's scheduler does not
// reliably run goroutines in creation order under contention, so a
// hardcoded-index approach cannot assume the failing page lands in an early
// wave.
//
// Instead, the server holds the first crawlConcurrency page requests to
// arrive — a hard guarantee, not a scheduling hope: the concurrency
// semaphore has exactly that many slots, so no 5th page can possibly be in
// flight while these are held open. Once that many have arrived, the test
// fails whichever of them has the lowest page number and lets the rest
// succeed. That page's failure sets the low-water mark to its own index,
// after which no page whose own request hasn't already started can pass the
// mark check — bounding how many *additional* pages (beyond that first,
// structurally-guaranteed batch) can still be legitimately attempted before
// the mark takes effect, without needing to guess which specific pages those
// are.
func TestCatalogStopsDispatchingAfterAnEarlyFailure(t *testing.T) {
	redirectCache(t)
	const total = 200 // 20 pages of 10, far more than crawlConcurrency
	var hits atomic.Int32

	type arrival struct {
		page    int
		verdict chan bool // test sends true to fail this one, false to let it succeed
	}
	// Buffered to the full page count so the handler's send never blocks
	// waiting on the test, regardless of how many pages end up arriving here.
	arrivals := make(chan arrival, total)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-WP-Total", strconv.Itoa(total))
		if perPage == 1 {
			_, _ = fmt.Fprint(w, `[]`) // the LiveBookCount probe that precedes the crawl
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page <= 0 {
			page = 1
		}
		v := make(chan bool, 1)
		arrivals <- arrival{page: page, verdict: v}
		if <-v {
			// 400 rather than 403: a 403 now gets one retry before being
			// reported (this fix round's change), which would send a second
			// request for this same page and confuse the single-arrival-per-
			// page bookkeeping below. 400 is never retried at all, so this
			// stays exactly one request per failing page, same as before.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var items []string
		for i := (page - 1) * perPage; i < page*perPage && i < total; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"link":"https://books.test/book-%02d/","metadata":{"name":"Book %02d"}}`, i+1, i+1, i+1))
		}
		_, _ = fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"

	type result struct {
		catalog Catalog
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		got, err := client.Catalog(context.Background(), hostOf(t, server), nil)
		resultCh <- result{got, err}
	}()

	// Collect exactly crawlConcurrency arrivals before deciding anything.
	// This is a structural guarantee, not a timing assumption: the semaphore
	// caps concurrent dispatch at crawlConcurrency, so every one of these
	// necessarily got here before any failure existed for anything to skip.
	wave := make([]arrival, crawlConcurrency)
	for i := range wave {
		wave[i] = <-arrivals
	}
	lowest := 0
	for i := range wave {
		if wave[i].page < wave[lowest].page {
			lowest = i
		}
	}
	for i := range wave {
		wave[i].verdict <- i == lowest
	}

	// Keep letting every further arrival succeed until the crawl finishes.
	// Any page skipped by the low-water mark never shows up here at all: the
	// check happens client-side, before a request is ever sent.
loop:
	for {
		select {
		case res := <-resultCh:
			// Which page ends up failing is not this test's to control (it's
			// whichever of the confirmed first wave has the lowest number),
			// so the failure may land at index 0 (a hard error, no prior
			// cache) or later (a nil-error partial per the longest-complete-
			// prefix rule tested elsewhere). Either way, a run with a failing
			// page must never report a *complete* catalog.
			if res.err == nil && res.catalog.Complete() {
				t.Fatal("a crawl with a failing page should not report a complete catalog")
			}
			break loop
		case a := <-arrivals:
			a.verdict <- false
		case <-time.After(5 * time.Second):
			t.Fatal("crawl did not finish")
		}
	}

	// The failing page becomes the low-water mark, so only pages at or below
	// its own index can ever be requested afterward — necessarily a slice of
	// the 20-page network, never all of it. The threshold is deliberately
	// loose: it does not pin an exact count (which page ends up failing, and
	// how many others share its early wave, is not something this test
	// controls), only that a crawl whose first page fails does not go on to
	// request every remaining page — an unbounded crawl would cost 1 probe +
	// 20 pages = 21 requests here.
	const pages = total / booksPerPage
	if want := int32(1 + 3*pages/4); hits.Load() > want {
		t.Fatalf("server saw %d requests, want no more than %d (of a possible %d) — a crawl whose "+
			"first page fails should not go on to request every remaining page", hits.Load(), want, 1+pages)
	}
}

// TestCatalogRejectsANegativeLiveBookCount guards against a malformed
// X-WP-Total turning into a nonsensical catalog instead of a loud failure.
func TestCatalogRejectsANegativeLiveBookCount(t *testing.T) {
	redirectCache(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-WP-Total", "-5")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	if _, err := client.Catalog(context.Background(), hostOf(t, server), nil); err == nil {
		t.Fatal("a negative X-WP-Total should be rejected as a malformed response, not produce an empty catalog")
	}
}

// TestCatalogCapsPagesAtTheCeilingAndReportsIncomplete guards against an
// unbounded X-WP-Total sizing the crawl's allocations and request count
// directly: a host claiming far more pages than any real network has must be
// crawled only up to maxCrawlPages, and the result must say so via
// Complete(), not pass off the truncated fetch as the whole network.
func TestCatalogCapsPagesAtTheCeilingAndReportsIncomplete(t *testing.T) {
	redirectCache(t)
	const networkTotal = (maxCrawlPages + 5) * booksPerPage // more pages than the cap allows
	var hits atomic.Int32
	server := booksServer(t, networkTotal, &hits)

	client := New(0)
	client.baseScheme = "http"
	got, err := client.Catalog(context.Background(), hostOf(t, server), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncedPages != maxCrawlPages {
		t.Fatalf("synced %d pages, want the cap of %d", got.SyncedPages, maxCrawlPages)
	}
	if len(got.Books) != maxCrawlPages*booksPerPage {
		t.Fatalf("got %d books, want %d (the cap, not the network's claimed size)", len(got.Books), maxCrawlPages*booksPerPage)
	}
	if got.Complete() {
		t.Fatal("a catalog truncated at the page cap must report itself incomplete")
	}
}

// TestCatalogContextCancellationStopsTheCrawl confirms cancellation is
// observed by pages still queued behind the concurrency limit, not just by
// the page in flight: once ctx is cancelled, no further request should ever
// reach the server, and the returned error must reflect the caller's own
// cancellation rather than being silently swallowed.
//
// The server never sends a response to any page request until the test has
// already confirmed the crawl returned, so there is no race between "the
// response arrives" and "the cancellation is noticed" for this test to
// depend on: the response literally cannot exist yet.
func TestCatalogContextCancellationStopsTheCrawl(t *testing.T) {
	redirectCache(t)
	const total = 100 // 10 pages of 10, well beyond crawlConcurrency
	var hits atomic.Int32
	started := make(chan struct{}, crawlConcurrency)
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-WP-Total", strconv.Itoa(total))
		if perPage == 1 {
			// the LiveBookCount probe that precedes the crawl
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		started <- struct{}{}
		<-release // never closed until the crawl has already returned
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan error, 1)
	go func() {
		_, err := client.Catalog(ctx, hostOf(t, server), nil)
		resultCh <- err
	}()

	// Wait until every worker slot is occupied by a held request, then cancel.
	// The server will never answer any of them (release stays closed only
	// after the assertion below), so the crawl can only ever resolve via the
	// cancellation itself — there is no window where a response could win a
	// race against it.
	for i := 0; i < crawlConcurrency; i++ {
		<-started
	}
	cancel()

	var err error
	select {
	case err = <-resultCh:
		if err == nil {
			t.Fatal("a cancelled crawl should return an error, not a completed catalog")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled (the caller's own cancellation), got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("crawl did not return after context cancellation")
	}
	// Only now let the permanently-held handlers finish, so the deferred
	// server.Close() below doesn't hang waiting for them.
	close(release)

	// 1 LiveBookCount probe + exactly crawlConcurrency page requests: every
	// page still queued behind the semaphore must fail fast on the cancelled
	// context instead of ever dialing the server.
	if got, want := hits.Load(), int32(1+crawlConcurrency); got != want {
		t.Fatalf("server saw %d requests, want %d (cancellation should stop further pages)", got, want)
	}
}

// TestASecondRunOverAPartialCacheStillReportsItPartial covers the two cache
// read paths that the first ErrPartialCatalog fix left open. Run one crawls,
// fails mid-way, and caches the partial catalog. Run two finds that entry in
// the cache and cannot reach the host to check it. Before cachedOrPartial,
// both fall-through paths returned the cached partial with a nil error —
// reproducing the very signature the Critical named (err=<nil>, books=N,
// complete=false), one run later, in the exact scenario the fix was written
// for: a host that is still rate-limiting on the next invocation.
//
// failFrom is what makes the two paths distinct. With it at page 1 the second
// run fails its live-count probe and returns from the LiveBookCount branch;
// with it at page 2 the probe succeeds (the count header is served on the
// error response too) and the run reaches the page-0 failure branch inside
// the crawl loop.
func TestASecondRunOverAPartialCacheStillReportsItPartial(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failFrom int // 1-based page; run two fails from here on
	}{
		{"live count probe fails", 1},
		{"first crawl page fails", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectCache(t)
			const total = 30 // 3 pages of 10
			var secondRun atomic.Bool

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page <= 0 {
					page = 1
				}
				// Run one: fail page 2 of the real crawl so pages 1 survives.
				// Run two: fail from failFrom onwards, probe included.
				fail := perPage == 10 && page == 2
				if secondRun.Load() {
					fail = page >= tc.failFrom
				}
				if fail {
					// Serve the count header even on the error so the
					// live-count probe can still succeed when failFrom > 1.
					w.Header().Set("X-WP-Total", strconv.Itoa(total))
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-WP-Total", strconv.Itoa(total))
				var items []string
				for i := (page - 1) * perPage; i < page*perPage && i < total; i++ {
					items = append(items, fmt.Sprintf(`{"id":%d,"link":"https://books.test/book-%02d/","metadata":{"name":"Book %02d"}}`, i+1, i+1, i+1))
				}
				_, _ = fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
			}))
			defer server.Close()

			client := New(0)
			client.baseScheme = "http"
			client.backoff = time.Millisecond
			host := hostOf(t, server)

			first, err := client.Catalog(context.Background(), host, nil)
			if !errors.Is(err, ErrPartialCatalog) {
				t.Fatalf("run one error = %v, want ErrPartialCatalog", err)
			}
			if first.Complete() || len(first.Books) == 0 {
				t.Fatalf("run one should have cached a non-empty partial catalog, got %d books complete=%v", len(first.Books), first.Complete())
			}

			secondRun.Store(true)
			got, err := client.Catalog(context.Background(), host, nil)
			if got.Complete() {
				t.Fatal("the cached entry under test must be the incomplete one")
			}
			if err == nil {
				t.Fatalf("a partial catalog served from cache returned a nil error alongside %d of %d books — indistinguishable from a small, complete network", len(got.Books), got.TotalBooks)
			}
			if !errors.Is(err, ErrPartialCatalog) {
				t.Fatalf("error = %v, want it to wrap ErrPartialCatalog", err)
			}
			if len(got.Books) != len(first.Books) {
				t.Fatalf("got %d books from cache, want the %d run one cached", len(got.Books), len(first.Books))
			}
		})
	}
}
