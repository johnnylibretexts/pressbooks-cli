package pressbooks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// BookRef is a book's identity: its host, its slug within that host, and the
// canonical URL built from the two. Slug is empty for a book served at the host
// root, which is how a single-book install is published.
type BookRef struct {
	Host string
	Slug string
	URL  string
}

// reservedSegments are path segments Pressbooks and WordPress own. A URL whose
// first segment is one of these belongs to a root-hosted book, so the segment
// must never be read as a book slug.
//
// This is an accepted tradeoff of resolving identifiers lexically with no
// network request: a book whose actual slug is one of these words (an "open"
// or "format" or "catalog" book is plausible) is misread as a root-hosted
// install and loses its slug. The failure is loud rather than silent — the
// resolved root serves no book, so every request against it 404s from the
// Pressbooks API instead of quietly returning the wrong book's content.
var reservedSegments = map[string]bool{
	"chapter":      true,
	"chapters":     true,
	"front-matter": true,
	"back-matter":  true,
	"part":         true,
	"parts":        true,
	"glossary":     true,
	"open":         true,
	"format":       true,
	"wp-json":      true,
	"wp-admin":     true,
	"wp-content":   true,
	"catalog":      true,
	"feed":         true,
}

// ParseBookRef resolves any reference to a book into its canonical form. It is
// purely lexical: it makes no request, so info and toc work against any
// Pressbooks host rather than only the bundled ones.
func ParseBookRef(ref string) (BookRef, error) {
	raw := strings.TrimSpace(ref)
	if raw == "" {
		return BookRef{}, fmt.Errorf("empty book reference")
	}
	// A test server is not HTTPS, so an explicit http:// scheme in the input
	// is preserved rather than forced to https like every other case.
	scheme := "https"
	if strings.HasPrefix(raw, "http://") {
		scheme = "http"
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return BookRef{}, fmt.Errorf("book reference %q is not a URL: %w", ref, err)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || !strings.Contains(host, ".") || strings.ContainsAny(host, " ") {
		return BookRef{}, fmt.Errorf("book reference %q has no usable host", ref)
	}
	// Host stays a bare hostname: the bundled network list, the catalog
	// crawl, and the cache filename are all keyed on it. The port, when
	// present, belongs only in the reconstructed URL.
	authority := host
	if port := parsed.Port(); port != "" {
		authority += ":" + port
	}

	slug := ""
	for _, segment := range strings.Split(strings.Trim(parsed.Path, "/"), "/") {
		if segment == "" {
			continue
		}
		if !reservedSegments[strings.ToLower(segment)] {
			slug = segment
		}
		break
	}

	out := BookRef{Host: host, Slug: slug}
	if slug == "" {
		out.URL = scheme + "://" + authority + "/"
	} else {
		out.URL = scheme + "://" + authority + "/" + slug + "/"
	}
	return out, nil
}

// APIBase is the book's Pressbooks v2 API root.
func (r BookRef) APIBase() string { return r.URL + "wp-json/pressbooks/v2" }

// ExportURL is the address of one publisher export format.
func (r BookRef) ExportURL(kind string) string {
	return r.URL + "open/download?type=" + url.QueryEscape(kind)
}

// ParsePageRef reads a page reference as either a numeric post id or a slug.
// A zero id means the slug should be matched instead.
func ParsePageRef(ref string) (int, string) {
	raw := strings.TrimSpace(ref)
	if raw == "" {
		return 0, ""
	}
	if id, err := strconv.Atoi(raw); err == nil && id > 0 {
		return id, ""
	}
	if strings.Contains(raw, "://") {
		if parsed, err := url.Parse(raw); err == nil {
			raw = parsed.Path
		}
	}
	segments := strings.Split(strings.Trim(raw, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i] != "" {
			return 0, strings.ToLower(segments[i])
		}
	}
	return 0, ""
}

func (f *flexString) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == `""` {
		*f = ""
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

func (f flexString) String() string { return string(f) }

// WriteJSON writes v as indented JSON. This is the --json path; agent mode has
// its own compact writer.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Complete reports whether a catalog's crawl finished. SyncedPages and
// TotalPages make an interrupted crawl resumable: a catalog is complete only
// when its last page arrived, so a partial one can be reported as partial
// rather than passed off as a small complete network.
func (c Catalog) Complete() bool {
	return c.TotalPages > 0 && c.SyncedPages >= c.TotalPages
}
