package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
	"github.com/spf13/cobra"
)

// extractDelayDefault is the default gap enforced between successive page
// fetches during whole-book extraction, matching design section 6's
// "--delay (default 100ms) paces requests between pages during whole-book
// extraction." It is applied by extractPages, so both `extract --all` and
// `download --kind text|json|html` get it for free — they share that one
// function precisely so a 104-page book is never fetched at full speed
// through either verb. `extract` also exposes it as the user-facing
// --delay flag; download does not (the design gives download no such flag
// of its own), so download always runs extractPages at this default.
//
// A var, not a const, for the same reason crawlPace and backendPace are:
// production wants the real pace, but a test driving a fake client is not
// the university server this exists to protect, and should not pay real
// wall-clock time for it.
var extractDelayDefault = 100 * time.Millisecond

// agentExtractedPage is extract's agent response for one page: bounded text
// plus the offsets a caller needs to page through the rest of it.
type agentExtractedPage struct {
	BookURL       string `json:"book_url"`
	PageID        int    `json:"page_id"`
	PageType      string `json:"page_type"`
	PageTitle     string `json:"page_title"`
	PageSlug      string `json:"page_slug"`
	SourceURL     string `json:"source_url"`
	Text          string `json:"text"`
	CharStart     int    `json:"char_start"`
	CharEnd       int    `json:"char_end"`
	TotalChars    int    `json:"total_chars"`
	Truncated     bool   `json:"truncated"`
	NextStartChar *int   `json:"next_start_char,omitempty"`
}

// agentExtractDirResult is extract's agent response when --dir was used: a
// directory write returns where it went and how much landed there, rather
// than the page text itself, which already lives on disk.
type agentExtractDirResult struct {
	Directory string `json:"directory"`
	PageCount int    `json:"page_count"`
	Bytes     int    `json:"bytes"`
}

// compactExtractedPage bounds one page's text to a [startChar, startChar+maxChars)
// window and reports where the next window would begin. The slicing is done
// on runes, never bytes: extracted textbook content routinely carries
// multi-byte characters (typographic quotes, dashes, accented names, math),
// and a byte offset that lands inside one of those would split it in half
// and hand back invalid UTF-8.
func compactExtractedPage(page pressbooks.ExtractedPage, startChar, maxChars int) (agentExtractedPage, error) {
	if startChar < 0 {
		return agentExtractedPage{}, &agentInputError{Code: "invalid_start_char", Message: "start-char must be zero or greater", Suggestion: "Use --start-char 0 for the beginning of the page."}
	}
	if maxChars < 1 {
		return agentExtractedPage{}, &agentInputError{Code: "invalid_max_chars", Message: "max-chars must be at least 1", Suggestion: "Omit --max-chars to use 12000."}
	}
	runes := []rune(page.Text)
	if startChar > len(runes) {
		return agentExtractedPage{}, &agentInputError{Code: "start_char_out_of_range", Message: fmt.Sprintf("start-char %d exceeds page length %d", startChar, len(runes)), Suggestion: "Use the total_chars value from the previous response."}
	}
	end := startChar + maxChars
	if end > len(runes) {
		end = len(runes)
	}
	result := agentExtractedPage{
		BookURL:    page.BookURL,
		PageID:     page.PageID,
		PageType:   page.PageType,
		PageTitle:  page.PageTitle,
		PageSlug:   page.PageSlug,
		SourceURL:  page.SourceURL,
		Text:       string(runes[startChar:end]),
		CharStart:  startChar,
		CharEnd:    end,
		TotalChars: len(runes),
		Truncated:  startChar > 0 || end < len(runes),
	}
	if end < len(runes) {
		next := end
		result.NextStartChar = &next
	}
	return result, nil
}

func compactExtractedPages(pages []pressbooks.ExtractedPage, startChar, maxChars int) ([]agentExtractedPage, error) {
	out := make([]agentExtractedPage, 0, len(pages))
	for _, page := range pages {
		compact, err := compactExtractedPage(page, startChar, maxChars)
		if err != nil {
			return nil, err
		}
		out = append(out, compact)
	}
	return out, nil
}

