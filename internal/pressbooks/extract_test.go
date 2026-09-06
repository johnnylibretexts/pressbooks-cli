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

const samplePageHTML = `<p><img loading="lazy" class="aligncenter" src="/app/uploads/sites/3/solar.jpg" alt="Illustration of the solar system" width="340" height="220" /></p>
<h3 style="text-align: justify">The Resource</h3>
<p style="text-align: justify"><em>Open Planets</em> is a textbook made up of five modules.</p>
<script>console.log("tracking")</script>
<p>Einstein wrote <img src="https://quicklatex.com/cache3/eq.png" alt="E = mc^{2}" class="latex" /> on a napkin.</p>
<table><tr><td>Mercury</td><td>0.39 AU</td></tr></table>`

func TestExtractPageProducesReadableText(t *testing.T) {
	book := Book{URL: "https://books.test/b/", Title: "A Book"}
	page := Page{ID: 5, Type: "chapters", Title: "Chapter One", Slug: "chapter-1", URL: "https://books.test/b/chapter/chapter-1/"}

	got, err := ExtractPage(book, page, page.URL, []byte(samplePageHTML), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "Open Planets is a textbook made up of five modules.") {
		t.Fatalf("prose missing: %q", got.Text)
	}
	if strings.Contains(got.Text, "tracking") {
		t.Fatalf("script content leaked into the text: %q", got.Text)
	}
	if !strings.Contains(got.Text, "Mercury") || !strings.Contains(got.Text, "0.39 AU") {
		t.Fatalf("table text was dropped: %q", got.Text)
	}
	if got.PageID != 5 || got.PageTitle != "Chapter One" || got.BookTitle != "A Book" {
		t.Fatalf("page identity lost: %#v", got)
	}
}

// Pressbooks renders equations to images and keeps the LaTeX in the alt text.
// Dropping the image drops the equation from the extracted text entirely.
func TestEquationImagesBecomeTheirLatexAltText(t *testing.T) {
	book := Book{URL: "https://books.test/b/"}
	page := Page{ID: 5, URL: "https://books.test/b/chapter/chapter-1/"}

	got, err := ExtractPage(book, page, page.URL, []byte(samplePageHTML), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "E = mc^{2}") {
		t.Fatalf("equation alt text missing from extracted text: %q", got.Text)
	}
	// An ordinary illustration is listed, not inlined as prose.
	if strings.Contains(got.Text, "Illustration of the solar system") {
		t.Fatalf("a non-equation alt was inlined: %q", got.Text)
	}
}

// Saved JSON and HTML must still be able to reach the images.
func TestExtractPageAbsolutizesResourceURLs(t *testing.T) {
	book := Book{URL: "https://books.test/b/"}
	page := Page{ID: 5, URL: "https://books.test/b/chapter/chapter-1/"}

	got, err := ExtractPage(book, page, page.URL, []byte(samplePageHTML), true)
	if err != nil {
		t.Fatal(err)
	}
	const want = "https://books.test/app/uploads/sites/3/solar.jpg"
	if len(got.Images) != 1 || got.Images[0].Src != want {
		t.Fatalf("images = %#v, want one absolute %q", got.Images, want)
	}
	if !strings.Contains(got.HTML, want) {
		t.Fatalf("HTML kept a relative URL: %s", got.HTML)
	}
}

// A missing page must be reported as ErrPageNotFound, not ErrBookNotFound —
// otherwise the sentinel task 8 added alongside ErrBookNotFound stays dead
// code and a missing page gets misclassified by the CLI's error classifier.
func TestPageContentFetchesRenderedBodyAndReportsErrPageNotFoundOn404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/wp-json/pressbooks/v2/chapters/5":
			_, _ = fmt.Fprint(w, `{"link":"https://books.test/b/chapter/chapter-1/",`+
				`"title":{"rendered":"Chapter One"},"content":{"rendered":"<p>Hello</p>"}}`)
		default:
			// A missing page 404s with a WordPress "no such post" body, not
			// the "rest_no_route" body a wholly unknown route answers with —
			// that distinction is what keeps this 404 from being classified
			// as ErrNoAPI before wrapPageNotFound ever sees it.
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"code":"rest_post_invalid_id","message":"Invalid post ID."}`)
		}
	}))
	t.Cleanup(server.Close)
	client := New(0)
	client.baseScheme = "http"
	ref := BookRef{Host: hostOf(t, server), URL: server.URL + "/"}

	body, pageURL, err := client.PageContent(context.Background(), ref, Page{ID: 5, Type: "chapters"})
	if err != nil {
		t.Fatal(err)
	}
	if pageURL != "https://books.test/b/chapter/chapter-1/" {
		t.Fatalf("pageURL = %q", pageURL)
	}
	if !strings.Contains(string(body), "Hello") {
		t.Fatalf("body = %q", body)
	}

	_, _, err = client.PageContent(context.Background(), ref, Page{ID: 99, Type: "chapters"})
	if !errors.Is(err, ErrPageNotFound) {
		t.Fatalf("PageContent error = %v, want ErrPageNotFound", err)
	}
	if errors.Is(err, ErrBookNotFound) {
		t.Fatalf("PageContent error = %v, must not also be ErrBookNotFound", err)
	}
}

func TestNormalizeTextCollapsesWhitespaceAndEntities(t *testing.T) {
	got := NormalizeText("  Hello   &amp;  \n\n\n  goodbye   \n")
	if got != "Hello &\n\ngoodbye" {
		t.Fatalf("NormalizeText = %q", got)
	}
}

// Paragraph breaks are not decoration: at the scale this tool runs at (whole
// books extracted at once), they are most of what makes the extracted text
// usable, whether by a person reading it or a retrieval pipeline chunking
// it. The block-aware walker in textOf is what puts a newline at each block
// boundary; NormalizeText must keep that structure, not flatten it away.
func TestNormalizeTextPreservesParagraphBreaks(t *testing.T) {
	got := NormalizeText("First paragraph.\n\nSecond paragraph.\n\n\nThird paragraph.")
	want := "First paragraph.\n\nSecond paragraph.\n\nThird paragraph."
	if got != want {
		t.Fatalf("NormalizeText = %q, want %q", got, want)
	}
}
