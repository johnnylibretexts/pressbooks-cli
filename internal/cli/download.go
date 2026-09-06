package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
	"github.com/spf13/cobra"
)

// agentDownloadResult is download's agent response: where the file landed,
// what kind it is, and how many bytes were written.
type agentDownloadResult struct {
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Bytes int64  `json:"bytes"`
}

// exportKindExt names the file extension a publisher export kind gets when no
// server-suggested filename is available and no --output was given.
var exportKindExt = map[string]string{
	"pdf":       ".pdf",
	"print_pdf": ".pdf",
	"epub":      ".epub",
	"mobi":      ".mobi",
	"xhtml":     ".html",
	"htmlbook":  ".html",
	"odt":       ".odt",
	"wxr":       ".xml",
}

// extractedKindExt is the same, for the extracted-content kinds download
// shares with extract.
//
// The two fallback names read as inconsistent — a generic "download.pdf"
// here, the book's own slug ("applied-ecology.txt") for extracted content —
// but the asymmetry is deliberate. An export's real name comes from the
// server (Content-Disposition, already sanitized by filenameFrom before
// this package ever sees it); exportKindExt only fires when the server
// suggested none at all, so there is nothing book-specific to fall back to
// without a second request. Extracted content has no server-suggested name
// to prefer in the first place, so it always uses the one identifier this
// tool already has on hand: the book's slug.
var extractedKindExt = map[string]string{"text": ".txt", "json": ".json", "html": ".html"}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// downloadCmd downloads either a publisher-made export (pdf, epub, ...) or
// extracted book content (text, json, html) to a file. Following
// openstax-cli's downloadCmd: the export-vs-extracted-content branch is the
// same shape, with IsExportKind replacing that sibling's single PDF branch
// now that Pressbooks offers eight export kinds instead of one.
func downloadCmd(f *flags) *cobra.Command {
	var outPath string
	var kind string
	var pageRef string
	var all bool
	cmd := &cobra.Command{
		Use:   "download BOOK",
		Short: "Download a publisher export file or save extracted content to a file.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.agent && outPath == "" {
				return &agentInputError{
					Code:       "output_required",
					Message:    "agent downloads require an explicit --output path",
					Suggestion: "Choose a writable file path and pass it with --output.",
				}
			}
			kind = strings.ToLower(kind)
			if !pressbooks.IsExportKind(kind) && kind != "text" && kind != "json" && kind != "html" {
				return &agentInputError{
					Code:       "invalid_download_kind",
					Message:    fmt.Sprintf("unsupported download kind %q", kind),
					Suggestion: "Use one of: " + strings.Join(pressbooks.ExportKinds, ", ") + ", text, json, or html.",
				}
			}
			// --all defaults to true so that a bare download saves the whole
			// book's extracted content. Honor --page when the caller did not
			// ask for --all explicitly, rather than silently ignoring --page.
			if pageRef != "" {
				if cmd.Flags().Changed("all") && all {
					return &agentInputError{Code: "conflicting_page_selection", Message: "--page and --all cannot be used together", Suggestion: "Choose one page with --page, or omit --page to save the whole book."}
				}
				all = false
			}
			ref, err := resolveBookRef(args[0])
			if err != nil {
				return err
			}
			c, ctx := clientAndContext(f)

			var written int64
			var finalPath string
			if pressbooks.IsExportKind(kind) {
				written, finalPath, err = downloadExport(ctx, c, ref, kind, outPath)
			} else {
				written, finalPath, err = downloadExtracted(ctx, c, ref, kind, pageRef, all, outPath)
			}
			if err != nil {
				return err
			}

			absolutePath, err := filepath.Abs(finalPath)
			if err != nil {
				return err
			}
			result := agentDownloadResult{Path: absolutePath, Kind: kind, Bytes: written}
			if f.agent {
				return writeAgentSuccess(cmd.OutOrStdout(), "download", result, nil)
			}
			if f.asJSON {
				return pressbooks.WriteJSON(cmd.OutOrStdout(), result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (%d bytes)\n", finalPath, written)
			return err
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "Output file path")
	cmd.Flags().StringVar(&kind, "kind", "pdf", "Download kind: pdf, print_pdf, epub, mobi, xhtml, htmlbook, odt, wxr, text, json, html")
	cmd.Flags().StringVar(&pageRef, "page", "", "Page id or slug for extracted text/json/html")
	cmd.Flags().BoolVar(&all, "all", true, "Extract every page for text/json/html")
	return cmd
}

// downloadExport probes which formats the book actually offers and, only
// when kind is among them, streams that export to disk. Checking
// ExportFormats first — rather than letting a plain 500 come back from
// Download — is what lets export_unavailable name the formats the book does
// offer, instead of just reporting a bare upstream error.
func downloadExport(ctx context.Context, c pressbooksClient, ref pressbooks.BookRef, kind, outPath string) (int64, string, error) {
	formats, err := c.ExportFormats(ctx, ref)
	if err != nil {
		return 0, "", err
	}
	if !containsString(formats, kind) {
		available := "none"
		if len(formats) > 0 {
			available = strings.Join(formats, ", ")
		}
		return 0, "", &agentInputError{
			Code:       "export_unavailable",
			Message:    fmt.Sprintf("%q export is not available for this book", kind),
			Suggestion: fmt.Sprintf("This book offers: %s.", available),
		}
	}
	return downloadToFile(ctx, c, ref.ExportURL(kind), outPath, exportKindExt[kind])
}

// downloadToFile streams rawURL to outPath and removes the partial file if
// the transfer fails, so a failed download never leaves a truncated file
// behind. When outPath is empty (only possible outside agent mode, which
// requires an explicit path), the file is written to a temporary path first
// and renamed to the name the server suggested — the sanitized value
// pressbooks.Client.Download's third return already reduced Content-
// Disposition to, never a raw header a hostile host could use to choose
// where this tool writes — falling back to fallbackExt when the server
// suggests no usable name at all.
func downloadToFile(ctx context.Context, c pressbooksClient, rawURL, outPath, fallbackExt string) (int64, string, error) {
	if outPath != "" {
		if err := ensureParentDir(outPath); err != nil {
			return 0, "", err
		}
		file, err := os.Create(outPath)
		if err != nil {
			return 0, "", err
		}
		written, _, err := c.Download(ctx, rawURL, file)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(outPath)
			return 0, "", err
		}
		return written, outPath, nil
	}

	tmp, err := os.CreateTemp(".", "pressbooks-download-*")
	if err != nil {
		return 0, "", err
	}
	tmpPath := tmp.Name()
	written, suggested, err := c.Download(ctx, rawURL, tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return 0, "", err
	}
	name := suggested
	if name == "" {
		name = "download" + fallbackExt
	}
	// A rename onto an existing path silently replaces it on every OS this
	// tool targets. Since this branch only runs when the caller did not
	// choose a path themselves, that existing file is not one they named as
	// the destination — it might be an unrelated file that happens to share
	// the server's suggested name. Refuse rather than guess: clobbering a
	// file the caller did not ask to overwrite is the wrong default, and an
	// explicit --output is the deliberate way to choose (and overwrite) a
	// specific path.
	if err := refuseIfExists(name); err != nil {
		_ = os.Remove(tmpPath)
		return 0, "", err
	}
	if err := os.Rename(tmpPath, name); err != nil {
		_ = os.Remove(tmpPath)
		return 0, "", err
	}
	return written, name, nil
}

