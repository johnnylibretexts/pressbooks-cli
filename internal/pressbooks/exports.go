package pressbooks

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// ExportKinds are the publisher export formats Pressbooks can serve, in the
// order they are reported. Availability is per book: a live probe found pdf,
// print_pdf, epub, mobi, xhtml and wxr present on one book while odt and
// htmlbook answered 500 on the same book.
var ExportKinds = []string{"pdf", "print_pdf", "epub", "mobi", "xhtml", "htmlbook", "odt", "wxr"}

// IsExportKind reports whether kind is one of the publisher export formats
// this tool knows how to request, as opposed to an extracted-content kind
// such as "text" or "json".
func IsExportKind(kind string) bool {
	for _, k := range ExportKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ExportFormats reports which formats a book actually offers, probed
// concurrently with HEAD — bounded by perHostConcurrency (backend.go), the
// same per-host ceiling the catalog crawl obeys, rather than firing all of
// ExportKinds at once — so the answer costs no transferred bytes and a few
// round trips' worth of wall-clock time rather than one long one. A format
// the book offers answers 200; one it does not answers 500 — that is the
// whole availability signal for a healthy host, verified against a live
// book, and a bare non-200 status with no transport-level error is still
// treated as "not offered" rather than failing the call.
//
// A probe that errors outright (a transport failure, or Head's own 403 ->
// ErrBlocked) is different: it says nothing about whether the book offers
// that format, only that this probe could not ask. When every probe errors
// this way, the empty result is not a fact about the book — it is this
// call's failure to reach it — so the error is returned instead of an empty
// list. Returning "no formats, no error" here once meant downloadExport
// could report a rate-limited or blocked host as "this book offers: none"
// with retryable:false: a false claim about the book, and wrong advice for a
// purely transient condition.
func (c *Client) ExportFormats(ctx context.Context, ref BookRef) ([]string, error) {
	available := make([]bool, len(ExportKinds))
	probeErrs := make([]error, len(ExportKinds))
	sem := make(chan struct{}, perHostConcurrency)
	var wg sync.WaitGroup
	for i, kind := range ExportKinds {
		wg.Add(1)
		go func(index int, kind string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			status, err := c.Head(ctx, ref.ExportURL(kind))
			if err != nil {
				probeErrs[index] = err
				return
			}
			available[index] = status == http.StatusOK
		}(i, kind)
	}
	wg.Wait()

	out := make([]string, 0, len(ExportKinds))
	var lastErr error
	failed := 0
	for i, ok := range available {
		if ok {
			out = append(out, ExportKinds[i])
		}
		if probeErrs[i] != nil {
			failed++
			lastErr = probeErrs[i]
		}
	}
	if failed == len(ExportKinds) {
		return nil, fmt.Errorf("probing export formats for %s: %w", ref.URL, lastErr)
	}
	return out, nil
}
