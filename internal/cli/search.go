package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
	"github.com/spf13/cobra"
)

// agentBookSummary is one book in a books or search response.
type agentBookSummary struct {
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle,omitempty"`
	Authors     []string `json:"authors,omitempty"`
	LicenseName string   `json:"license_name,omitempty"`
	Host        string   `json:"host"`
	NetworkName string   `json:"network_name,omitempty"`
	WordCount   int      `json:"word_count"`
}

func summarizeBooks(books []pressbooks.Book) []agentBookSummary {
	out := make([]agentBookSummary, 0, len(books))
	for _, b := range books {
		out = append(out, agentBookSummary{
			URL:         b.URL,
			Title:       b.Title,
			Subtitle:    b.Subtitle,
			Authors:     b.Authors,
			LicenseName: b.LicenseName,
			Host:        b.Host,
			NetworkName: b.NetworkName,
			WordCount:   b.WordCount,
		})
	}
	return out
}

// sweepConcurrency bounds how many networks a search crawls at once, total,
// across every backend combined. Deliberately modest, for the same reason
// doctorConcurrency and crawlConcurrency are: these are university servers,
// not a load-testing target, and an all-network sweep already means
// hundreds of requests. backendLimiter applies a further, per-backend cap
// underneath this one (see backendKey): sweepConcurrency alone is not
// enough when several bundled hosts turn out to share one backend.
const sweepConcurrency = 3

