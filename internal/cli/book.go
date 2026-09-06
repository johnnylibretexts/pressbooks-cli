package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
	"github.com/spf13/cobra"
)

// agentLicense is a license name and URL, compact enough to embed directly in
// an agentBookDetails response.
type agentLicense struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// agentBookDetails is info's agent response: enough to identify the book,
// license it, know how much of it there is to read, and know which
// publisher export formats it actually offers. ExportFormats is omitted
// entirely (nil, not an empty list) when --no-exports skipped the probe, so
// a caller can tell "not probed" from "probed, and this book offers none".
type agentBookDetails struct {
	URL           string       `json:"url"`
	Title         string       `json:"title"`
	Subtitle      string       `json:"subtitle,omitempty"`
	Authors       []string     `json:"authors,omitempty"`
	Host          string       `json:"host"`
	Language      string       `json:"language,omitempty"`
	CopyrightYear string       `json:"copyright_year,omitempty"`
	License       agentLicense `json:"license"`
	// WordCount is omitempty: the book metadata endpoint this command reads
	// genuinely omits wordCount (the network listing endpoint search and
	// books read carries it; this one does not), so a zero here would assert
	// "no words" as a fact about the book rather than admit the field was
	// never returned.
	WordCount     int      `json:"word_count,omitempty"`
	PageCount     int      `json:"page_count"`
	ExportFormats []string `json:"export_formats,omitempty"`
}

