//go:build integration

package pressbooks

import (
	"context"
	"strings"
	"testing"
	"time"
)

// milnepublishing.geneseo.edu is a bundled network that is not on the
// pressbooks.pub shared backend, so a live check here never contends with
// the rate limit that fifteen of the twenty-eight bundled networks share.
// It is also one of the smaller bundled networks (90 books at last probe),
// so a live check costs four requests rather than three hundred. These
// tests exist so an endpoint that moves or dies fails the build instead of
// shipping.
const liveHost = "milnepublishing.geneseo.edu"

// A known, stable book on liveHost used for TOC, page, and export checks.
const liveBookURL = "https://milnepublishing.geneseo.edu/concise-introduction-to-logic/"

func TestLiveCatalogCrawl(t *testing.T) {
	redirectCache(t)
	client := New(30 * time.Second)
	catalog, err := client.Catalog(context.Background(), liveHost, nil)
	if err != nil {
		t.Fatalf("crawl %s: %v", liveHost, err)
	}
	if len(catalog.Books) < 20 || !catalog.Complete() {
		t.Fatalf("got %d of %d books, complete=%v", len(catalog.Books), catalog.TotalBooks, catalog.Complete())
	}
	if catalog.Books[0].LicenseName == "" {
		t.Fatalf("live books should carry a license: %#v", catalog.Books[0])
	}
}

func TestLiveBookTOCAndPage(t *testing.T) {
	client := New(30 * time.Second)
	ref, err := ParseBookRef(liveBookURL)
	if err != nil {
		t.Fatal(err)
	}
	toc, err := client.TOC(context.Background(), ref)
	if err != nil {
		t.Fatalf("live TOC: %v", err)
	}
	pages := FlattenPages(toc, ref)
	if len(pages) < 5 {
		t.Fatalf("got %d extractable pages, want at least 5", len(pages))
	}

	book, err := client.BookMetadata(context.Background(), ref)
	if err != nil {
		t.Fatalf("live metadata: %v", err)
	}
	body, pageURL, err := client.PageContent(context.Background(), ref, pages[0])
	if err != nil {
		t.Fatalf("live page: %v", err)
	}
	got, err := ExtractPage(book, pages[0], pageURL, body, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(got.Text)) < 20 {
		t.Fatalf("extracted page looks empty: %q", got.Text)
	}
}

func TestLiveExportFormats(t *testing.T) {
	client := New(30 * time.Second)
	ref, _ := ParseBookRef(liveBookURL)
	formats, err := client.ExportFormats(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range formats {
		if f == "pdf" || f == "epub" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a live book should offer at least a PDF or EPUB, got %#v", formats)
	}
}
