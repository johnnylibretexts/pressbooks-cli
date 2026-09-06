package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
)

// extractTestTOC gives extract tests four extractable pages, spanning every
// TOC section extraction has to walk: front matter, a chapter inside a part,
// a part that carries its own content, and back matter.
func extractTestTOC() pressbooks.TOC {
	return pressbooks.TOC{
		FrontMatter: []pressbooks.TOCItem{
			{ID: 1, Title: "Preface", Slug: "preface", Status: "publish", HasPostContent: true,
				Link: "https://books.test/b/front-matter/preface/"},
		},
		Parts: []pressbooks.TOCPart{
			{ID: 10, Title: "Part One", Slug: "part-one", Status: "publish", HasPostContent: false,
				Chapters: []pressbooks.TOCItem{
					{ID: 5, Title: "Chapter One", Slug: "chapter-1", Status: "publish", HasPostContent: true,
						Link: "https://books.test/b/chapter/chapter-1/"},
				}},
			{ID: 20, Title: "Appendix Part", Slug: "appendix-part", Status: "publish", HasPostContent: true,
				Link: "https://books.test/b/part/appendix-part/"},
		},
		BackMatter: []pressbooks.TOCItem{
			{ID: 90, Title: "Resources", Slug: "resources", Status: "publish", HasPostContent: true,
				Link: "https://books.test/b/back-matter/resources/"},
		},
	}
}

const extractTestBookURL = "https://books.test/b/"

func extractTestFake() *fakeClient {
	return &fakeClient{
		books: map[string]pressbooks.Book{
			extractTestBookURL: {URL: extractTestBookURL, Title: "Applied Ecology"},
		},
		tocs: map[string]pressbooks.TOC{extractTestBookURL: extractTestTOC()},
		pageHTML: map[int]string{
			1:  `<html><body><div><p>Front matter text.</p></div></body></html>`,
			5:  `<html><body><div><p>abçdef</p><p>Second paragraph.</p></div></body></html>`,
			20: `<html><body><div><p>Appendix text.</p></div></body></html>`,
			90: `<html><body><div><p>Back matter text.</p></div></body></html>`,
		},
	}
}

// 1. extract BOOK --page 5 prints that page's text and no other page's.
func TestExtractPagePrintsOnlyThatPage(t *testing.T) {
	fake := extractTestFake()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--page", "5")
	if err != nil {
		t.Fatalf("extract --page 5: %v; stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "abçdef\n\nSecond paragraph.") {
		t.Fatalf("expected chapter one's paragraph-structured text (blank line between paragraphs) in output: %q", stdout)
	}
	if strings.Contains(stdout, "Front matter text") || strings.Contains(stdout, "Appendix text") || strings.Contains(stdout, "Back matter text") {
		t.Fatalf("extract --page 5 must not print any other page's text: %q", stdout)
	}
}

// 2. extract BOOK --all --format json returns every extractable page in
// reading order.
func TestExtractAllJSONReturnsEveryPageInReadingOrder(t *testing.T) {
	fake := extractTestFake()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--format", "json")
	if err != nil {
		t.Fatalf("extract --all --format json: %v; stderr=%s", err, stderr)
	}
	var got []pressbooks.ExtractedPage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("extract --all json output is not JSON: %v; output=%s", err, stdout)
	}
	if len(got) != 4 {
		t.Fatalf("got %d pages, want 4: %#v", len(got), got)
	}
	wantOrder := []int{1, 5, 20, 90}
	for i, page := range got {
		if page.PageID != wantOrder[i] {
			t.Fatalf("page %d has id %d, want %d (reading order front-matter, chapter, part, back-matter)", i, page.PageID, wantOrder[i])
		}
	}
}