// skippedNetwork names one network a sweep could not read, and the
// classified reason why, so a caller can tell "this host is blocked" from
// "we hammered it" from "it timed out" rather than an unexplained gap in
// the result.
type skippedNetwork struct {
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

// sweepProgress reports per-host crawl progress during a sweep: host is the
// network currently being paged, current and total are pages fetched and
// pages needed for that host's crawl.
type sweepProgress func(host string, current, total int)

// sweepNetworks crawls hosts concurrently, bounded by sweepConcurrency and,
// per backend, by backendLimiter, and returns every book found along with
// the networks whose crawl failed and why. A failing network must not fail
// the whole sweep: it is skipped here rather than returned as an error, so
// a caller can tell a genuinely empty result from a partial one. Results
// are assembled in the order hosts were given, not arrival order, so a run
// is deterministic regardless of which host answers first.
func sweepNetworks(ctx context.Context, c pressbooksClient, hosts []string, progress sweepProgress) ([]pressbooks.Book, []skippedNetwork) {
	type outcome struct {
		books []pressbooks.Book
		err   error
	}
	results := make([]outcome, len(hosts))
	sem := make(chan struct{}, sweepConcurrency)
	limiter := newBackendLimiter()
	var wg sync.WaitGroup
	// progressMu serializes calls into the caller's progress function: with
	// several hosts crawling at once, each on its own goroutine, an
	// unsynchronized progress callback writing to a shared io.Writer (as
	// every caller here does, straight to stderr) would be a data race.
	var progressMu sync.Mutex
	wg.Add(len(hosts))
	for i, host := range hosts {
		go func(i int, host string) {
			defer wg.Done()
			// The backend lock is acquired before the sweepConcurrency
			// semaphore, deliberately: waiting your turn on a shared backend
			// must not itself consume one of only sweepConcurrency global
			// slots, or two backend-mates could sit blocked holding slots
			// that a genuinely independent host needs, cutting its effective
			// concurrency far below the limit this exists to guarantee it.
			if err := limiter.acquire(ctx, host); err != nil {
				results[i] = outcome{err: err}
				return
			}
			defer limiter.release(host)
			sem <- struct{}{}
			defer func() { <-sem }()
			catalog, err := c.Catalog(ctx, host, func(current, total int) {
				if progress == nil {
					return
				}
				progressMu.Lock()
				defer progressMu.Unlock()
				progress(host, current, total)
			})
			results[i] = outcome{books: catalog.Books, err: err}
		}(i, host)
	}
	wg.Wait()

	var books []pressbooks.Book
	var skipped []skippedNetwork
	for i, r := range results {
		if r.err != nil {
			// A partial catalog (ErrPartialCatalog) still carries whatever
			// books its crawl landed before the failure — real books, worth
			// keeping in the result — but the network is still named here as
			// a caller must be told its contribution was truncated, exactly
			// as a wholly failed network already is. Without this branch, a
			// network that crawled 40 of 3,078 books before a mid-crawl 403
			// would contribute those 40 with no sign anything was wrong.
			skipped = append(skipped, skippedNetwork{Host: hosts[i], Reason: classifyAgentError(r.err).Code})
		}
		books = append(books, r.books...)
	}
	return books, skipped
}

func stderrProgress(w io.Writer) sweepProgress {
	return func(host string, current, total int) {
		_, _ = fmt.Fprintf(w, "crawling %s (page %d/%d)\n", host, current, total)
	}
}

// catalogOrPartial handles a single-host Catalog result for books and
// search --host: a total failure is returned unchanged, but a partial
// catalog (ErrPartialCatalog) is treated as success — its books are real and
// still worth listing or searching — after warning on stderr, by name, how
// much of the network actually landed. Without this, a command driven
// straight off Catalog's returned error would either silently show a
// truncated listing as complete (the bug this exists to fix) or, now that a
// partial crawl returns a non-nil error at all, treat a partial crawl as a
// total failure instead — trading one silent wrong answer for a needlessly
// loud one.
func catalogOrPartial(w io.Writer, host string, catalog pressbooks.Catalog, err error) (pressbooks.Catalog, error) {
	if err == nil {
		return catalog, nil
	}
	if !errors.Is(err, pressbooks.ErrPartialCatalog) {
		return pressbooks.Catalog{}, err
	}
	_, _ = fmt.Fprintf(w, "warning: %s catalog is incomplete (%d of %d books; crawl stopped: %v)\n",
		host, len(catalog.Books), catalog.TotalBooks, err)
	return catalog, nil
}

func booksCmd(f *flags) *cobra.Command {
	var host string
	var limit int
	var offset int
	cmd := &cobra.Command{
		Use:   "books",
		Short: "List one Pressbooks network's books.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host = strings.ToLower(strings.TrimSpace(host))
			if host == "" {
				// A bare "books" would otherwise sweep every bundled network by
				// accident, roughly 974 requests against other people's servers for
				// a command whose whole point is to look at one of them.
				return &agentInputError{
					Code:       "invalid_arguments",
					Message:    "books requires --host",
					Suggestion: "Run 'networks' for the bundled hosts, or pass any Pressbooks host with --host.",
				}
			}
			if f.agent {
				// Fail on an out-of-range --limit/--offset before crawling, not
				// after: the bounds that do not depend on the total are known
				// up front, and a single host's crawl can still be hundreds of
				// requests.
				if _, err := agentWindowBounds(offset, limit, 10, 100); err != nil {
					return err
				}
			}
			c, ctx := clientAndContext(f)
			progress := stderrProgress(cmd.ErrOrStderr())
			catalog, err := c.Catalog(ctx, host, func(current, total int) { progress(host, current, total) })
			catalog, err = catalogOrPartial(cmd.ErrOrStderr(), host, catalog, err)
			if err != nil {
				return err
			}
			books := catalog.Books
			if f.agent {
				start, end, meta, err := agentWindow(len(books), offset, limit, 10, 100)
				if err != nil {
					return err
				}
				return writeAgentSuccess(cmd.OutOrStdout(), "books", summarizeBooks(books[start:end]), &meta)
			}
			if f.asJSON {
				return pressbooks.WriteJSON(cmd.OutOrStdout(), books)
			}
			return writeBooksTable(cmd.OutOrStdout(), books)
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "Pressbooks host to list (required); any reachable Pressbooks host, not only a bundled one")
	cmd.Flags().IntVar(&limit, "limit", 0, "Agent result limit (default 10, maximum 100)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Agent result offset")
	return cmd
}

func searchCmd(f *flags) *cobra.Command {
	var host string
	var limit int
	var offset int
	cmd := &cobra.Command{
		Use:   "search QUERY",
		Short: "Find books by title, subtitle or author (not chapter titles or book text)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.agent {
				// Fail on an out-of-range --limit/--offset before the sweep
				// starts, not after: a cold, all-network sweep is roughly 974
				// requests, so a typo in --limit must not be discovered only
				// once every one of them has already run.
				if _, err := agentWindowBounds(offset, limit, 5, 100); err != nil {
					return err
				}
			}
			c, ctx := clientAndContext(f)
			progress := stderrProgress(cmd.ErrOrStderr())

			var books []pressbooks.Book
			var skipped []skippedNetwork
			host = strings.ToLower(strings.TrimSpace(host))
			if host != "" {
				catalog, err := c.Catalog(ctx, host, func(current, total int) { progress(host, current, total) })
				catalog, err = catalogOrPartial(cmd.ErrOrStderr(), host, catalog, err)
				if err != nil {
					return err
				}
				books = catalog.Books
			} else {
				networks := pressbooks.Networks()
				hosts := make([]string, 0, len(networks))
				for _, n := range networks {
					hosts = append(hosts, n.Host)
				}
				books, skipped = sweepNetworks(ctx, c, hosts, progress)
				if len(skipped) > 0 {
					// Named on stderr with its reason, and reported in
					// meta.skipped_networks below, so a caller can tell a
					// genuinely empty result from a partial one, and tell
					// "blocked" from "rate_limited" from "timeout".
					parts := make([]string, len(skipped))
					for i, s := range skipped {
						parts[i] = fmt.Sprintf("%s (%s)", s.Host, s.Reason)
					}
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "skipped %d network(s) that failed: %s\n",
						len(skipped), strings.Join(parts, ", "))
				}
			}

			matches := pressbooks.SearchBooks(books, args[0])
			if f.agent {
				start, end, meta, err := agentWindow(len(matches), offset, limit, 5, 100)
				if err != nil {
					return err
				}
				meta.SkippedNetworks = skipped
				return writeAgentSuccess(cmd.OutOrStdout(), "search", summarizeBooks(matches[start:end]), &meta)
			}
			if f.asJSON {
				return pressbooks.WriteJSON(cmd.OutOrStdout(), matches)
			}
			return writeBooksTable(cmd.OutOrStdout(), matches)
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "Limit the search to one Pressbooks host instead of sweeping every bundled network")
	cmd.Flags().IntVar(&limit, "limit", 0, "Agent result limit (default 5, maximum 100)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Agent result offset")
	return cmd
}

func writeBooksTable(w io.Writer, books []pressbooks.Book) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "TITLE\tHOST\tAUTHORS\tLICENSE"); err != nil {
		return err
	}
	for _, b := range books {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.Title, b.Host, strings.Join(b.Authors, ", "), b.LicenseName); err != nil {
			return err
		}
	}
	return tw.Flush()
}
