package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
)

var (
	errUnexpectedCall = errors.New("the command made a network request it should not have")
	errBlockedForTest = fmt.Errorf("probe: %w", pressbooks.ErrBlocked)
)

type fakeClient struct {
	catalogs   map[string]pressbooks.Catalog
	catalogErr map[string]error
	counts     map[string]int
	countErr   error
	// countErrs lets a single test give different hosts different failures
	// (blocked, unreachable, ...) in the same run, for exercising doctor's
	// concurrent multi-host probe. countErr, when set, still overrides every
	// host, matching the existing single-error tests.
	countErrs map[string]error
	// books, tocs and exportFormats are keyed by BookRef.URL, like catalogs is
	// keyed by host: a book a test never configured must fail loudly with "no
	// fake X for %q" rather than silently returning a zero value that could be
	// mistaken for real data.
	books         map[string]pressbooks.Book
	bookErr       error
	tocs          map[string]pressbooks.TOC
	tocErr        error
	pageHTML      map[int]string
	exportFormats map[string][]string
	exportErr     error
	downloadBody  []byte
	downloadErr   error
	downloadURLs  []string
	cacheDisabled bool
	// pageContentCalls records when each PageContent call happened, in call
	// order. extractPages walks pages sequentially (never concurrently), so
	// this needs no locking; it exists purely so a pacing test can measure
	// the gap between successive page fetches.
	pageContentCalls []time.Time
}

func (f *fakeClient) SetCacheTTL(time.Duration) { f.cacheDisabled = true }

func (f *fakeClient) Catalog(_ context.Context, host string, progress func(int, int)) (pressbooks.Catalog, error) {
	if err, ok := f.catalogErr[host]; ok {
		// A test that also set f.catalogs[host] is expressing a *partial*
		// catalog: books already synced alongside a non-nil error, exactly
		// what a real mid-crawl failure (ErrPartialCatalog) returns. A test
		// that left f.catalogs[host] unset gets the zero-value Catalog{} —
		// a wholly failed network, no books at all — matching every
		// existing caller of this field before partial catalogs existed.
		return f.catalogs[host], err
	}
	if progress != nil {
		progress(1, 1)
	}
	catalog, ok := f.catalogs[host]
	if !ok {
		return pressbooks.Catalog{}, fmt.Errorf("no fake catalog for %q", host)
	}
	return catalog, nil
}

func (f *fakeClient) LiveBookCount(_ context.Context, host string) (int, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	if err, ok := f.countErrs[host]; ok {
		return 0, err
	}
	count, ok := f.counts[host]
	if !ok {
		return 0, fmt.Errorf("no fake count for %q", host)
	}
	return count, nil
}

func (f *fakeClient) BookMetadata(_ context.Context, ref pressbooks.BookRef) (pressbooks.Book, error) {
	if f.bookErr != nil {
		return pressbooks.Book{}, f.bookErr
	}
	book, ok := f.books[ref.URL]
	if !ok {
		return pressbooks.Book{}, fmt.Errorf("no fake book metadata for %q", ref.URL)
	}
	return book, nil
}

func (f *fakeClient) TOC(_ context.Context, ref pressbooks.BookRef) (pressbooks.TOC, error) {
	if f.tocErr != nil {
		return pressbooks.TOC{}, f.tocErr
	}
	toc, ok := f.tocs[ref.URL]
	if !ok {
		return pressbooks.TOC{}, fmt.Errorf("no fake TOC for %q", ref.URL)
	}
	return toc, nil
}

func (f *fakeClient) PageContent(_ context.Context, _ pressbooks.BookRef, page pressbooks.Page) ([]byte, string, error) {
	f.pageContentCalls = append(f.pageContentCalls, time.Now())
	body, ok := f.pageHTML[page.ID]
	if !ok {
		return nil, "", fmt.Errorf("missing fake page %d", page.ID)
	}
	return []byte(body), page.URL, nil
}

func (f *fakeClient) ExportFormats(_ context.Context, ref pressbooks.BookRef) ([]string, error) {
	if f.exportErr != nil {
		return nil, f.exportErr
	}
	formats, ok := f.exportFormats[ref.URL]
	if !ok {
		return nil, fmt.Errorf("no fake export formats for %q", ref.URL)
	}
	return formats, nil
}

func (f *fakeClient) Download(_ context.Context, rawURL string, w io.Writer) (int64, string, error) {
	f.downloadURLs = append(f.downloadURLs, rawURL)
	if f.downloadErr != nil {
		// Mimic a transfer that fails after bytes have already reached the file.
		if len(f.downloadBody) > 0 {
			_, _ = w.Write(f.downloadBody)
		}
		return 0, "", f.downloadErr
	}
	n, err := w.Write(f.downloadBody)
	return int64(n), "book.pdf", err
}

func executeWithFake(t *testing.T, fake *fakeClient, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := executeArgsWithClient(args, &stdout, &stderr, func(time.Duration) pressbooksClient { return fake })
	return stdout.String(), stderr.String(), err
}

func agentErrorCode(t *testing.T, stdout string) string {
	t.Helper()
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode agent error from %q: %v", stdout, err)
	}
	return got.Error.Code
}