// 3. extract BOOK --page 5 --agent bounds text to 12,000 characters and sets
// next_start_char; passing that value back as --start-char returns the
// remainder with truncated true and no next_start_char. Uses genuinely
// non-ASCII content ("abçdef") so that byte-slicing (as opposed to
// rune-slicing) would either fail this test or, worse, corrupt the output.
func TestExtractAgentContinuationSlicesByRuneNotByte(t *testing.T) {
	fake := extractTestFake()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--page", "5", "--start-char", "2", "--max-chars", "3", "--agent")
	if err != nil {
		t.Fatalf("extract --agent (first window): %v; stderr=%s", err, stderr)
	}
	var first struct {
		Data []agentExtractedPage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatalf("agent extract is not JSON: %v; output=%s", err, stdout)
	}
	if len(first.Data) != 1 {
		t.Fatalf("got %d pages, want 1", len(first.Data))
	}
	page := first.Data[0]
	// Text is "abçdef\n\nSecond paragraph."; runes[2:5] = "çde". A byte
	// slice at the same offsets would split ç's two-byte encoding in half
	// and produce invalid UTF-8 instead of "çde".
	if page.Text != "çde" {
		t.Fatalf("text = %q, want %q (proves rune, not byte, slicing)", page.Text, "çde")
	}
	if !page.Truncated || page.NextStartChar == nil {
		t.Fatalf("expected a truncated window with a continuation cursor: %#v", page)
	}
	next := *page.NextStartChar

	stdout2, stderr2, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--page", "5",
		"--start-char", strconv.Itoa(next), "--agent")
	if err != nil {
		t.Fatalf("extract --agent (continuation): %v; stderr=%s", err, stderr2)
	}
	var second struct {
		Data []agentExtractedPage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout2), &second); err != nil {
		t.Fatalf("agent extract continuation is not JSON: %v; output=%s", err, stdout2)
	}
	if len(second.Data) != 1 {
		t.Fatalf("got %d pages, want 1", len(second.Data))
	}
	tail := second.Data[0]
	if tail.CharStart != next {
		t.Fatalf("continuation char_start = %d, want %d", tail.CharStart, next)
	}
	if tail.CharEnd != tail.TotalChars {
		t.Fatalf("continuation must reach the end of the page: char_end=%d total_chars=%d", tail.CharEnd, tail.TotalChars)
	}
	if !tail.Truncated {
		t.Fatalf("final window should still report truncated=true (it starts mid-page): %#v", tail)
	}
	if tail.NextStartChar != nil {
		t.Fatalf("final window must not carry a further continuation cursor: %#v", tail)
	}
	// The full text, reconstructed from char 0, must match "abçdef" once
	// window one's remainder and window two's start (both at rune offset 2)
	// are combined: window one covers [0,2) implicitly via total_chars and
	// window two covers [2, end) — nothing dropped, nothing repeated.
	full := []rune("abçdef\n\nSecond paragraph.")
	if tail.TotalChars != len(full) {
		t.Fatalf("total_chars = %d, want %d", tail.TotalChars, len(full))
	}
	if tail.Text != string(full[next:]) {
		t.Fatalf("continuation text = %q, want %q", tail.Text, string(full[next:]))
	}
}

// TestExtractPacesWholeBookFetches is the Important-3 fix's test: design
// section 6 specifies --delay (default 100ms) to pace requests between pages
// during whole-book extraction, and before this fix no such flag existed
// anywhere and extractPages fetched every page back-to-back at full speed.
func TestExtractPacesWholeBookFetches(t *testing.T) {
	const testDelay = 20 * time.Millisecond
	withExtractDelay(t, testDelay)
	fake := extractTestFake()

	_, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--format", "json")
	if err != nil {
		t.Fatalf("extract --all: %v; stderr=%s", err, stderr)
	}
	assertPacedCalls(t, fake.pageContentCalls, testDelay)
}

// --delay 0 disables pacing outright, even when extractDelayDefault (the
// package's test default) is already zero: this pins that an explicit
// --delay 0 is honored as a deliberate choice, not merely the absence of one.
func TestExtractDelayZeroDisablesPacing(t *testing.T) {
	withExtractDelay(t, 50*time.Millisecond)
	fake := extractTestFake()

	start := time.Now()
	_, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--format", "json", "--delay", "0")
	if err != nil {
		t.Fatalf("extract --all --delay 0: %v; stderr=%s", err, stderr)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("--delay 0 should disable pacing entirely; took %v for 4 pages against a fake client", elapsed)
	}
}

// A negative --delay is rejected rather than silently treated as zero.
func TestExtractNegativeDelayIsRejected(t *testing.T) {
	fake := extractTestFake()
	stdout, _, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--delay", "-5", "--agent")
	if err == nil {
		t.Fatal("a negative --delay should fail")
	}
	if code := agentErrorCode(t, stdout); code != "invalid_delay" {
		t.Fatalf("error code = %q, want invalid_delay", code)
	}
}

