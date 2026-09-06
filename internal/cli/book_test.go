package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
)

// sampleTOCForCLI mirrors internal/pressbooks/book_test.go's sampleTOC: a
// front-matter page, a container part with a draft and an empty stub
// alongside a real chapter, a part that carries its own content, and a
// generated back-matter entry with no content.
func sampleTOCForCLI() pressbooks.TOC {
	return pressbooks.TOC{
		FrontMatter: []pressbooks.TOCItem{
			{ID: 4, Title: "Introduction", Slug: "introduction", Status: "publish", HasPostContent: true,
				Link: "https://books.test/b/front-matter/introduction/"},
		},
		Parts: []pressbooks.TOCPart{
			{ID: 279, Title: "Main Body", Slug: "main-body", Status: "publish", HasPostContent: false,
				Chapters: []pressbooks.TOCItem{
					{ID: 5, Title: "Chapter One", Slug: "chapter-1", Status: "publish", HasPostContent: true,
						Link: "https://books.test/b/chapter/chapter-1/"},
					{ID: 6, Title: "A Draft", Slug: "draft", Status: "draft", HasPostContent: true,
						Link: "https://books.test/b/chapter/draft/"},
					{ID: 7, Title: "An Empty Stub", Slug: "stub", Status: "publish", HasPostContent: false,
						Link: "https://books.test/b/chapter/stub/"},
				}},
			{ID: 288, Title: "Featured Ecologists", Slug: "featured", Status: "publish", HasPostContent: true,
				Link: "https://books.test/b/part/featured/"},
		},
		BackMatter: []pressbooks.TOCItem{
			{ID: 90, Title: "Resources", Slug: "resources", Status: "publish", HasPostContent: true,
				Link: "https://books.test/b/back-matter/resources/"},
			{ID: 91, Title: "Glossary", Slug: "glossary", Status: "publish", HasPostContent: false,
				Link: "https://books.test/b/back-matter/glossary/"},
		},
	}
}

const cliTestBookURL = "https://books.test/b/"