// downloadExtracted writes a page or a whole book's extracted content to a
// file, in text, JSON, or HTML form. It mirrors extractCmd's own page
// selection so download BOOK --kind text and extract BOOK --format text
// agree on which pages "the whole book" means.
func downloadExtracted(ctx context.Context, c pressbooksClient, ref pressbooks.BookRef, kind, pageRef string, all bool, outPath string) (int64, string, error) {
	book, err := c.BookMetadata(ctx, ref)
	if err != nil {
		return 0, "", err
	}
	toc, err := c.TOC(ctx, ref)
	if err != nil {
		return 0, "", err
	}
	pages := pressbooks.FlattenPages(toc, ref)
	if len(pages) == 0 {
		return 0, "", fmt.Errorf("%q has no extractable pages", book.Title)
	}
	if !all {
		if pageRef == "" {
			pageRef = pages[0].Slug
		}
		page, ok := pressbooks.FindPage(pages, pageRef)
		if !ok {
			return 0, "", fmt.Errorf("page %q not found in %s", pageRef, book.Title)
		}
		pages = []pressbooks.Page{page}
	}
	// Settle the output path and refuse to clobber BEFORE fetching anything.
	// extractPages below walks every page of the book at extractDelayDefault
	// pacing; on a 300-page book that is 300 paced requests against someone
	// else's server, and failing afterwards would spend all of them only to
	// throw the result away. Being modest with these hosts is a stated
	// design property, so the check that can fail without a request runs
	// first.
	path := outPath
	if path == "" {
		// ref.Slug is empty for a root-hosted book (design section 4 — a
		// single-book install published at the host root), and a bare
		// extractedKindExt[kind] extension with no name in front of it is a
		// dotfile: ".txt", written silently in the working directory. Falling
		// back to ref.Host keeps the tool-chosen name meaningful for exactly
		// the books this tool explicitly supports reading.
		base := ref.Slug
		if base == "" {
			base = ref.Host
		}
		path = base + extractedKindExt[kind]
		// Same refuse-to-clobber policy downloadToFile already applies when
		// it, too, is choosing the name instead of the caller: an explicit
		// --output is how a caller deliberately overwrites a specific path,
		// so a tool-chosen name must never silently replace an existing file
		// that merely happens to share it.
		if err := refuseIfExists(path); err != nil {
			return 0, "", err
		}
	}
	// download exposes no --delay flag of its own (the design gives that flag
	// to extract only), but it shares extractPages with extract --all, so a
	// whole-book download is paced by the same default rather than firing
	// every page's request back-to-back.
	extracted, err := extractPages(ctx, c, ref, book, pages, kind == "html", extractDelayDefault)
	if err != nil {
		return 0, "", err
	}
	var b strings.Builder
	if err := writeExtract(&b, extracted, kind); err != nil {
		return 0, "", err
	}
	content := []byte(b.String())

	if err := ensureParentDir(path); err != nil {
		return 0, "", err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return 0, "", err
	}
	return int64(len(content)), path, nil
}

// refuseIfExists reports an error if path already exists, and nil if the
// path is free to write or the check itself failed in a way that is not
// simply "does not exist" (which is then surfaced as its own error rather
// than silently treated as a green light to overwrite).
func refuseIfExists(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("a file named %q already exists; pass --output to choose a different path", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ensureParentDir creates outPath's parent directory when it does not
// already exist, so --output nested/path.pdf need not be preceded by a
// separate mkdir.
func ensureParentDir(outPath string) error {
	dir := filepath.Dir(filepath.Clean(outPath))
	if dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