// 4. --start-char together with --all fails with code conflicting_text_offset.
func TestExtractStartCharWithAllConflicts(t *testing.T) {
	fake := extractTestFake()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--start-char", "5", "--agent")
	if err == nil {
		t.Fatal("expected an error")
	}
	var inputErr *agentInputError
	if !errors.As(err, &inputErr) || inputErr.Code != "conflicting_text_offset" {
		t.Fatalf("error code = %v, want conflicting_text_offset; stdout=%s stderr=%s", err, stdout, stderr)
	}
	if agentErrorCode(t, stdout) != "conflicting_text_offset" {
		t.Fatalf("agent envelope error code mismatch: %s", stdout)
	}
}

// 5. --page together with --all fails with code conflicting_page_selection.
func TestExtractPageWithAllConflicts(t *testing.T) {
	fake := extractTestFake()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--page", "5", "--agent")
	if err == nil {
		t.Fatal("expected an error")
	}
	var inputErr *agentInputError
	if !errors.As(err, &inputErr) || inputErr.Code != "conflicting_page_selection" {
		t.Fatalf("error code = %v, want conflicting_page_selection; stdout=%s stderr=%s", err, stdout, stderr)
	}
	if agentErrorCode(t, stdout) != "conflicting_page_selection" {
		t.Fatalf("agent envelope error code mismatch: %s", stdout)
	}
}

// 6. --include-html with --agent fails with code unsupported_agent_option.
func TestExtractIncludeHTMLWithAgentUnsupported(t *testing.T) {
	fake := extractTestFake()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--page", "5", "--include-html", "--agent")
	if err == nil {
		t.Fatal("expected an error")
	}
	var inputErr *agentInputError
	if !errors.As(err, &inputErr) || inputErr.Code != "unsupported_agent_option" {
		t.Fatalf("error code = %v, want unsupported_agent_option; stdout=%s stderr=%s", err, stdout, stderr)
	}
	if agentErrorCode(t, stdout) != "unsupported_agent_option" {
		t.Fatalf("agent envelope error code mismatch: %s", stdout)
	}
}