func TestTOCFlatPrintsOneLinePerExtractablePage(t *testing.T) {
	fake := &fakeClient{tocs: map[string]pressbooks.TOC{cliTestBookURL: sampleTOCForCLI()}}

	stdout, stderr, err := executeWithFake(t, fake, "toc", cliTestBookURL, "--flat")
	if err != nil {
		t.Fatalf("toc --flat: %v; stderr=%s", err, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4 (one per extractable page): %q", len(lines), stdout)
	}
	if !strings.Contains(lines[0], "introduction") || !strings.Contains(lines[1], "chapter-1") ||
		!strings.Contains(lines[2], "featured") || !strings.Contains(lines[3], "resources") {
		t.Fatalf("unexpected toc --flat output: %q", stdout)
	}
	// The draft and the empty stub must never be printed.
	if strings.Contains(stdout, "draft") || strings.Contains(stdout, "stub") {
		t.Fatalf("toc --flat printed a non-extractable node: %q", stdout)
	}
}

func TestInfoAgentReturnsLicenseAndPageCount(t *testing.T) {
	fake := &fakeClient{
		books: map[string]pressbooks.Book{
			cliTestBookURL: {
				URL: cliTestBookURL, Title: "Applied Ecology",
				LicenseName: "CC BY (Attribution)", LicenseURL: "https://creativecommons.org/licenses/by/4.0/",
			},
		},
		tocs:          map[string]pressbooks.TOC{cliTestBookURL: sampleTOCForCLI()},
		exportFormats: map[string][]string{cliTestBookURL: {"pdf", "epub"}},
	}

	stdout, stderr, err := executeWithFake(t, fake, "info", cliTestBookURL, "--agent")
	if err != nil {
		t.Fatalf("info --agent: %v; stderr=%s", err, stderr)
	}
	var got struct {
		Data agentBookDetails `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("agent info is not JSON: %v; output=%s", err, stdout)
	}
	if got.Data.License.Name != "CC BY (Attribution)" ||
		got.Data.License.URL != "https://creativecommons.org/licenses/by/4.0/" {
		t.Fatalf("unexpected license: %#v", got.Data.License)
	}
	if got.Data.PageCount != 4 {
		t.Fatalf("page count = %d, want 4 (matching FlattenPages' extractable set)", got.Data.PageCount)
	}
	if len(got.Data.ExportFormats) != 2 || got.Data.ExportFormats[0] != "pdf" || got.Data.ExportFormats[1] != "epub" {
		t.Fatalf("export formats = %#v, want [pdf epub]", got.Data.ExportFormats)
	}
}

// TestInfoOmitsWordCountWhenTheBookMetadataEndpointDidNotReportOne pins the
// Minor fix: the book metadata endpoint info reads genuinely omits
// wordCount (the network listing endpoint search and books read carries it;
// this one does not), so a book with Book.WordCount left at its zero value
// must have "word_count" absent from info's JSON entirely, not asserted as
// 0 — the difference between "not reported" and "this book has no words."
func TestInfoOmitsWordCountWhenTheBookMetadataEndpointDidNotReportOne(t *testing.T) {
	fake := &fakeClient{
		books: map[string]pressbooks.Book{
			cliTestBookURL: {URL: cliTestBookURL, Title: "Applied Ecology"}, // WordCount left at 0
		},
		tocs:          map[string]pressbooks.TOC{cliTestBookURL: sampleTOCForCLI()},
		exportFormats: map[string][]string{cliTestBookURL: {}},
	}
	stdout, stderr, err := executeWithFake(t, fake, "info", cliTestBookURL, "--agent")
	if err != nil {
		t.Fatalf("info --agent: %v; stderr=%s", err, stderr)
	}
	if strings.Contains(stdout, "word_count") {
		t.Fatalf("word_count must be omitted (not asserted as 0) when unreported: %s", stdout)
	}
}

func TestInfoNoExportsNeverCallsExportFormats(t *testing.T) {
	fake := &fakeClient{
		books: map[string]pressbooks.Book{
			cliTestBookURL: {URL: cliTestBookURL, Title: "Applied Ecology", LicenseName: "CC BY (Attribution)"},
		},
		tocs: map[string]pressbooks.TOC{cliTestBookURL: sampleTOCForCLI()},
		// exportFormats is left nil: ExportFormats would fail loudly with "no
		// fake export formats for %q" if info called it despite --no-exports.
	}

	stdout, stderr, err := executeWithFake(t, fake, "info", cliTestBookURL, "--no-exports", "--agent")
	if err != nil {
		t.Fatalf("info --no-exports --agent: %v; stderr=%s", err, stderr)
	}
	var got struct {
		OK   bool             `json:"ok"`
		Data agentBookDetails `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("agent info is not JSON: %v; output=%s", err, stdout)
	}
	if !got.OK || got.Data.Title != "Applied Ecology" {
		t.Fatalf("unexpected result: %#v", got)
	}
}

// toc's default output must show the book's actual structure — including a
// container part with no content of its own — which the flattened,
// extractable-only list --flat prints can never show, since FlattenPages
// drops that container entirely. sampleTOCForCLI's "Main Body" part is
// exactly that shape: HasPostContent is false, and it holds three chapters.
func TestTOCDefaultShowsHierarchyWhileFlatShowsOnlyExtractablePages(t *testing.T) {
	fake := &fakeClient{tocs: map[string]pressbooks.TOC{cliTestBookURL: sampleTOCForCLI()}}

	stdout, stderr, err := executeWithFake(t, fake, "toc", cliTestBookURL)
	if err != nil {
		t.Fatalf("toc: %v; stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "main-body") {
		t.Fatalf("default toc output must show the container part: %q", stdout)
	}
	if !strings.Contains(stdout, "draft") || !strings.Contains(stdout, "stub") {
		t.Fatalf("default toc output must show every chapter, extractable or not: %q", stdout)
	}

	flatStdout, flatStderr, err := executeWithFake(t, fake, "toc", cliTestBookURL, "--flat")
	if err != nil {
		t.Fatalf("toc --flat: %v; stderr=%s", err, flatStderr)
	}
	if strings.Contains(flatStdout, "main-body") {
		t.Fatalf("toc --flat must not show the container part, which has no content of its own: %q", flatStdout)
	}
	if strings.Contains(flatStdout, "draft") || strings.Contains(flatStdout, "stub") {
		t.Fatalf("toc --flat must not show a non-extractable node: %q", flatStdout)
	}
	if stdout == flatStdout {
		t.Fatal("default and --flat toc output must differ")
	}
}
