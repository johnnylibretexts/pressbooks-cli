package pressbooks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bookServer serves a book's /metadata and /toc endpoints at the paths
// BookRef.APIBase() builds, with entity-encoded titles the way WordPress
// actually stores them, so decoding is exercised the same way the catalog
// crawl's is.
func bookServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/wp-json/pressbooks/v2/metadata":
			_, _ = fmt.Fprint(w, `{
				"name":"Polymer Science &amp; Engineering","alternativeHeadline":"An Introduction",
				"inLanguage":"en","copyrightYear":2021,"wordCount":50000,"image":"https://books.test/cover.jpg",
				"author":[{"name":"J. Smith"}],
				"license":{"url":"https://creativecommons.org/licenses/by/4.0/","name":"CC BY (Attribution)"},
				"network":{"host":"books.test","name":"Books Test Network"}}`)
		case "/wp-json/pressbooks/v2/toc":
			_, _ = fmt.Fprint(w, `{
				"front-matter":[{"id":4,"title":"Introduction &amp; Scope","slug":"introduction","status":"publish","has_post_content":true,"link":"https://books.test/b/front-matter/introduction/"}],
				"parts":[{"id":279,"title":"Main Body","slug":"main-body","status":"publish","menu_order":1,"has_post_content":false,"link":"https://books.test/b/part/main-body/","chapters":[{"id":5,"title":"Chapter One","slug":"chapter-1","status":"publish","has_post_content":true,"link":"https://books.test/b/chapter/chapter-1/"}]}],
				"back-matter":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"code":"rest_no_route"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestBookMetadataDecodesEntityEncodedTitles(t *testing.T) {
	server := bookServer(t)
	client := New(0)
	client.baseScheme = "http"
	ref := BookRef{Host: hostOf(t, server), URL: server.URL + "/"}

	book, err := client.BookMetadata(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if book.Title != "Polymer Science & Engineering" {
		t.Fatalf("title was not entity-decoded: %q", book.Title)
	}
	if book.LicenseName != "CC BY (Attribution)" {
		t.Fatalf("license name = %q", book.LicenseName)
	}
}

func TestTOCDecodesEntityEncodedTitles(t *testing.T) {
	server := bookServer(t)
	client := New(0)
	client.baseScheme = "http"
	ref := BookRef{Host: hostOf(t, server), URL: server.URL + "/"}

	toc, err := client.TOC(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if toc.FrontMatter[0].Title != "Introduction & Scope" {
		t.Fatalf("front matter title was not entity-decoded: %q", toc.FrontMatter[0].Title)
	}
	pages := FlattenPages(toc, ref)
	wantIDs := []int{4, 5}
	if len(pages) != len(wantIDs) {
		t.Fatalf("got %d pages, want %d: %#v", len(pages), len(wantIDs), pages)
	}
	for i, want := range wantIDs {
		if pages[i].ID != want {
			t.Fatalf("page %d = id %d, want %d", i, pages[i].ID, want)
		}
	}
}

// A title's own display text can itself contain entity-like syntax: a
// chapter literally titled "Rock &amp; Roll" is stored by WordPress, and
// served by the API, as "Rock &amp;amp; Roll". CleanHTMLTitle must be the
// only place a title is ever decoded — TOC routes titles through it once,
// and anything downstream (ExtractPage's Page.Title) must carry that
// decoded value through unchanged rather than decoding it a second time,
// or the "amp;" that was actually part of the display text is silently
// eaten.
func TestTOCDecodesATitleExactlyOnceEvenWhenItsOwnTextLooksLikeAnEntity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/wp-json/pressbooks/v2/toc":
			_, _ = fmt.Fprint(w, `{
				"front-matter":[{"id":4,"title":"Rock &amp;amp; Roll","slug":"intro","status":"publish","has_post_content":true,"link":"https://books.test/b/front-matter/intro/"}],
				"parts":[],
				"back-matter":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"code":"rest_no_route"}`)
		}
	}))
	t.Cleanup(server.Close)
	client := New(0)
	client.baseScheme = "http"
	ref := BookRef{Host: hostOf(t, server), URL: server.URL + "/"}

	toc, err := client.TOC(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	got := toc.FrontMatter[0].Title
	const want = "Rock &amp; Roll"
	if got != want {
		t.Fatalf("TOC title = %q, want %q (decoded exactly once)", got, want)
	}

	// The same title, carried through as Page.Title, must reach ExtractPage
	// unchanged: a second decode there is exactly the bug this test guards
	// against.
	page := Page{ID: 4, Type: "front-matter", Title: got, URL: "https://books.test/b/front-matter/intro/"}
	extracted, err := ExtractPage(Book{}, page, page.URL, []byte("<p>hi</p>"), false)
	if err != nil {
		t.Fatal(err)
	}
	if extracted.PageTitle != want {
		t.Fatalf("ExtractPage re-decoded the title: got %q, want %q", extracted.PageTitle, want)
	}
}

// A title can carry real markup — WordPress allows emphasis in a post
// title — and CleanHTMLTitle must strip it, not let it leak into extracted
// text (or a saved title field) as literal angle brackets.
func TestTOCStripsMarkupFromATitle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/wp-json/pressbooks/v2/toc":
			_, _ = fmt.Fprint(w, `{
				"front-matter":[{"id":4,"title":"<em>Chapter</em> One","slug":"intro","status":"publish","has_post_content":true,"link":"https://books.test/b/front-matter/intro/"}],
				"parts":[],
				"back-matter":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"code":"rest_no_route"}`)
		}
	}))
	t.Cleanup(server.Close)
	client := New(0)
	client.baseScheme = "http"
	ref := BookRef{Host: hostOf(t, server), URL: server.URL + "/"}

	toc, err := client.TOC(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	got := toc.FrontMatter[0].Title
	if got != "Chapter One" {
		t.Fatalf("title = %q, want markup stripped to %q", got, "Chapter One")
	}
	if strings.ContainsAny(got, "<>") {
		t.Fatalf("markup leaked into the title: %q", got)
	}
}

func sampleTOC() TOC {
	return TOC{
		FrontMatter: []TOCItem{
			{ID: 4, Title: "Introduction", Slug: "introduction", Status: "publish", HasPostContent: true,
				Link: "https://books.test/b/front-matter/introduction/"},
		},
		Parts: []TOCPart{
			{ID: 279, Title: "Main Body", Slug: "main-body", Status: "publish", HasPostContent: false,
				Chapters: []TOCItem{
					{ID: 5, Title: "Chapter One", Slug: "chapter-1", Status: "publish", HasPostContent: true,
						Link: "https://books.test/b/chapter/chapter-1/"},
					{ID: 6, Title: "A Draft", Slug: "draft", Status: "draft", HasPostContent: true,
						Link: "https://books.test/b/chapter/draft/"},
					{ID: 7, Title: "An Empty Stub", Slug: "stub", Status: "publish", HasPostContent: false,
						Link: "https://books.test/b/chapter/stub/"},
				}},
			// A part that carries its own content is a page in its own right.
			{ID: 288, Title: "Featured Ecologists", Slug: "featured", Status: "publish", HasPostContent: true,
				Link: "https://books.test/b/part/featured/"},
		},
		BackMatter: []TOCItem{
			{ID: 90, Title: "Resources", Slug: "resources", Status: "publish", HasPostContent: true,
				Link: "https://books.test/b/back-matter/resources/"},
			// Glossary and Contributors are generated, with no post content.
			{ID: 91, Title: "Glossary", Slug: "glossary", Status: "publish", HasPostContent: false,
				Link: "https://books.test/b/back-matter/glossary/"},
		},
	}
}

func TestFlattenPagesKeepsReadingOrderAndSkipsEmptyNodes(t *testing.T) {
	ref, err := ParseBookRef("https://books.test/b/")
	if err != nil {
		t.Fatal(err)
	}
	pages := FlattenPages(sampleTOC(), ref)

	wantIDs := []int{4, 5, 288, 90}
	if len(pages) != len(wantIDs) {
		t.Fatalf("got %d pages, want %d: %#v", len(pages), len(wantIDs), pages)
	}
	for i, want := range wantIDs {
		if pages[i].ID != want {
			t.Fatalf("page %d = id %d, want %d", i, pages[i].ID, want)
		}
	}
	wantTypes := []string{"front-matter", "chapters", "parts", "back-matter"}
	for i, want := range wantTypes {
		if pages[i].Type != want {
			t.Fatalf("page %d type = %q, want %q", i, pages[i].Type, want)
		}
	}
	if pages[1].PartTitle != "Main Body" {
		t.Fatalf("a chapter should carry its part: %#v", pages[1])
	}
}

// A web-only page is extractable even though it is not "publish": Pressbooks
// uses that status for content published to the web export but withheld from
// other exports, and it still has real post content worth reading.
func TestFlattenPagesKeepsWebOnlyStatus(t *testing.T) {
	ref, _ := ParseBookRef("https://books.test/b/")
	toc := TOC{
		FrontMatter: []TOCItem{
			{ID: 10, Title: "Preface", Slug: "preface", Status: "web-only", HasPostContent: true,
				Link: "https://books.test/b/front-matter/preface/"},
		},
	}
	pages := FlattenPages(toc, ref)
	if len(pages) != 1 || pages[0].ID != 10 {
		t.Fatalf("web-only page with post content should be extractable: %#v", pages)
	}
}

// Reading order is front matter, then parts by MenuOrder with each part's own
// page before its chapters, then back matter. A test that only checked set
// membership would pass even if a later change scrambled the order, so this
// asserts the exact sequence against parts given out of MenuOrder order.
func TestFlattenPagesOrdersPartsByMenuOrder(t *testing.T) {
	ref, _ := ParseBookRef("https://books.test/b/")
	toc := TOC{
		Parts: []TOCPart{
			{ID: 2, Title: "Second Part", Slug: "second", Status: "publish", HasPostContent: true, MenuOrder: 20,
				Link: "https://books.test/b/part/second/"},
			{ID: 1, Title: "First Part", Slug: "first", Status: "publish", HasPostContent: true, MenuOrder: 10,
				Link: "https://books.test/b/part/first/"},
		},
	}
	pages := FlattenPages(toc, ref)
	wantIDs := []int{1, 2}
	if len(pages) != len(wantIDs) {
		t.Fatalf("got %d pages, want %d: %#v", len(pages), len(wantIDs), pages)
	}
	for i, want := range wantIDs {
		if pages[i].ID != want {
			t.Fatalf("page %d = id %d, want %d (order not sorted by MenuOrder): %#v", i, pages[i].ID, want, pages)
		}
	}
}

func TestFindPageAcceptsIDsAndSlugs(t *testing.T) {
	ref, _ := ParseBookRef("https://books.test/b/")
	pages := FlattenPages(sampleTOC(), ref)

	for _, in := range []string{"5", "chapter-1", "chapter/chapter-1", "https://books.test/b/chapter/chapter-1/"} {
		got, ok := FindPage(pages, in)
		if !ok || got.ID != 5 {
			t.Errorf("FindPage(%q) = (%#v, %v)", in, got, ok)
		}
	}
	if _, ok := FindPage(pages, "no-such-page"); ok {
		t.Error("FindPage matched a page that does not exist")
	}
}

// A host serving a missing book answers 404 with whatever error page it
// likes — including, on at least one real host, an HTML page whose own
// <title> says "Page not found" for what is actually a missing *book*. Both
// BookMetadata and TOC must classify that as ErrBookNotFound from the status
// code alone, not from the body's wording, or a caller (classifyAgentError
// in the cli package) ends up reporting the wrong kind of "not found".
func TestBookMetadataAndTOCReport404AsBookNotFoundRegardlessOfBodyWording(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `<!doctype html><html><head><title>Page not found</title></head>`+
			`<body><h1>Page not found</h1><script>/* analytics */</script></body></html>`)
	}))
	t.Cleanup(server.Close)
	client := New(0)
	client.baseScheme = "http"
	ref := BookRef{Host: hostOf(t, server), URL: server.URL + "/"}

	if _, err := client.BookMetadata(context.Background(), ref); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("BookMetadata error = %v, want ErrBookNotFound", err)
	}
	if _, err := client.TOC(context.Background(), ref); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("TOC error = %v, want ErrBookNotFound", err)
	}
}
