package pressbooks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// wrapBookNotFound turns a plain 404 (one that reached neither ErrNoAPI's
// "rest_no_route" check nor ErrBlocked's challenge-page check) into
// ErrBookNotFound. A book's own API endpoints 404 this way when its slug
// does not exist on the host at all, and the host is then free to answer
// with whatever 404 page it likes — including a full HTML page, and
// including one whose own wording happens to say "page not found" for what
// is actually a missing book. Matching on the status code here, rather than
// on that wording, is what keeps this classification from depending on a
// remote host's copy.
func wrapBookNotFound(err error, ref BookRef) error {
	var statusErr StatusError
	if errors.As(err, &statusErr) && statusErr.Status == http.StatusNotFound {
		return fmt.Errorf("book %s: %w", ref.URL, ErrBookNotFound)
	}
	return err
}

// BookMetadata fetches a book's schema.org-shaped metadata, the same wire
// shape the catalog decodes each entry from, but read directly from the
// book's own {APIBase}/metadata endpoint rather than a network listing page.
func (c *Client) BookMetadata(ctx context.Context, ref BookRef) (Book, error) {
	var wire wireMetadata
	if _, err := c.GetJSON(ctx, ref.APIBase()+"/metadata", &wire); err != nil {
		return Book{}, wrapBookNotFound(err, ref)
	}
	wrapped := wireBook{Link: ref.URL, Metadata: wire}
	return wrapped.book(ref.Host), nil
}

// TOC fetches a book's table of contents: front matter and back matter as
// flat lists, and parts each carrying their own chapters. Titles are cleaned
// through CleanHTMLTitle, not decodeProse: a TOC title can carry real markup
// (WordPress allows emphasis in a post title), and this is the one and only
// place a page or part title is decoded — every TOCItem.Title a caller sees
// from here on, including the Page.Title ExtractPage later receives, is
// already both entity-decoded and markup-stripped, so nothing downstream may
// decode it again.
func (c *Client) TOC(ctx context.Context, ref BookRef) (TOC, error) {
	var toc TOC
	if _, err := c.GetJSON(ctx, ref.APIBase()+"/toc", &toc); err != nil {
		return TOC{}, wrapBookNotFound(err, ref)
	}
	cleanTOCTitles(toc.FrontMatter)
	cleanTOCTitles(toc.BackMatter)
	for i := range toc.Parts {
		toc.Parts[i].Title = CleanHTMLTitle(toc.Parts[i].Title)
		cleanTOCTitles(toc.Parts[i].Chapters)
	}
	return toc, nil
}

func cleanTOCTitles(items []TOCItem) {
	for i := range items {
		items[i].Title = CleanHTMLTitle(items[i].Title)
	}
}

// isExtractable reports whether a TOC node has real content to fetch. A node
// is extractable when it has post content and its status is either publish
// or web-only (published to the web export but withheld from others); a
// container part with no content of its own, a generated back-matter entry
// such as a glossary, and a draft are all excluded.
func isExtractable(hasPostContent bool, status string) bool {
	if !hasPostContent {
		return false
	}
	return status == "publish" || status == "web-only"
}

func pageFromItem(item TOCItem, pageType, partTitle string) Page {
	return Page{
		ID:        item.ID,
		Type:      pageType,
		Title:     item.Title,
		Slug:      item.Slug,
		URL:       item.Link,
		PartTitle: partTitle,
		WordCount: item.WordCount,
	}
}

// FlattenPages walks a TOC in reading order and returns only its extractable
// nodes. Reading order is front matter, then parts in MenuOrder with each
// part's own page (when it has one) before its chapters, then back matter. A
// part that carries its own content is itself a page, of type "parts"; its
// chapters, if any, are still walked and typed "chapters" regardless of
// whether the part itself qualified.
//
// ref is unused today: every TOCItem.Link the API returns is already a
// complete, absolute URL (the same shape the catalog crawl consumes in
// wireBook.book), so there is no relative link to resolve against the book's
// own URL. It stays part of the signature — matching what a caller building
// against this book's identity would expect to pass — so a future host that
// omits Link does not require a breaking signature change to fix.
func FlattenPages(toc TOC, ref BookRef) []Page {
	_ = ref
	var pages []Page

	for _, item := range toc.FrontMatter {
		if isExtractable(item.HasPostContent, item.Status) {
			pages = append(pages, pageFromItem(item, "front-matter", ""))
		}
	}

	parts := make([]TOCPart, len(toc.Parts))
	copy(parts, toc.Parts)
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].MenuOrder < parts[j].MenuOrder })

	for _, part := range parts {
		if isExtractable(part.HasPostContent, part.Status) {
			pages = append(pages, pageFromItem(TOCItem{
				ID: part.ID, Title: part.Title, Slug: part.Slug, Status: part.Status,
				HasPostContent: part.HasPostContent, WordCount: part.WordCount, Link: part.Link,
			}, "parts", ""))
		}
		for _, chapter := range part.Chapters {
			if isExtractable(chapter.HasPostContent, chapter.Status) {
				pages = append(pages, pageFromItem(chapter, "chapters", part.Title))
			}
		}
	}

	for _, item := range toc.BackMatter {
		if isExtractable(item.HasPostContent, item.Status) {
			pages = append(pages, pageFromItem(item, "back-matter", ""))
		}
	}

	return pages
}

// FindPage resolves a page reference against an already-flattened page list.
// ref may be a numeric post id ("5"), a bare slug ("chapter-1"), a
// type-prefixed path ("chapter/chapter-1", matching Pressbooks' REST
// collection names even though Page.Type stores the plural "chapters"), or a
// full page URL. A miss is reported via the second return rather than
// silently falling back to the first page, so a caller can tell "no such
// page" from "here is page one".
func FindPage(pages []Page, ref string) (Page, bool) {
	raw := strings.TrimSpace(ref)
	if raw == "" {
		return Page{}, false
	}

	if id, err := strconv.Atoi(raw); err == nil && id > 0 {
		for _, p := range pages {
			if p.ID == id {
				return p, true
			}
		}
		return Page{}, false
	}

	for _, p := range pages {
		if p.URL == raw {
			return p, true
		}
	}

	slug := raw
	if idx := strings.LastIndex(raw, "/"); idx >= 0 {
		slug = raw[idx+1:]
	}
	slug = strings.ToLower(slug)
	for _, p := range pages {
		if strings.ToLower(p.Slug) == slug {
			return p, true
		}
	}

	return Page{}, false
}
