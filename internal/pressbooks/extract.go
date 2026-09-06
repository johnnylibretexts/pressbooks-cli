package pressbooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	xhtml "golang.org/x/net/html"
)

// latexAlt recognizes a LaTeX-looking alt attribute. Pressbooks renders
// equations to images through QuickLaTeX and keeps the source in the alt
// attribute, so an extractor that ignores images silently drops every
// equation in the book.
var latexAlt = regexp.MustCompile(`\\[a-zA-Z]+|[_^]\{|\\\(|\\\[|\$\$`)

func isEquationImage(alt string) bool {
	return alt != "" && latexAlt.MatchString(alt)
}

// wrapPageNotFound turns a plain 404 into ErrPageNotFound, the same way
// wrapBookNotFound in book.go turns one into ErrBookNotFound: matching on the
// status code rather than the body's wording is what keeps this
// classification from depending on a remote host's own error-page copy.
func wrapPageNotFound(err error, ref BookRef, page Page) error {
	var statusErr StatusError
	if errors.As(err, &statusErr) && statusErr.Status == http.StatusNotFound {
		return fmt.Errorf("page %d in book %s: %w", page.ID, ref.URL, ErrPageNotFound)
	}
	return err
}

// wirePageContent is the shape of one page's own REST resource: only the two
// fields this task needs out of it.
type wirePageContent struct {
	Link  string `json:"link"`
	Title struct {
		Rendered string `json:"rendered"`
	} `json:"title"`
	Content struct {
		Rendered string `json:"rendered"`
	} `json:"content"`
}

// PageContent fetches a page's own REST resource ({APIBase}/{page.Type}/{id})
// and returns its rendered HTML body along with the page's public URL, which
// ExtractPage needs to resolve relative resource URLs against. A 404 is
// wrapped in ErrPageNotFound rather than left as a bare StatusError, the same
// way BookMetadata and TOC wrap theirs in ErrBookNotFound.
func (c *Client) PageContent(ctx context.Context, ref BookRef, page Page) ([]byte, string, error) {
	var wire wirePageContent
	endpoint := fmt.Sprintf("%s/%s/%d", ref.APIBase(), page.Type, page.ID)
	if _, err := c.GetJSON(ctx, endpoint, &wire); err != nil {
		return nil, "", wrapPageNotFound(err, ref, page)
	}
	pageURL := wire.Link
	if pageURL == "" {
		pageURL = page.URL
	}
	return []byte(wire.Content.Rendered), pageURL, nil
}

// ExtractPage walks a page's rendered HTML and turns it into readable text,
// plus (optionally) the cleaned-up HTML and the list of ordinary images. The
// walker is openstax-cli's block-aware textOf, plus one rule this source
// needs: an equation image's alt text (its LaTeX source) is inlined into the
// text stream instead of being dropped, since Pressbooks renders every
// equation as an image and nothing else in the response carries the
// equation's content.
func ExtractPage(book Book, page Page, pageURL string, body []byte, includeHTML bool) (ExtractedPage, error) {
	doc, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return ExtractedPage{}, err
	}
	absolutizeResourceURLs(doc, pageURL)
	var htmlBody string
	if includeHTML {
		var b bytes.Buffer
		_ = xhtml.Render(&b, doc)
		htmlBody = b.String()
	}
	return ExtractedPage{
		BookURL:   book.URL,
		BookTitle: book.Title,
		PageID:    page.ID,
		PageType:  page.Type,
		// page.Title already passed through CleanHTMLTitle once, in TOC —
		// cleaning it again here would decode a title whose display text
		// itself contains entity-like syntax a second time.
		PageTitle: page.Title,
		PageSlug:  page.Slug,
		SourceURL: pageURL,
		Text:      NormalizeText(textOf(doc)),
		HTML:      htmlBody,
		Images:    collectImages(doc),
	}, nil
}