// 7. extract BOOK --all --dir DIR writes one numbered file per page plus
// index.json, and the manifest's file values all exist on disk. Also
// confirms paragraph structure survives into the written files.
func TestExtractAllDirWritesNumberedFilesAndManifest(t *testing.T) {
	fake := extractTestFake()
	dir := t.TempDir()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--dir", dir)
	if err != nil {
		t.Fatalf("extract --all --dir: %v; stderr=%s", err, stderr)
	}
	_ = stdout

	indexBody, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatalf("reading index.json: %v", err)
	}
	var manifest struct {
		Pages []struct {
			File  string `json:"file"`
			Slug  string `json:"slug"`
			Chars int    `json:"chars"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(indexBody, &manifest); err != nil {
		t.Fatalf("index.json is not JSON: %v", err)
	}
	if len(manifest.Pages) != 4 {
		t.Fatalf("manifest has %d pages, want 4", len(manifest.Pages))
	}
	for _, entry := range manifest.Pages {
		path := filepath.Join(dir, entry.File)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("manifest file %q does not exist on disk: %v", entry.File, err)
		}
		if entry.Slug == "chapter-1" {
			if !strings.Contains(string(body), "abçdef") || !strings.Contains(string(body), "\n\nSecond paragraph.") {
				t.Fatalf("chapter-1's paragraph structure did not survive into its file: %q", string(body))
			}
		}
	}
}

// 8. extract BOOK --all --dir DIR --agent without an explicit directory
// fails; with one it returns the directory, page count and byte count.
func TestExtractAllDirAgentRequiresExplicitDirectory(t *testing.T) {
	fake := extractTestFake()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--dir", "", "--agent")
	if err == nil {
		t.Fatal("expected an error when --dir is explicitly empty in agent mode")
	}
	if agentErrorCode(t, stdout) != "output_required" {
		t.Fatalf("error code = %s, want output_required; stderr=%s", agentErrorCode(t, stdout), stderr)
	}

	dir := t.TempDir()
	stdout2, stderr2, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--dir", dir, "--agent")
	if err != nil {
		t.Fatalf("extract --all --dir --agent: %v; stderr=%s", err, stderr2)
	}
	var got struct {
		Data agentExtractDirResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout2), &got); err != nil {
		t.Fatalf("agent extract dir result is not JSON: %v; output=%s", err, stdout2)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Data.Directory != absDir {
		t.Fatalf("directory = %q, want %q", got.Data.Directory, absDir)
	}
	if got.Data.PageCount != 4 {
		t.Fatalf("page_count = %d, want 4", got.Data.PageCount)
	}
	if got.Data.Bytes <= 0 {
		t.Fatalf("bytes = %d, want > 0", got.Data.Bytes)
	}
}

// safeFileSlug must map a traversal or absolute-path slug to the exact
// sanitized string, not merely to "something that happens not to contain
// dots or slashes" — a test that only checked the absence of ".." would
// keep passing even if safeFileSlug were deleted outright, since the
// four-digit ordinal writeExtractDir prefixes onto every filename already
// guarantees no name can equal "..", independent of whatever safeFileSlug
// does. Asserting the exact expected output is what makes this test able to
// fail if the sanitization logic regresses.
func TestSafeFileSlugStripsTraversalAndAbsolutePaths(t *testing.T) {
	cases := []struct {
		slug string
		id   int
		want string
	}{
		{"../../../../etc/evil", 1, "etcevil"},
		{"/etc/passwd", 2, "etcpasswd"},
		{"Chapter One!", 3, "chapter-one"},
		{"....", 4, "page-4"},
	}
	for _, c := range cases {
		got := safeFileSlug(c.slug, c.id)
		if got != c.want {
			t.Errorf("safeFileSlug(%q, %d) = %q, want %q", c.slug, c.id, got, c.want)
		}
	}
}

// A --dir export always writes every matching page, ignoring agent
// pagination entirely, so a --limit that would be rejected for a normal
// (non-dir) agent --all extraction must not be rejected here: it is never
// even applied.
func TestExtractAllDirAgentIgnoresPaginationBounds(t *testing.T) {
	fake := extractTestFake()
	dir := t.TempDir()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--dir", dir, "--limit", "6", "--agent")
	if err != nil {
		t.Fatalf("extract --all --dir --limit 6 --agent: %v; stderr=%s", err, stderr)
	}
	var got struct {
		Data agentExtractDirResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("agent extract dir result is not JSON: %v; output=%s", err, stdout)
	}
	if got.Data.PageCount != 4 {
		t.Fatalf("page_count = %d, want 4 (a --dir export must write every page, ignoring --limit)", got.Data.PageCount)
	}
}

// --format is validated before any network request. A fake client with no
// books or TOCs configured fails loudly ("no fake book metadata for ...")
// the instant it is called, so if validation happened only inside
// writeExtract/writeExtractDir (after fetching), this test would fail with
// that unrelated error instead of invalid_format.
func TestExtractValidatesFormatBeforeAnyNetworkRequest(t *testing.T) {
	fake := &fakeClient{}

	_, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--format", "yaml")
	if err == nil {
		t.Fatal("expected an error for an unsupported format")
	}
	var inputErr *agentInputError
	if !errors.As(err, &inputErr) || inputErr.Code != "invalid_format" {
		t.Fatalf("error = %v (%T), want a typed invalid_format error; stderr=%s", err, err, stderr)
	}

	dir := t.TempDir()
	_, stderr2, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--dir", dir, "--format", "yaml")
	if err == nil {
		t.Fatal("expected an error for an unsupported format with --dir")
	}
	var inputErr2 *agentInputError
	if !errors.As(err, &inputErr2) || inputErr2.Code != "invalid_format" {
		t.Fatalf("error = %v (%T), want a typed invalid_format error; stderr=%s", err, err, stderr2)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("an unsupported --format must be rejected before --dir is populated, got %d entries", len(entries))
	}
}

// A bad --format must fail with the same typed error code whether or not
// --dir is used: it is the same user mistake either way.
func TestExtractBadFormatSameCodeWithAndWithoutDir(t *testing.T) {
	fake := extractTestFake()

	_, _, errPlain := executeWithFake(t, fake, "extract", extractTestBookURL, "--page", "5", "--format", "yaml")
	_, _, errDir := executeWithFake(t, fake, "extract", extractTestBookURL, "--page", "5", "--format", "yaml", "--dir", t.TempDir())

	var plainErr, dirErr *agentInputError
	if !errors.As(errPlain, &plainErr) || plainErr.Code != "invalid_format" {
		t.Fatalf("plain path error = %v, want invalid_format", errPlain)
	}
	if !errors.As(errDir, &dirErr) || dirErr.Code != "invalid_format" {
		t.Fatalf("--dir path error = %v, want invalid_format", errDir)
	}
	if plainErr.Code != dirErr.Code {
		t.Fatalf("same bad --format produced different codes: %q vs %q", plainErr.Code, dirErr.Code)
	}
}

// The agent defaults this command owns, pinned so a silent drift cannot
// break a contract the schema command publishes: whole-book extraction in
// agent mode returns exactly one page by default, and a page's text is
// bounded to 12,000 characters by default.
func TestExtractAgentDefaultsOnePageAnd12000Chars(t *testing.T) {
	longText := strings.Repeat("a", 12500)
	fake := &fakeClient{
		books: map[string]pressbooks.Book{
			extractTestBookURL: {URL: extractTestBookURL, Title: "Applied Ecology"},
		},
		tocs: map[string]pressbooks.TOC{extractTestBookURL: {
			FrontMatter: []pressbooks.TOCItem{
				{ID: 1, Title: "One", Slug: "one", Status: "publish", HasPostContent: true, Link: "https://books.test/b/front-matter/one/"},
			},
			BackMatter: []pressbooks.TOCItem{
				{ID: 2, Title: "Two", Slug: "two", Status: "publish", HasPostContent: true, Link: "https://books.test/b/back-matter/two/"},
			},
		}},
		pageHTML: map[int]string{
			1: "<html><body><div><p>" + longText + "</p></div></body></html>",
			2: `<html><body><div><p>short</p></div></body></html>`,
		},
	}

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--agent")
	if err != nil {
		t.Fatalf("extract --all --agent: %v; stderr=%s", err, stderr)
	}
	var got struct {
		Data []agentExtractedPage `json:"data"`
		Meta agentMeta            `json:"meta"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 1 || got.Meta.Limit != 1 {
		t.Fatalf("whole-book extraction in agent mode must default to one page: %#v", got)
	}
	page := got.Data[0]
	if page.PageID != 1 {
		t.Fatalf("expected the first (long) page, got page_id=%d", page.PageID)
	}
	if len([]rune(page.Text)) != 12000 {
		t.Fatalf("page text length = %d, want the default max-chars of 12000", len([]rune(page.Text)))
	}
	if !page.Truncated || page.NextStartChar == nil || *page.NextStartChar != 12000 {
		t.Fatalf("expected truncation at the default 12000-char boundary: %#v", page)
	}
}

// A hostile slug (path traversal, or an absolute path) must never let a
// directory write escape the requested output directory. This checks the
// exact written filename, not just the absence of "..": the four-digit
// ordinal prefix writeExtractDir adds to every filename already guarantees
// that, regardless of what safeFileSlug does with the traversal characters,
// so a test settling for "no .. in the name" could pass even with
// safeFileSlug deleted entirely.
func TestExtractDirSanitizesHostileSlugs(t *testing.T) {
	fake := &fakeClient{
		books: map[string]pressbooks.Book{
			extractTestBookURL: {URL: extractTestBookURL, Title: "Applied Ecology"},
		},
		tocs: map[string]pressbooks.TOC{extractTestBookURL: {
			FrontMatter: []pressbooks.TOCItem{
				{ID: 1, Title: "Escape", Slug: "../../../../etc/evil", Status: "publish", HasPostContent: true,
					Link: "https://books.test/b/front-matter/escape/"},
			},
		}},
		pageHTML: map[int]string{
			1: `<html><body><div><p>hostile</p></div></body></html>`,
		},
	}
	dir := t.TempDir()

	stdout, stderr, err := executeWithFake(t, fake, "extract", extractTestBookURL, "--all", "--dir", dir)
	if err != nil {
		t.Fatalf("extract --all --dir: %v; stderr=%s", err, stderr)
	}
	_ = stdout

	const wantName = "0001-etcevil.txt"
	if _, err := os.Stat(filepath.Join(dir, wantName)); err != nil {
		t.Fatalf("expected the sanitized filename %q in %s: %v", wantName, dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 || (names[0] != wantName && names[1] != wantName) {
		t.Fatalf("directory contents = %v, want exactly %q and index.json", names, wantName)
	}
	// Nothing must have been written outside dir.
	if _, err := os.Stat("/etc/evil"); err == nil {
		t.Fatal("hostile slug escaped the output directory")
	}
}
