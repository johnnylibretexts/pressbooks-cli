package pressbooks

import (
	"context"
	"errors"
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errSkippedAfterEarlierFailure marks a page a crawl never attempted because
// a lower-numbered page in the same crawl had already failed. The
// longest-complete-prefix rule can never keep a page past that point
// regardless of what this one would have returned, so it is skipped rather
// than spending a request whose result could never be used.
var errSkippedAfterEarlierFailure = errors.New("skipped: an earlier page in this crawl already failed")

// ErrPartialCatalog marks a Catalog returned alongside at least one
// successfully-synced page, whose crawl nonetheless stopped before every
// page arrived (an ordinary page failure — a 403 from a rate-limited
// backend, a 5xx, a transport error — at some page index > 0). Before this
// existed, that case returned ctx.Err() as the error, which is nil for an
// ordinary page failure: the caller saw a nil error and a Books slice that
// looked like a small, complete network rather than a truncated one. Every
// caller that walks a catalog must check for this (with errors.Is) rather
// than trusting a nil error to mean "complete" — Catalog.Complete() is the
// field that actually answers that, and is what a caller should consult
// when it wants to keep going rather than merely report the gap.
//
// A genuine context cancellation (ctx.Err() != nil — a user's Ctrl-C, a
// caller's own deadline) is not wrapped in this: it is returned as-is, since
// it is already a non-nil error a caller cannot mistake for success.
var ErrPartialCatalog = errors.New("catalog crawl stopped before every page synced")

// booksPerPage is the hard upstream cap. per_page=100 is refused with
// rest_invalid_param and the message "per_page must be between 1 (inclusive)
// and 10 (inclusive)", so a 3,000-book network is 300 requests.
const booksPerPage = 10

// crawlConcurrency is how many pages of one network are fetched at once.
// Deliberately modest: these are university servers. It is perHostConcurrency
// (backend.go) under its crawl-specific name, so this file's own comments
// about "crawlConcurrency" stay meaningful without also duplicating the
// number itself.
const crawlConcurrency = perHostConcurrency

// maxCrawlPages bounds how many pages a single crawl will ever fetch. The
// largest network this tool knows of is 308 pages; this cap sits far above
// that so real growth is unaffected, while turning a hostile or broken
// X-WP-Total header into a bounded crawl rather than an out-of-memory
// allocation or a multi-thousand-request storm against someone else's
// server.
const maxCrawlPages = 1000

// crawlPace is the minimum gap enforced between successive page dispatches
// within a single crawl. crawlConcurrency alone bounds how many pages are
// in flight at once, but not how quickly new ones start: the largest
// bundled network (eCampusOntario) is 308 pages, and firing them four at a
// time as fast as the server answers is the single largest burst this tool
// produces against any one backend — including a backend several other
// bundled hosts share with it. This exists purely to be gentle with a large
// crawl, not to fix a bug in it.
//
// Deliberately smaller than the cli package's backendPace, which paces whole
// crawls against each other rather than smoothing dispatch within one: that
// is a coarser courtesy between hosts, this is a finer one inside a single
// host's own crawl. Neither interval was tuned against measurement — this
// machine was rate-limited at the time both were chosen, which is what
// exposed the need for either — so both err deliberately toward gentleness
// rather than toward a number backed by data.
//
// A var, not a const, for the same reason Client.backoff and the cli
// package's backendPace are: production wants the real value, but a test
// driving an in-process httptest server is not the university server this
// pace exists to protect, and should not pay real wall-clock time for it.
var crawlPace = 50 * time.Millisecond

// pageDispatchPacer enforces crawlPace between successive page dispatches
// within one crawl. It is scoped to a single (*Client).Catalog call — a
// fresh instance every time, never shared across crawls or hosts.
// Coordinating concurrency and pacing *across* different hosts' crawls
// (some of which turn out to share a backend) is the cli package's
// responsibility, not this domain package's; this only smooths dispatch
// inside the one crawl it belongs to.
type pageDispatchPacer struct {
	mu   sync.Mutex
	next time.Time
}

// wait blocks the caller until it is its turn to dispatch, then reserves
// the next slot crawlPace later. A page skipped by the low-water mark (see
// errSkippedAfterEarlierFailure) must never call this: it spends no real
// request, so it must not consume a pacing turn either.
func (p *pageDispatchPacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	start := p.next
	if start.Before(now) {
		start = now
	}
	p.next = start.Add(crawlPace)
	p.mu.Unlock()

	delay := time.Until(start)
	if delay <= 0 {
		return nil
	}
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type wirePerson struct {
	Name string `json:"name"`
}

type wireLicense struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type wireNetwork struct {
	Host string `json:"host"`
	Name string `json:"name"`
}

type wireMetadata struct {
	Name                string       `json:"name"`
	AlternativeHeadline string       `json:"alternativeHeadline"`
	InLanguage          string       `json:"inLanguage"`
	CopyrightYear       flexString   `json:"copyrightYear"`
	Image               string       `json:"image"`
	WordCount           int          `json:"wordCount"`
	Author              []wirePerson `json:"author"`
	License             wireLicense  `json:"license"`
	Network             wireNetwork  `json:"network"`
}

type wireBook struct {
	ID       int          `json:"id"`
	Link     string       `json:"link"`
	Metadata wireMetadata `json:"metadata"`
}

// decodeProse decodes HTML entities in fields WordPress stores as post
// content. WordPress stores this text HTML-encoded, and the Pressbooks API
// returns it that way verbatim: a real subtitle arrives as "An
// Introduction &amp; Overview", not "An Introduction & Overview". Left
// alone, that breaks two things, not one: it displays wrong, and it makes
// SearchBooks unmatchable by a query typed with a plain "&" or "'" against a
// haystack that still contains the entity. Applied to subtitle, author
// names, and the network's own display name — plain prose that WordPress
// never lets carry markup — but deliberately not to a title (see
// CleanHTMLTitle, which decodes those instead, once, at the same wire
// boundary) and not to URLs or license identifiers, which are not prose and
// are not affected by this encoding.
func decodeProse(s string) string {
	return strings.TrimSpace(html.UnescapeString(s))
}

func (w wireBook) book(host string) Book {
	authors := make([]string, 0, len(w.Metadata.Author))
	for _, a := range w.Metadata.Author {
		if name := decodeProse(a.Name); name != "" {
			authors = append(authors, name)
		}
	}
	url := strings.TrimSpace(w.Link)
	if url != "" && !strings.HasSuffix(url, "/") {
		url += "/"
	}
	return Book{
		URL:  url,
		Host: host,
		// Title goes through CleanHTMLTitle, not decodeProse: a title can
		// carry real markup (WordPress allows emphasis in a post title),
		// and CleanHTMLTitle both decodes entities and strips that markup
		// in one pass. This is also the one and only place a book title is
		// decoded — ExtractPage must not decode it again, or a title whose
		// display text itself contains entity-like syntax (a book called
		// "Rock &amp; Roll", stored as "Rock &amp;amp; Roll") loses a
		// character on the second pass.
		Title:         CleanHTMLTitle(w.Metadata.Name),
		Subtitle:      decodeProse(w.Metadata.AlternativeHeadline),
		Authors:       authors,
		LicenseName:   w.Metadata.License.Name,
		LicenseURL:    w.Metadata.License.URL,
		Language:      w.Metadata.InLanguage,
		CopyrightYear: w.Metadata.CopyrightYear.String(),
		CoverURL:      w.Metadata.Image,
		WordCount:     w.Metadata.WordCount,
		NetworkName:   decodeProse(w.Metadata.Network.Name),
	}
}

// scheme is https everywhere except in tests, which point baseScheme at a
// plain-HTTP httptest server.
func (c *Client) scheme() string {
	if c.baseScheme == "" {
		return "https"
	}
	return c.baseScheme
}

// invalidAuthorityChars are characters that cannot appear in a URL authority
// (or would let a host smuggle in a path or query of its own if interpolated
// unescaped). ':' is deliberately allowed: an httptest server's authority is
// 127.0.0.1:PORT.
const invalidAuthorityChars = "/\\?#@\"'<>`"

// validateHost reports whether host is safe to interpolate directly into a
// request URL's authority component. It is validated, not escaped: an earlier
// version passed host through url.PathEscape, which turns the colon in
// 127.0.0.1:PORT into %3A and produces a URL that cannot resolve, so instead
// anything that could not appear in a real authority is rejected outright.
func validateHost(host string) error {
	if host == "" {
		return fmt.Errorf("empty host")
	}
	for _, r := range host {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("host %q contains whitespace or a control character", host)
		}
		if strings.ContainsRune(invalidAuthorityChars, r) {
			return fmt.Errorf("host %q contains %q, which cannot appear in a URL authority", host, string(r))
		}
	}
	return nil
}

func (c *Client) booksURL(host string, page, perPage int) string {
	return fmt.Sprintf("%s://%s/wp-json/pressbooks/v2/books?per_page=%d&page=%d",
		c.scheme(), host, perPage, page)
}

// LiveBookCount reads a network's current book count from the X-WP-Total
// header of a one-book request. This is what makes staleness one request to
// check rather than a full re-enumeration.
func (c *Client) LiveBookCount(ctx context.Context, host string) (int, error) {
	if err := validateHost(host); err != nil {
		return 0, err
	}
	var page []wireBook
	header, err := c.GetJSON(ctx, c.booksURL(host, 1, 1), &page)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(header.Get("X-WP-Total"))
}

// Catalog returns a network's books, from cache when it is fresh and complete
// and the live count still agrees, and by crawling otherwise. progress may be
// nil; when set it is called with pages fetched and pages needed. A negative
// X-WP-Total is rejected outright as a malformed response. A crawl never
// fetches more than maxCrawlPages regardless of what the host claims; when
// that cap binds, the returned Catalog reports itself incomplete via
// Complete() rather than passing off the truncated fetch as the whole
// network.
func (c *Client) Catalog(ctx context.Context, host string, progress func(current, total int)) (Catalog, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if err := validateHost(host); err != nil {
		return Catalog{}, err
	}

	cached, hit := readCatalogCache(host, c.cacheTTL)

	// probed tracks whether the block below already spent a request finding
	// out the live count. Every fall-through path from that block — the count
	// changed, or the probe failed permanently — used to re-fetch it a second
	// time; reusing it here means a staleness check never costs more than one
	// request, on either path.
	var total int
	var err error
	probed := false
	if hit && cached.Complete() {
		total, err = c.LiveBookCount(ctx, host)
		probed = true
		if err == nil && total == cached.TotalBooks {
			return cached, nil
		}
		if err != nil && !isPermanent(err) {
			// The network is unreachable right now. A complete cached listing
			// is far better than failing the caller's search.
			return cached, nil
		}
	}
	if !probed {
		total, err = c.LiveBookCount(ctx, host)
	}
	if err != nil {
		if hit {
			// A cached listing beats failing the caller's search — but only a
			// complete one may be handed back as plain success. An incomplete
			// cache entry is the partial catalog some earlier run wrote, and
			// returning it with a nil error recreates exactly the bug
			// ErrPartialCatalog exists to prevent, one run later.
			return cachedOrPartial(cached, err)
		}
		return Catalog{}, err
	}
	// A negative count is a malformed response, not a real network size: it
	// would otherwise report a nonsensical catalog rather than failing loudly.
	if total < 0 {
		return Catalog{}, fmt.Errorf("host %s reported a negative book count from X-WP-Total (%d)", host, total)
	}
	totalPages := (total + booksPerPage - 1) / booksPerPage

	// crawlPages bounds how many pages this call will actually fetch. total
	// comes straight from a response header with no ceiling of its own: a
	// hostile or merely broken host could claim an enormous count and turn
	// this into either a multi-gigabyte allocation before a single page is
	// fetched, or a request storm against a server crawlConcurrency exists to
	// go easy on. The largest network this tool knows of is 308 pages, so
	// capping far above that costs nothing real while making either failure
	// mode impossible; totalPages itself is left uncapped so Complete() still
	// reports the truncation honestly instead of passing off a partial
	// network as whole.
	crawlPages := totalPages
	if crawlPages > maxCrawlPages {
		crawlPages = maxCrawlPages
	}

	out := Catalog{Host: host, TotalBooks: total, TotalPages: totalPages, SyncedAt: time.Now().UTC()}
	if crawlPages == 0 {
		out.SyncedPages = 0
		writeCatalogCache(out)
		return out, nil
	}

	// minFailIndex is the smallest page index known to have failed so far,
	// starting at crawlPages (a sentinel meaning "no failure yet": every real
	// index is < crawlPages). A page is only ever skipped once its own index
	// is already known to be past the earliest failure, so a page that could
	// still end up inside the longest complete prefix is never denied its own
	// attempt — this is what keeps that prefix identical to what an
	// uncancelled crawl would have produced, while still stopping the
	// hundreds of requests that a large network gains nothing by making once
	// its result can never be kept. An in-flight request is never aborted:
	// only a page that has not yet been dispatched can be skipped, so this
	// can never turn an otherwise-successful lower-index page into a failure.
	var minFailIndex atomic.Int32
	minFailIndex.Store(int32(crawlPages))

	pages := make([][]Book, crawlPages)
	errs := make([]error, crawlPages)
	sem := make(chan struct{}, crawlConcurrency)
	done := make(chan int, crawlPages)
	pacer := &pageDispatchPacer{}
	for i := 0; i < crawlPages; i++ {
		go func(index int) {
			sem <- struct{}{}
			defer func() { <-sem }()
			if int32(index) > minFailIndex.Load() {
				errs[index] = errSkippedAfterEarlierFailure
				done <- index
				return
			}
			fail := func(err error) {
				errs[index] = err
				for {
					old := minFailIndex.Load()
					if int32(index) >= old {
						break
					}
					if minFailIndex.CompareAndSwap(old, int32(index)) {
						break
					}
				}
				done <- index
			}
			// Smooths dispatch across this crawl's own pages; it must run
			// after the low-water-mark check above (a skipped page spends
			// no request, so it must not consume a pacing turn) and before
			// the request itself.
			if err := pacer.wait(ctx); err != nil {
				fail(err)
				return
			}
			var wire []wireBook
			if _, err := c.GetJSON(ctx, c.booksURL(host, index+1, booksPerPage), &wire); err != nil {
				fail(err)
				return
			}
			books := make([]Book, 0, len(wire))
			for _, w := range wire {
				books = append(books, w.book(host))
			}
			pages[index] = books
			done <- index
		}(i)
	}

	fetched := 0
	for i := 0; i < crawlPages; i++ {
		<-done
		fetched++
		if progress != nil {
			progress(fetched, crawlPages)
		}
	}

	// Keep the longest complete prefix. A crawl with a hole in it is not a
	// position anything can resume from, so pages after the first failure are
	// discarded even when they arrived. ctx.Err() is what tells a genuine
	// cancellation (a user's Ctrl-C, a caller's deadline) apart from an
	// ordinary page error: without this check, a cancelled crawl that had
	// already landed its first page would report success with no error at
	// all.
	for i := 0; i < crawlPages; i++ {
		if errs[i] != nil {
			out.SyncedPages = i
			if i == 0 {
				if hit {
					// ctx.Err() is nil for an ordinary page failure, so
					// returning it bare would hand back a possibly-partial
					// cache entry as success. Same guarantee as the
					// non-cached path below.
					if cerr := ctx.Err(); cerr != nil {
						return cached, cerr
					}
					return cachedOrPartial(cached, errs[i])
				}
				if cerr := ctx.Err(); cerr != nil {
					return Catalog{}, cerr
				}
				return Catalog{}, errs[i]
			}
			// Set whenever any books arrived, not only on the complete path
			// below: a partial catalog cached with an empty Name is still
			// cached and still served back on a later hit, so it should carry
			// the same network name a complete one would.
			if len(out.Books) > 0 {
				out.Name = out.Books[0].NetworkName
			}
			// NOTE(resume): this partial catalog is cached as-is, but Catalog
			// (above) only ever reuses a cache hit when cached.Complete() is
			// true — a partial one is never read back as a starting point.
			// The design's section 9 promises a later run resumes from here
			// rather than re-crawling from page 1; that resume path does not
			// exist yet. Until it does, a host that keeps failing mid-crawl
			// (rate-limited, or genuinely down) pays for a full re-crawl on
			// every subsequent call, not just the first one that hit the
			// failure.
			writeCatalogCache(out)
			// A genuine cancellation (ctx.Err() != nil) is already a non-nil
			// error no caller could mistake for success; only an ordinary
			// page failure — ctx.Err() == nil — needs ErrPartialCatalog to
			// keep that same guarantee, since errs[i] alone (a StatusError,
			// ErrBlocked, ...) would otherwise look like the caller simply
			// never checked for an error at all, not like a deliberate signal
			// that this catalog is incomplete.
			if cerr := ctx.Err(); cerr != nil {
				return out, cerr
			}
			return out, fmt.Errorf("%w: %w", ErrPartialCatalog, errs[i])
		}
		out.Books = append(out.Books, pages[i]...)
		out.SyncedPages = i + 1
	}
	if len(out.Books) > 0 {
		out.Name = out.Books[0].NetworkName
	}
	writeCatalogCache(out)
	return out, nil
}

// cachedOrPartial hands back a cache entry that is standing in for a live
// crawl that could not be completed. A complete entry is plain success: it is
// the whole network, merely not fetched just now. An incomplete one is the
// partial catalog an earlier run wrote, and must carry ErrPartialCatalog for
// the same reason the live path does — a nil error alongside a truncated
// Books slice is indistinguishable from a small, complete network, and no
// caller reads Complete().
func cachedOrPartial(cached Catalog, cause error) (Catalog, error) {
	if cached.Complete() {
		return cached, nil
	}
	if cause == nil {
		return cached, ErrPartialCatalog
	}
	return cached, fmt.Errorf("%w: %w", ErrPartialCatalog, cause)
}

// isPermanent reports whether retrying or waiting could ever help.
func isPermanent(err error) bool {
	return errors.Is(err, ErrBlocked) || errors.Is(err, ErrNoAPI)
}

// SearchBooks filters books by title, subtitle and author, case-insensitively.
// Matching is local because the upstream search parameter is accepted and then
// ignored: asking a network for "biology" returns its unfiltered first page.
func SearchBooks(books []Book, query string) []Book {
	query = strings.ToLower(strings.TrimSpace(query))
	out := make([]Book, 0, len(books))
	for _, b := range books {
		if query == "" {
			out = append(out, b)
			continue
		}
		haystack := strings.ToLower(strings.Join(append([]string{b.Title, b.Subtitle}, b.Authors...), " "))
		if strings.Contains(haystack, query) {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}