// agentPageSummary is one page in a toc response.
type agentPageSummary struct {
	ID        int    `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Slug      string `json:"slug"`
	URL       string `json:"url"`
	PartTitle string `json:"part_title,omitempty"`
	WordCount int    `json:"word_count"`
}

func summarizeBookDetails(book pressbooks.Book, ref pressbooks.BookRef, pageCount int, exportFormats []string) agentBookDetails {
	return agentBookDetails{
		URL:           ref.URL,
		Title:         book.Title,
		Subtitle:      book.Subtitle,
		Authors:       book.Authors,
		Host:          ref.Host,
		Language:      book.Language,
		CopyrightYear: book.CopyrightYear,
		License:       agentLicense{Name: book.LicenseName, URL: book.LicenseURL},
		WordCount:     book.WordCount,
		PageCount:     pageCount,
		ExportFormats: exportFormats,
	}
}

func summarizePages(pages []pressbooks.Page) []agentPageSummary {
	out := make([]agentPageSummary, 0, len(pages))
	for _, p := range pages {
		out = append(out, agentPageSummary{
			ID:        p.ID,
			Type:      p.Type,
			Title:     p.Title,
			Slug:      p.Slug,
			URL:       p.URL,
			PartTitle: p.PartTitle,
			WordCount: p.WordCount,
		})
	}
	return out
}

// resolveBookRef parses a book reference and reports a book_not_found-style
// error, rather than a raw parse error, when the reference is unusable —
// consistent with how a failed metadata or TOC fetch is classified below.
func resolveBookRef(raw string) (pressbooks.BookRef, error) {
	ref, err := pressbooks.ParseBookRef(raw)
	if err != nil {
		return pressbooks.BookRef{}, fmt.Errorf("book %q not found: %w", raw, err)
	}
	return ref, nil
}

// infoCmd shows one book's metadata, license, page count and available
// export formats. --no-exports skips the export probe (one round trip of
// eight concurrent HEAD requests) for a caller that only wants metadata and
// would rather not pay for it.
func infoCmd(f *flags) *cobra.Command {
	var noExports bool
	cmd := &cobra.Command{
		Use:   "info BOOK",
		Short: "Get one book's metadata, license, page count and export formats.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := resolveBookRef(args[0])
			if err != nil {
				return err
			}
			c, ctx := clientAndContext(f)
			book, err := c.BookMetadata(ctx, ref)
			if err != nil {
				return err
			}
			toc, err := c.TOC(ctx, ref)
			if err != nil {
				return err
			}
			var exportFormats []string
			if !noExports {
				exportFormats, err = c.ExportFormats(ctx, ref)
				if err != nil {
					return err
				}
			}
			pages := pressbooks.FlattenPages(toc, ref)
			details := summarizeBookDetails(book, ref, len(pages), exportFormats)
			if f.agent {
				return writeAgentSuccess(cmd.OutOrStdout(), "info", details, nil)
			}
			if f.asJSON {
				return pressbooks.WriteJSON(cmd.OutOrStdout(), details)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\nurl: %s\nauthors: %s\nlicense: %s (%s)\nlanguage: %s\npages: %d\nexports: %s\n",
				details.Title, details.URL, strings.Join(details.Authors, ", "),
				details.License.Name, details.License.URL, details.Language, details.PageCount,
				formatExportsLine(noExports, details.ExportFormats))
			return err
		},
	}
	cmd.Flags().BoolVar(&noExports, "no-exports", false, "Skip probing which publisher export formats this book offers")
	return cmd
}

// formatExportsLine renders info's plain-text "exports:" line: "not probed"
// when --no-exports skipped the check, "none" when the probe found nothing,
// or the comma-joined list of formats the book actually offers.
func formatExportsLine(noExports bool, formats []string) string {
	if noExports {
		return "not probed"
	}
	if len(formats) == 0 {
		return "none"
	}
	return strings.Join(formats, ", ")
}

// tocCmd lists a book's extractable pages, in reading order.
func tocCmd(f *flags) *cobra.Command {
	var flat bool
	var limit int
	var offset int
	cmd := &cobra.Command{
		Use:   "toc BOOK",
		Short: "List a book's extractable pages.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.agent {
				if _, err := agentWindowBounds(offset, limit, 20, 100); err != nil {
					return err
				}
			}
			ref, err := resolveBookRef(args[0])
			if err != nil {
				return err
			}
			c, ctx := clientAndContext(f)
			toc, err := c.TOC(ctx, ref)
			if err != nil {
				return err
			}
			pages := pressbooks.FlattenPages(toc, ref)
			if f.agent {
				start, end, meta, err := agentWindow(len(pages), offset, limit, 20, 100)
				if err != nil {
					return err
				}
				return writeAgentSuccess(cmd.OutOrStdout(), "toc", summarizePages(pages[start:end]), &meta)
			}
			if f.asJSON {
				return pressbooks.WriteJSON(cmd.OutOrStdout(), pages)
			}
			if flat {
				return writeTOCFlat(cmd.OutOrStdout(), pages)
			}
			return writeTOCHierarchy(cmd.OutOrStdout(), toc)
		},
	}
	cmd.Flags().BoolVar(&flat, "flat", false, "Print one line per extractable page (id, type, slug, title)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Agent page limit (default 20, maximum 100)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Agent page offset")
	return cmd
}

// writeTOCFlat prints exactly the extractable pages FlattenPages returns,
// one per line: id, type, slug, title, and, for a chapter, the part it
// belongs to. This is what --flat asks for.
func writeTOCFlat(w io.Writer, pages []pressbooks.Page) error {
	for _, p := range pages {
		if p.PartTitle == "" {
			if _, err := fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", p.ID, p.Type, p.Slug, p.Title); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\t%s\t(part: %s)\n", p.ID, p.Type, p.Slug, p.Title, p.PartTitle); err != nil {
			return err
		}
	}
	return nil
}

// sortedParts returns toc.Parts in MenuOrder, the same ordering FlattenPages
// applies, without mutating the caller's slice.
func sortedParts(toc pressbooks.TOC) []pressbooks.TOCPart {
	parts := make([]pressbooks.TOCPart, len(toc.Parts))
	copy(parts, toc.Parts)
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].MenuOrder < parts[j].MenuOrder })
	return parts
}

// writeTOCHierarchy prints the book's actual structure — front matter, then
// each part with its chapters indented beneath it, then back matter —
// rather than the flattened, extractable-only list --flat prints. This is
// the only way a container part (one with chapters but no content of its
// own) is ever visible in toc's output: FlattenPages drops it entirely,
// which is correct for extraction but would hide the book's organization
// from a human reading the default output. Every node is printed regardless
// of HasPostContent or Status; a non-extractable node (a container part, a
// draft, a generated back-matter entry) is printed the same as any other,
// since telling the two apart is exactly what this view is for.
func writeTOCHierarchy(w io.Writer, toc pressbooks.TOC) error {
	line := func(id int, kind, slug, title string) error {
		_, err := fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", id, kind, slug, title)
		return err
	}
	indented := func(id int, kind, slug, title string) error {
		_, err := fmt.Fprintf(w, "\t%d\t%s\t%s\t%s\n", id, kind, slug, title)
		return err
	}

	for _, item := range toc.FrontMatter {
		if err := line(item.ID, "front-matter", item.Slug, item.Title); err != nil {
			return err
		}
	}
	for _, part := range sortedParts(toc) {
		if err := line(part.ID, "part", part.Slug, part.Title); err != nil {
			return err
		}
		for _, chapter := range part.Chapters {
			if err := indented(chapter.ID, "chapter", chapter.Slug, chapter.Title); err != nil {
				return err
			}
		}
	}
	for _, item := range toc.BackMatter {
		if err := line(item.ID, "back-matter", item.Slug, item.Title); err != nil {
			return err
		}
	}
	return nil
}