// extractPages fetches and extracts each page's content in order, failing on
// the first error rather than returning a partial book with a silent gap.
// delay is waited before every fetch after the first, pacing whole-book
// extraction the same way crawlPace paces a catalog crawl: without it, a
// 104-page book is 104 back-to-back requests at full speed against the same
// host that just answered the TOC and metadata calls above it.
func extractPages(ctx context.Context, c pressbooksClient, ref pressbooks.BookRef, book pressbooks.Book, pages []pressbooks.Page, includeHTML bool, delay time.Duration) ([]pressbooks.ExtractedPage, error) {
	var out []pressbooks.ExtractedPage
	for i, page := range pages {
		if i > 0 && delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		body, pageURL, err := c.PageContent(ctx, ref, page)
		if err != nil {
			return nil, err
		}
		extracted, err := pressbooks.ExtractPage(book, page, pageURL, body, includeHTML)
		if err != nil {
			return nil, err
		}
		out = append(out, extracted)
	}
	return out, nil
}

// extractFormatExt maps a normalized --format value to the file extension
// writeExtractDir gives it. It also defines the complete set of formats
// extract accepts, so writeExtract and writeExtractDir reject an unsupported
// format through the same typed error rather than two different shapes for
// the same user mistake.
var extractFormatExt = map[string]string{"text": ".txt", "json": ".json", "html": ".html"}

// validateExtractFormat is called as early as possible — before any network
// request — so a typo in --format is never discovered only after a whole
// book has already been fetched.
func validateExtractFormat(format string) error {
	if _, ok := extractFormatExt[format]; !ok {
		return &agentInputError{
			Code:       "invalid_format",
			Message:    fmt.Sprintf("unsupported format %q", format),
			Suggestion: "Use text, json, or html.",
		}
	}
	return nil
}

// writeExtract renders extracted pages as text, JSON, or HTML. In the text
// and HTML forms, a page's own paragraph structure (blank-line-separated,
// from NormalizeText) is written through unchanged: collapsing it to one
// line would erase the one thing that makes whole-book extraction usable.
func writeExtract(w io.Writer, pages []pressbooks.ExtractedPage, format string) error {
	format = strings.ToLower(format)
	if format == "" {
		format = "text"
	}
	switch format {
	case "json":
		return pressbooks.WriteJSON(w, pages)
	case "html":
		for _, p := range pages {
			if _, err := fmt.Fprintf(w, "<!-- %s | %s -->\n%s\n\n", p.PageTitle, p.SourceURL, p.HTML); err != nil {
				return err
			}
		}
	case "text":
		for i, p := range pages {
			if i > 0 {
				if _, err := fmt.Fprintln(w, "\n\n---"); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "# %s\n%s\n\n%s\n", p.PageTitle, p.SourceURL, p.Text); err != nil {
				return err
			}
		}
	default:
		return validateExtractFormat(format)
	}
	return nil
}

// writeExtractDir writes one file per page plus an index.json manifest. This is
// the shape a retrieval pipeline wants: numbered files in reading order, and
// one place to read every page's identity back from.
func writeExtractDir(dir string, pages []pressbooks.ExtractedPage, format string) (int, error) {
	if err := validateExtractFormat(format); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	ext := extractFormatExt[format]

	type manifestEntry struct {
		Ordinal   int    `json:"ordinal"`
		PageID    int    `json:"page_id"`
		PageType  string `json:"page_type"`
		Title     string `json:"title"`
		Slug      string `json:"slug"`
		SourceURL string `json:"source_url"`
		File      string `json:"file"`
		Chars     int    `json:"chars"`
	}
	manifest := struct {
		BookURL   string          `json:"book_url"`
		BookTitle string          `json:"book_title"`
		Format    string          `json:"format"`
		Pages     []manifestEntry `json:"pages"`
	}{Format: format}
	if len(pages) > 0 {
		manifest.BookURL = pages[0].BookURL
		manifest.BookTitle = pages[0].BookTitle
	}

	written := 0
	for i, page := range pages {
		name := fmt.Sprintf("%04d-%s%s", i+1, safeFileSlug(page.PageSlug, page.PageID), ext)
		var body strings.Builder
		if err := writeExtract(&body, []pressbooks.ExtractedPage{page}, format); err != nil {
			return written, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body.String()), 0o644); err != nil {
			return written, err
		}
		written += len(body.String())
		manifest.Pages = append(manifest.Pages, manifestEntry{
			Ordinal: i + 1, PageID: page.PageID, PageType: page.PageType,
			Title: page.PageTitle, Slug: page.PageSlug, SourceURL: page.SourceURL,
			File: name, Chars: len([]rune(page.Text)),
		})
	}

	indexBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return written, err
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), indexBody, 0o644); err != nil {
		return written, err
	}
	return written + len(indexBody), nil
}

