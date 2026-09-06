package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
)

// withBackendPace temporarily overrides the package-level backendPace for
// the duration of a test and restores it on cleanup, the same way
// Client.backoff in the pressbooks package lets a test shrink the HTTP
// retry wait. A test that is not itself measuring the pace or the
// serialization it enforces should not pay real wall-clock time for a delay
// that exists purely to be polite to a remote server the fake never talks
// to.
func withBackendPace(t *testing.T, pace time.Duration) {
	t.Helper()
	original := backendPace
	backendPace = pace
	t.Cleanup(func() { backendPace = original })
}

func catalogOf(host string, titles ...string) pressbooks.Catalog {
	catalog := pressbooks.Catalog{Host: host, Name: host, TotalPages: 1, SyncedPages: 1}
	for _, title := range titles {
		catalog.Books = append(catalog.Books, pressbooks.Book{
			URL:         "https://" + host + "/" + strings.ToLower(strings.ReplaceAll(title, " ", "-")) + "/",
			Host:        host,
			Title:       title,
			LicenseName: "CC BY (Attribution)",
		})
	}
	catalog.TotalBooks = len(catalog.Books)
	return catalog
}

func TestSearchSweepsEveryBundledNetwork(t *testing.T) {
	// With only sweepConcurrency hosts or fewer, every host fits in the first
	// wave and the semaphore is never actually exercised.
	if len(pressbooks.Networks()) <= sweepConcurrency {
		t.Fatalf("test setup: %d bundled networks does not exceed sweepConcurrency (%d)", len(pressbooks.Networks()), sweepConcurrency)
	}
	// This test is about cross-network matching, not pacing: it sweeps all 28
	// bundled networks, 15 of which share the pressbooks.pub backend, and has
	// no interest in the real inter-crawl delay.
	withBackendPace(t, time.Millisecond)
	fake := &fakeClient{catalogs: map[string]pressbooks.Catalog{}}
	for _, n := range pressbooks.Networks() {
		fake.catalogs[n.Host] = catalogOf(n.Host, "Unrelated Book")
	}
	fake.catalogs["ncstate.pressbooks.pub"] = catalogOf("ncstate.pressbooks.pub", "Applied Ecology")

	stdout, _, err := executeWithFake(t, fake, "search", "ecology", "--agent")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data []agentBookSummary `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 1 || got.Data[0].Title != "Applied Ecology" {
		t.Fatalf("cross-network search failed: %#v", got.Data)
	}
}

// An agent capturing stdout must read exactly one JSON line, however long the
// crawl took, so progress belongs on stderr.
func TestCrawlProgressGoesToStderr(t *testing.T) {
	fake := &fakeClient{catalogs: map[string]pressbooks.Catalog{
		"ncstate.pressbooks.pub": catalogOf("ncstate.pressbooks.pub", "Applied Ecology"),
	}}
	stdout, stderr, err := executeWithFake(t, fake, "search", "ecology", "--host", "ncstate.pressbooks.pub", "--agent")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Fatalf("stdout must stay one line: %q", stdout)
	}
	if !strings.Contains(stderr, "ncstate.pressbooks.pub") {
		t.Fatalf("expected crawl progress on stderr, got %q", stderr)
	}
}

// A failing network must not fail the search, and a caller must be able to
// tell a genuinely empty result from a partial one — and, per this fix
// round, must be able to tell why each network was skipped.
func TestSearchSkipsFailingNetworksAndSaysSo(t *testing.T) {
	if len(pressbooks.Networks()) <= sweepConcurrency {
		t.Fatalf("test setup: %d bundled networks does not exceed sweepConcurrency (%d)", len(pressbooks.Networks()), sweepConcurrency)
	}
	// Same reasoning as TestSearchSweepsEveryBundledNetwork: this test is
	// about skip reporting, not pacing.
	withBackendPace(t, time.Millisecond)
	fake := &fakeClient{
		catalogs:   map[string]pressbooks.Catalog{},
		catalogErr: map[string]error{"ecampusontario.pressbooks.pub": errBlockedForTest},
	}
	for _, n := range pressbooks.Networks() {
		fake.catalogs[n.Host] = catalogOf(n.Host, "Applied Ecology")
	}

	stdout, stderr, err := executeWithFake(t, fake, "search", "ecology", "--agent", "--limit", "3")
	if err != nil {
		t.Fatalf("one blocked network should not fail the search: %v", err)
	}
	var got struct {
		OK   bool `json:"ok"`
		Meta struct {
			SkippedNetworks []struct {
				Host   string `json:"host"`
				Reason string `json:"reason"`
			} `json:"skipped_networks"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || len(got.Meta.SkippedNetworks) != 1 ||
		got.Meta.SkippedNetworks[0].Host != "ecampusontario.pressbooks.pub" ||
		got.Meta.SkippedNetworks[0].Reason != "blocked_by_host" {
		t.Fatalf("skipped networks not reported with a reason: %#v", got.Meta)
	}
	if !strings.Contains(stderr, "ecampusontario.pressbooks.pub") || !strings.Contains(stderr, "blocked_by_host") {
		t.Fatalf("a skipped network should be named on stderr with its reason: %q", stderr)
	}
}