// absolutizeResourceURLs rewrites every src, href and poster attribute in the
// tree to an absolute URL, resolved against pageURL. Saved JSON and HTML must
// still be able to reach a page's images after the fact, so a relative URL
// that only ever resolved correctly in the context of the original page is
// not good enough.
func absolutizeResourceURLs(n *xhtml.Node, pageURL string) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return
	}
	var walk func(*xhtml.Node)
	walk = func(cur *xhtml.Node) {
		if cur.Type == xhtml.ElementNode {
			for i := range cur.Attr {
				a := &cur.Attr[i]
				if a.Key != "src" && a.Key != "href" && a.Key != "poster" {
					continue
				}
				ref, err := url.Parse(a.Val)
				if err == nil {
					a.Val = base.ResolveReference(ref).String()
				}
			}
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
}

// imgAlt reads an img element's alt attribute.
func imgAlt(n *xhtml.Node) string {
	for _, a := range n.Attr {
		if a.Key == "alt" {
			return a.Val
		}
	}
	return ""
}

// textOf walks the tree collecting readable text: script, style and svg
// content is never emitted, block-level elements are separated by
// newlines so prose does not run together, and an equation image's alt text
// (its LaTeX source) is written into the stream in place of the image it
// would otherwise silently drop. An ordinary image contributes nothing here
// — it is collected separately by collectImages instead.
func textOf(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(cur *xhtml.Node) {
		if cur.Type == xhtml.ElementNode {
			switch cur.Data {
			case "script", "style", "svg":
				return
			case "img":
				if alt := imgAlt(cur); isEquationImage(alt) {
					b.WriteString(alt)
					b.WriteString(" ")
				}
			case "h1", "h2", "h3", "h4", "h5", "h6", "p", "div", "section", "table", "tr", "ul", "ol", "li", "blockquote", "figure":
				b.WriteString("\n")
			case "br":
				b.WriteString("\n")
			}
		}
		if cur.Type == xhtml.TextNode {
			b.WriteString(cur.Data)
			b.WriteString(" ")
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if cur.Type == xhtml.ElementNode {
			switch cur.Data {
			case "h1", "h2", "h3", "h4", "h5", "h6", "p", "div", "section", "table", "tr", "ul", "ol", "li", "blockquote", "figure":
				b.WriteString("\n")
			}
		}
	}
	walk(n)
	return b.String()
}

// NormalizeText collapses each line's whitespace (including HTML entities
// left over from a fallback path that did not go through the HTML parser,
// and non-breaking spaces WordPress content carries) down to single spaces,
// drops lines left empty by that collapse, and joins what remains with a
// blank line between paragraphs. The block-aware walker in textOf is what
// puts a newline at each block boundary in the first place — the two are a
// pair, and collapsing this down to a single line would throw away
// everything the walker contributed: at the scale this tool runs at (whole
// books extracted at once), paragraph structure is not decoration, it is
// most of what makes the extracted text usable at all.
func NormalizeText(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(html.UnescapeString(line)), " ")
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n\n")
}

// collectImages gathers every ordinary image in the tree, deduplicated by
// src, in document order. An equation image is deliberately excluded: its
// content already reached the text via textOf, and listing it too would
// duplicate the equation as if it were also a picture worth looking at.
func collectImages(n *xhtml.Node) []Image {
	seen := map[string]bool{}
	var images []Image
	var walk func(*xhtml.Node)
	walk = func(cur *xhtml.Node) {
		if cur.Type == xhtml.ElementNode && cur.Data == "img" {
			var img Image
			for _, a := range cur.Attr {
				switch a.Key {
				case "src":
					img.Src = a.Val
				case "alt":
					img.Alt = a.Val
				}
			}
			if !isEquationImage(img.Alt) && img.Src != "" && !seen[img.Src] {
				seen[img.Src] = true
				images = append(images, img)
			}
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return images
}

// CleanHTMLTitle strips markup from a title, decoding entities along the
// way. It is the one place a title is decoded: TOC and the catalog/metadata
// wire decoder both route their titles through this, once, at the wire
// boundary, rather than through decodeProse — a title can carry real markup
// (WordPress allows emphasis in a post title) and, unlike ordinary prose, a
// title's own display text can itself contain entity-like syntax (a title
// wire value of "Rock &amp;amp; Roll" is a book literally called
// "Rock &amp; Roll"), so decoding it twice would silently eat a character.
// Callers downstream of TOC, BookMetadata or Catalog must never run a
// title through this — or through decodeProse — a second time.
func CleanHTMLTitle(raw string) string {
	node, err := xhtml.Parse(strings.NewReader(raw))
	if err != nil {
		return strings.Join(strings.Fields(html.UnescapeString(raw)), " ")
	}
	return strings.Join(strings.Fields(textOf(node)), " ")
}
