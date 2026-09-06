package pressbooks

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// A format a book offers answers 200; one it does not answers 500. That is the
// whole availability signal, so it is probed with HEAD rather than by
// downloading eight files to find out.
func TestExportFormatsProbesWithHead(t *testing.T) {
	offered := map[string]bool{"pdf": true, "epub": true, "xhtml": true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		if !offered[r.URL.Query().Get("type")] {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	parsed, _ := url.Parse(server.URL)
	client := New(0)
	client.baseScheme = "http"
	ref, err := ParseBookRef("http://" + parsed.Host + "/a-book/")
	if err != nil {
		t.Fatal(err)
	}
	ref.URL = server.URL + "/a-book/"

	got, err := client.ExportFormats(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "pdf" || got[1] != "print_pdf" && got[1] != "epub" {
		t.Fatalf("export formats = %#v", got)
	}
	// Order follows ExportKinds, so output is stable between runs.
	for i := 1; i < len(got); i++ {
		if indexOfKind(got[i-1]) >= indexOfKind(got[i]) {
			t.Fatalf("formats are not in canonical order: %#v", got)
		}
	}
}

func indexOfKind(kind string) int {
	for i, k := range ExportKinds {
		if k == kind {
			return i
		}
	}
	return -1
}

// TestExportFormatsReportsNoneWithoutError covers a book that offers nothing:
// every probe answers 500, so ExportFormats must return an empty (not nil,
// not erroring) slice rather than surfacing the last probe's failure.
func TestExportFormatsReportsNoneWithoutError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	ref, err := ParseBookRef(server.URL + "/a-book/")
	if err != nil {
		t.Fatal(err)
	}

	got, err := client.ExportFormats(context.Background(), ref)
	if err != nil {
		t.Fatalf("ExportFormats: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("export formats = %#v, want none", got)
	}
}

// TestExportFormatsReturnsErrorWhenEveryProbeIsBlocked pins the Important-5
// fix: on a rate-limited or blocked host, every HEAD probe answers 403, and
// before this fix that was folded into "not offered" the same as a genuine
// 500 — ExportFormats returned an empty list with a nil error. That let
// downloadExport report export_unavailable with retryable:false and "This
// book offers: none," a false claim about the book and wrong advice for a
// purely transient condition. Now every probe erroring (Head's 403 ->
// ErrBlocked) must surface as an error instead of an empty success.
func TestExportFormatsReturnsErrorWhenEveryProbeIsBlocked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	ref, err := ParseBookRef(server.URL + "/a-book/")
	if err != nil {
		t.Fatal(err)
	}

	got, err := client.ExportFormats(context.Background(), ref)
	if err == nil {
		t.Fatalf("expected an error when every probe is blocked, got formats %#v", got)
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("error = %v, want it to wrap ErrBlocked", err)
	}
}

// A single blocked probe among otherwise-answering ones must not fail the
// whole call: the other formats' 200/500 answers are still real information,
// so failed < len(ExportKinds) should still return a partial-but-honest list.
func TestExportFormatsToleratesOneBlockedProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "pdf" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("type") == "epub" {
			return // 200
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	ref, err := ParseBookRef(server.URL + "/a-book/")
	if err != nil {
		t.Fatal(err)
	}

	got, err := client.ExportFormats(context.Background(), ref)
	if err != nil {
		t.Fatalf("a single blocked probe among others should not fail the call: %v", err)
	}
	if len(got) != 1 || got[0] != "epub" {
		t.Fatalf("export formats = %#v, want [epub]", got)
	}
}

// TestIsExportKind covers both membership directions: a kind ExportKinds
// actually lists, and one it does not.
func TestIsExportKind(t *testing.T) {
	if !IsExportKind("pdf") {
		t.Error("IsExportKind(pdf) = false, want true")
	}
	if IsExportKind("docx") {
		t.Error("IsExportKind(docx) = true, want false")
	}
}

// TestExpectsHTMLExportMatchesExportKinds ties expectsHTMLExport's
// hand-built exempt set to the canonical ExportKinds list, so the two
// cannot drift apart silently. expectsHTMLExport's own comment says a kind
// missing from its exempt set fails closed — a real export is reported as a
// blocked host instead of downloading — so every kind ExportKinds lists
// must have an explicit, reviewed answer here. wantHTMLShaped's length is
// checked against ExportKinds' length first: a kind added to ExportKinds
// without a corresponding entry here fails loudly, rather than silently
// defaulting to "not HTML-shaped".
func TestExpectsHTMLExportMatchesExportKinds(t *testing.T) {
	wantHTMLShaped := map[string]bool{
		"pdf":       false,
		"print_pdf": false,
		"epub":      false,
		"mobi":      false,
		"xhtml":     true,
		"htmlbook":  true,
		"odt":       false,
		"wxr":       false,
	}
	if len(wantHTMLShaped) != len(ExportKinds) {
		t.Fatalf("wantHTMLShaped has %d entries but ExportKinds has %d; update both together so a new export kind is always considered", len(wantHTMLShaped), len(ExportKinds))
	}

	ref := BookRef{URL: "https://books.test/a-book/"}
	for _, kind := range ExportKinds {
		want, ok := wantHTMLShaped[kind]
		if !ok {
			t.Fatalf("no expectation recorded for export kind %q; add one to wantHTMLShaped", kind)
		}
		if got := expectsHTMLExport(ref.ExportURL(kind)); got != want {
			t.Errorf("expectsHTMLExport(%q export) = %v, want %v", kind, got, want)
		}
	}
}