// TestSearchKeepsPartialNetworkBooksAndReportsIt is the CLI-level half of
// the Critical fix: a network whose crawl only got partway (ErrPartialCatalog,
// books already synced alongside a non-nil error) must still contribute its
// books to the search result — they are real books, not a fabrication — but
// must also be named in meta.skipped_networks, exactly like a wholly failed
// network already is. Before this fix, sweepNetworks branched only on
// r.err != nil and dropped a partial network's books entirely once Catalog
// started returning a non-nil error for that case; before *that* fix,
// Catalog returned a nil error for a partial crawl and this case was
// invisible to the caller altogether.
func TestSearchKeepsPartialNetworkBooksAndReportsIt(t *testing.T) {
	if len(pressbooks.Networks()) <= sweepConcurrency {
		t.Fatalf("test setup: %d bundled networks does not exceed sweepConcurrency (%d)", len(pressbooks.Networks()), sweepConcurrency)
	}
	withBackendPace(t, time.Millisecond)
	partial := catalogOf("ecampusontario.pressbooks.pub", "Applied Ecology")
	partial.TotalBooks = 40
	fake := &fakeClient{
		catalogs: map[string]pressbooks.Catalog{
			"ecampusontario.pressbooks.pub": partial,
		},
		catalogErr: map[string]error{
			"ecampusontario.pressbooks.pub": fmt.Errorf("%w: %w", pressbooks.ErrPartialCatalog, errBlockedForTest),
		},
	}
	for _, n := range pressbooks.Networks() {
		if n.Host == "ecampusontario.pressbooks.pub" {
			continue
		}
		fake.catalogs[n.Host] = catalogOf(n.Host, "Unrelated Book")
	}

	stdout, stderr, err := executeWithFake(t, fake, "search", "ecology", "--agent")
	if err != nil {
		t.Fatalf("a partial network should not fail the search: %v", err)
	}
	var got struct {
		OK   bool `json:"ok"`
		Data []struct {
			Title string `json:"title"`
		} `json:"data"`
		Meta struct {
			SkippedNetworks []struct {
				Host   string `json:"host"`
				Reason string `json:"reason"`
			} `json:"skipped_networks"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range got.Data {
		if d.Title == "Applied Ecology" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a partial network's books must still appear in the result: %#v", got.Data)
	}
	if len(got.Meta.SkippedNetworks) != 1 || got.Meta.SkippedNetworks[0].Host != "ecampusontario.pressbooks.pub" {
		t.Fatalf("a partial network must still be named in meta.skipped_networks: %#v", got.Meta)
	}
	if !strings.Contains(stderr, "ecampusontario.pressbooks.pub") {
		t.Fatalf("a partial network must still be named on stderr: %q", stderr)
	}
}

func TestBooksRequiresAHost(t *testing.T) {
	fake := &fakeClient{catalogs: map[string]pressbooks.Catalog{}}
	stdout, _, err := executeWithFake(t, fake, "books", "--agent")
	if err == nil {
		t.Fatal("books without --host should fail")
	}
	if code := agentErrorCode(t, stdout); code != "invalid_arguments" {
		t.Fatalf("error code = %q", code)
	}
}

// --host accepts any Pressbooks host, not only a bundled one.
func TestBooksAcceptsAnUnbundledHost(t *testing.T) {
	fake := &fakeClient{catalogs: map[string]pressbooks.Catalog{
		"books.example": catalogOf("books.example", "A Local Book"),
	}}
	stdout, _, err := executeWithFake(t, fake, "books", "--host", "books.example", "--agent")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "A Local Book") {
		t.Fatalf("an unbundled host should still be readable: %s", stdout)
	}
}

// TestBooksWarnsOnAPartialCatalog covers the other CLI-level half of the
// Critical fix: books --host must not treat a partial catalog the same as a
// total failure (it must still list the books that did arrive), and must
// not treat it as an ordinary success either (it must say, on stderr, that
// the listing is incomplete and how much of the network it actually holds).
func TestBooksWarnsOnAPartialCatalog(t *testing.T) {
	partial := catalogOf("ncstate.pressbooks.pub", "Applied Ecology")
	partial.TotalBooks = 34
	fake := &fakeClient{
		catalogs: map[string]pressbooks.Catalog{"ncstate.pressbooks.pub": partial},
		catalogErr: map[string]error{
			"ncstate.pressbooks.pub": fmt.Errorf("%w: %w", pressbooks.ErrPartialCatalog, errBlockedForTest),
		},
	}
	stdout, stderr, err := executeWithFake(t, fake, "books", "--host", "ncstate.pressbooks.pub")
	if err != nil {
		t.Fatalf("a partial catalog should not fail books: %v", err)
	}
	if !strings.Contains(stdout, "Applied Ecology") {
		t.Fatalf("a partial catalog's books must still be listed: %q", stdout)
	}
	if !strings.Contains(stderr, "incomplete") || !strings.Contains(stderr, "1") || !strings.Contains(stderr, "34") {
		t.Fatalf("books must warn on stderr how much of the network it holds: %q", stderr)
	}
}

// A total failure (no books at all) must still fail books outright, not be
// mistaken for a partial catalog with zero books synced.
func TestBooksFailsOnATotalFailure(t *testing.T) {
	fake := &fakeClient{catalogErr: map[string]error{"ncstate.pressbooks.pub": errBlockedForTest}}
	_, _, err := executeWithFake(t, fake, "books", "--host", "ncstate.pressbooks.pub")
	if err == nil {
		t.Fatal("a wholly failed catalog should fail books")
	}
}

// A bad --limit must fail before the crawl runs, not after paying for it.
// catalogErr is set for the target host to errUnexpectedCall, so if the
// bounds check were only applied after fetching, the command would fail with
// a generic error rather than the expected invalid_limit.
func TestBooksValidatesLimitBeforeCrawling(t *testing.T) {
	fake := &fakeClient{catalogErr: map[string]error{"ncstate.pressbooks.pub": errUnexpectedCall}}
	stdout, _, err := executeWithFake(t, fake, "books", "--host", "ncstate.pressbooks.pub", "--limit", "-1", "--agent")
	if err == nil {
		t.Fatal("a negative limit should fail")
	}
	if code := agentErrorCode(t, stdout); code != "invalid_limit" {
		t.Fatalf("error code = %q, want invalid_limit (the crawl should never have run)", code)
	}
}

// The same bad-limit check must run before a full, ~974-request sweep, not
// only before a single host's crawl. Every bundled host is wired to fail
// loudly if it is ever contacted, so this would surface as something other
// than invalid_limit if the sweep ran first.
func TestSearchValidatesLimitBeforeSweeping(t *testing.T) {
	fake := &fakeClient{catalogErr: map[string]error{}}
	for _, n := range pressbooks.Networks() {
		fake.catalogErr[n.Host] = errUnexpectedCall
	}
	stdout, _, err := executeWithFake(t, fake, "search", "ecology", "--limit", "-1", "--agent")
	if err == nil {
		t.Fatal("a negative limit should fail")
	}
	if code := agentErrorCode(t, stdout); code != "invalid_limit" {
		t.Fatalf("error code = %q, want invalid_limit (the sweep should never have run)", code)
	}
}

// Several bundled networks are individual *.pressbooks.pub subdomains that
// share one physical serving backend; everything else is its own backend.
func TestBackendKeyGroupsPressbooksPubSubdomains(t *testing.T) {
	cases := map[string]string{
		"ecampusontario.pressbooks.pub": "pressbooks.pub",
		"ncstate.pressbooks.pub":        "pressbooks.pub",
		"pressbooks.pub":                "pressbooks.pub",
		"milnepublishing.geneseo.edu":   "milnepublishing.geneseo.edu",
		"pressbooks.bccampus.ca":        "pressbooks.bccampus.ca",
		"open.library.okstate.edu":      "open.library.okstate.edu",
	}
	for host, want := range cases {
		if got := backendKey(host); got != want {
			t.Errorf("backendKey(%q) = %q, want %q", host, got, want)
		}
	}
}

// concurrencyTrackingClient is a pressbooksClient double built for this test
// alone (not the shared fakeClient, which has no notion of overlapping
// calls): it records, per backend key, how many Catalog calls are
// simultaneously in flight, the peak observed, and each call's start/end
// time — the only way to prove backendLimiter actually serializes and paces
// crawls sharing a backend, rather than merely compiling.
type concurrencyTrackingClient struct {
	mu       sync.Mutex
	inFlight map[string]int
	peak     map[string]int
	calls    map[string][]struct{ start, end time.Time }
}

func newConcurrencyTrackingClient() *concurrencyTrackingClient {
	return &concurrencyTrackingClient{
		inFlight: map[string]int{},
		peak:     map[string]int{},
		calls:    map[string][]struct{ start, end time.Time }{},
	}
}

func (c *concurrencyTrackingClient) Catalog(_ context.Context, host string, _ func(int, int)) (pressbooks.Catalog, error) {
	key := backendKey(host)
	start := time.Now()
	c.mu.Lock()
	c.inFlight[key]++
	if c.inFlight[key] > c.peak[key] {
		c.peak[key] = c.inFlight[key]
	}
	c.mu.Unlock()

	// Long enough that two overlapping calls sharing a backend would
	// unmistakably both be in flight at once if nothing serialized them.
	time.Sleep(20 * time.Millisecond)

	end := time.Now()
	c.mu.Lock()
	c.inFlight[key]--
	c.calls[key] = append(c.calls[key], struct{ start, end time.Time }{start, end})
	c.mu.Unlock()
	return pressbooks.Catalog{Host: host}, nil
}

func (c *concurrencyTrackingClient) LiveBookCount(context.Context, string) (int, error) {
	return 0, errUnexpectedCall
}

func (c *concurrencyTrackingClient) BookMetadata(context.Context, pressbooks.BookRef) (pressbooks.Book, error) {
	return pressbooks.Book{}, errUnexpectedCall
}

func (c *concurrencyTrackingClient) TOC(context.Context, pressbooks.BookRef) (pressbooks.TOC, error) {
	return pressbooks.TOC{}, errUnexpectedCall
}

func (c *concurrencyTrackingClient) PageContent(context.Context, pressbooks.BookRef, pressbooks.Page) ([]byte, string, error) {
	return nil, "", errUnexpectedCall
}

func (c *concurrencyTrackingClient) ExportFormats(context.Context, pressbooks.BookRef) ([]string, error) {
	return nil, errUnexpectedCall
}

func (c *concurrencyTrackingClient) Download(context.Context, string, io.Writer) (int64, string, error) {
	return 0, "", errUnexpectedCall
}

// testBackendPace is what the two tests below use in place of the
// production backendPace: they are measuring the pacing and serialization
// mechanism itself, so unlike every other sweep test here they must keep a
// real, nonzero pace rather than collapsing it to nothing — just a much
// smaller one than production needs, so the assertions stay fast.
const testBackendPace = 30 * time.Millisecond

// This is the test that would have caught the original defect: three
// bundled *.pressbooks.pub hosts sharing one backend must never crawl at
// the same time, even though sweepConcurrency (3) would otherwise let all
// three run at once, and two genuinely independent hosts must be
// unaffected by that restriction.
func TestSweepNetworksSerializesCrawlsSharingABackend(t *testing.T) {
	withBackendPace(t, testBackendPace)
	fake := newConcurrencyTrackingClient()
	hosts := []string{
		"uw.pressbooks.pub", "boisestate.pressbooks.pub", "uen.pressbooks.pub", // share "pressbooks.pub"
		"milnepublishing.geneseo.edu", "open.library.okstate.edu", // each its own backend
	}

	sweepNetworks(context.Background(), fake, hosts, nil)

	if peak := fake.peak["pressbooks.pub"]; peak > 1 {
		t.Fatalf("peak concurrent crawls sharing the pressbooks.pub backend = %d, want at most 1", peak)
	}
	for _, host := range []string{"milnepublishing.geneseo.edu", "open.library.okstate.edu"} {
		if peak := fake.peak[host]; peak != 1 {
			t.Fatalf("independent host %s: peak concurrency = %d, want 1 (it should still run)", host, peak)
		}
	}
}

// The pacing half of the same fix: even serialized one-at-a-time, one crawl
// on a shared backend must not start back-to-back with the previous one's
// last request. A generous tolerance keeps this from being flaky under
// scheduler jitter while still catching a regression that removes the pace
// entirely (a zero or near-zero gap).
func TestSweepNetworksPacesCrawlsSharingABackend(t *testing.T) {
	withBackendPace(t, testBackendPace)
	fake := newConcurrencyTrackingClient()
	hosts := []string{"uw.pressbooks.pub", "boisestate.pressbooks.pub", "uen.pressbooks.pub"}

	sweepNetworks(context.Background(), fake, hosts, nil)

	calls := fake.calls["pressbooks.pub"]
	if len(calls) != len(hosts) {
		t.Fatalf("got %d recorded calls, want %d", len(calls), len(hosts))
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].start.Before(calls[j].start) })
	const tolerance = testBackendPace / 2
	for i := 1; i < len(calls); i++ {
		gap := calls[i].start.Sub(calls[i-1].end)
		if gap < testBackendPace-tolerance {
			t.Fatalf("gap between successive same-backend crawls = %v, want at least ~%v (testBackendPace)", gap, testBackendPace)
		}
	}
}

// A crawl failing outright must still release both the semaphore and the
// backend lock, or one failing host in a shared-backend group would wedge
// every host after it in that group for the rest of the sweep.
func TestSweepNetworksReleasesLocksAfterAFailingCrawl(t *testing.T) {
	// This test is about lock release on failure, not pacing.
	withBackendPace(t, time.Millisecond)
	fake := &fakeClient{catalogErr: map[string]error{
		"uw.pressbooks.pub":         errBlockedForTest,
		"boisestate.pressbooks.pub": errBlockedForTest,
	}}
	books, skipped := sweepNetworks(context.Background(), fake, []string{"uw.pressbooks.pub", "boisestate.pressbooks.pub"}, nil)
	if len(books) != 0 {
		t.Fatalf("got %d books from two failing hosts, want 0", len(books))
	}
	if len(skipped) != 2 {
		t.Fatalf("got %d skipped hosts, want 2 (a wedged lock would strand the second): %#v", len(skipped), skipped)
	}
}