// safeFileSlug keeps a page's slug usable as a filename. A slug is upstream
// data, so it is never trusted to stay inside the output directory.
func safeFileSlug(slug string, id int) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		case r == ' ' || r == '_':
			return '-'
		default:
			return -1
		}
	}, slug)
	cleaned = strings.Trim(cleaned, "-")
	if len(cleaned) > 60 {
		cleaned = cleaned[:60]
	}
	if cleaned == "" {
		return fmt.Sprintf("page-%d", id)
	}
	return cleaned
}

// extractCmd extracts one page or a whole book as text, JSON, or HTML, or
// writes it out to a directory of files.
func extractCmd(f *flags) *cobra.Command {
	var pageRef string
	var all bool
	var includeHTML bool
	var format string
	var limit int
	var offset int
	var maxChars int
	var startChar int
	var dir string
	var delayMS int
	cmd := &cobra.Command{
		Use:   "extract BOOK",
		Short: "Extract a page or an entire textbook as text, JSON, or HTML.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && pageRef != "" {
				return &agentInputError{Code: "conflicting_page_selection", Message: "--page and --all cannot be used together", Suggestion: "Choose one page with --page, or extract every page with --all."}
			}
			if all && startChar != 0 {
				return &agentInputError{Code: "conflicting_text_offset", Message: "--start-char and --all cannot be used together", Suggestion: "Continue one page at a time with --page PAGE --start-char N."}
			}
			if f.agent && includeHTML {
				return &agentInputError{Code: "unsupported_agent_option", Message: "--include-html is not available in agent mode", Suggestion: "Use bounded text in agent mode, or use --json without --agent for raw HTML."}
			}
			delay := extractDelayDefault
			if cmd.Flags().Changed("delay") {
				if delayMS < 0 {
					return &agentInputError{Code: "invalid_delay", Message: "delay must be zero or greater", Suggestion: "Use --delay 0 to disable pacing, or omit --delay to use the default of 100ms."}
				}
				delay = time.Duration(delayMS) * time.Millisecond
			}
			if f.agent && cmd.Flags().Changed("dir") && dir == "" {
				return &agentInputError{Code: "output_required", Message: "agent directory export requires an explicit --dir path", Suggestion: "Choose a writable directory path and pass it with --dir."}
			}
			// A directory export always writes every matching page, ignoring
			// agent pagination entirely, so the pagination bounds check below
			// must not run for it: it would reject a --limit/--offset that a
			// --dir export never even looks at.
			if f.agent && all && dir == "" {
				if _, err := agentWindowBounds(offset, limit, 1, 5); err != nil {
					return err
				}
			}
			if f.agent && maxChars == 0 {
				maxChars = 12000
			}
			if f.agent && maxChars > 50000 {
				return &agentInputError{Code: "max_chars_too_large", Message: fmt.Sprintf("max-chars %d exceeds the agent maximum of 50000", maxChars), Suggestion: "Use --max-chars 50000 or less and follow next_start_char."}
			}
			format = strings.ToLower(format)
			if f.asJSON && !f.agent && !cmd.Flags().Changed("format") {
				format = "json"
			}
			// Validate --format before any network request: an unsupported
			// format must never be discovered only after a whole book has
			// already been fetched (or, worse, after --dir has already
			// created the output directory).
			if format == "" {
				format = "text"
			}
			if err := validateExtractFormat(format); err != nil {
				return err
			}
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
			pages := pressbooks.FlattenPages(toc, ref)
			var meta *agentMeta
			switch {
			case all && dir != "":
				// A directory export always writes every matching page; agent
				// pagination limits do not apply to it.
			case all && f.agent:
				start, end, pageMeta, windowErr := agentWindow(len(pages), offset, limit, 1, 5)
				if windowErr != nil {
					return windowErr
				}
				pages = pages[start:end]
				meta = &pageMeta
			case !all:
				if pageRef == "" {
					if len(pages) == 0 {
						return fmt.Errorf("book has no extractable pages")
					}
					pageRef = pages[0].Slug
				}
				page, ok := pressbooks.FindPage(pages, pageRef)
				if !ok {
					return fmt.Errorf("page %q not found in %s", pageRef, book.Title)
				}
				pages = []pressbooks.Page{page}
				if f.agent {
					_, _, pageMeta, _ := agentWindow(1, 0, 1, 1, 1)
					meta = &pageMeta
				}
			}
			extracted, err := extractPages(ctx, c, ref, book, pages, !f.agent && (includeHTML || format == "html"), delay)
			if err != nil {
				return err
			}
			if dir != "" {
				written, writeErr := writeExtractDir(dir, extracted, format)
				if writeErr != nil {
					return writeErr
				}
				absDir, absErr := filepath.Abs(dir)
				if absErr != nil {
					return absErr
				}
				result := agentExtractDirResult{Directory: absDir, PageCount: len(extracted), Bytes: written}
				if f.agent {
					return writeAgentSuccess(cmd.OutOrStdout(), "extract", result, nil)
				}
				if f.asJSON {
					return pressbooks.WriteJSON(cmd.OutOrStdout(), result)
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %d page(s) to %s (%d bytes)\n", result.PageCount, result.Directory, result.Bytes)
				return err
			}
			if f.agent {
				compact, compactErr := compactExtractedPages(extracted, startChar, maxChars)
				if compactErr != nil {
					return compactErr
				}
				return writeAgentSuccess(cmd.OutOrStdout(), "extract", compact, meta)
			}
			return writeExtract(cmd.OutOrStdout(), extracted, format)
		},
	}
	cmd.Flags().StringVar(&pageRef, "page", "", "Page id or slug; defaults to the first page unless --all is set")
	cmd.Flags().BoolVar(&all, "all", false, "Extract every page in table-of-contents order")
	cmd.Flags().BoolVar(&includeHTML, "include-html", false, "Include raw content HTML in JSON output")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text, json, html")
	cmd.Flags().IntVar(&limit, "limit", 0, "Agent page limit with --all (default 1, maximum 5)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Agent page offset with --all")
	cmd.Flags().IntVar(&maxChars, "max-chars", 0, "Agent text limit per page (default 12000, maximum 50000)")
	cmd.Flags().IntVar(&startChar, "start-char", 0, "Agent text offset for continuation")
	cmd.Flags().StringVar(&dir, "dir", "", "Write extracted pages to this directory as numbered files plus index.json")
	// The registered default is 0, not the real 100ms default, and not a -1
	// sentinel: cobra appends "(default N)" to the help line for any non-zero
	// default, so -1 rendered as a second, contradictory default beside the
	// real one this usage string states. Which value is the sentinel does not
	// matter — Changed("delay") is what distinguishes "unset" from "set" —
	// and 0 is the same convention --max-chars already uses. An explicit
	// --delay 0 still means no pacing.
	cmd.Flags().IntVar(&delayMS, "delay", 0, "Milliseconds to wait between page fetches during whole-book extraction (default 100)")
	return cmd
}
