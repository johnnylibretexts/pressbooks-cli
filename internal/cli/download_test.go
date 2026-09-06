package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
)

// downloadTestFake extends extractTestFake with the export-probing and
// download plumbing download needs: a book that offers pdf and epub but not
// mobi, so export_unavailable has something concrete to name.
func downloadTestFake() *fakeClient {
	fake := extractTestFake()
	fake.exportFormats = map[string][]string{
		extractTestBookURL: {"pdf", "epub"},
	}
	fake.downloadBody = []byte("%PDF-1.4 fake export bytes")
	return fake
}

func decodeDownloadResult(t *testing.T, stdout string) agentDownloadResult {
	t.Helper()
	var envelope struct {
		Data agentDownloadResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode download result from %q: %v", stdout, err)
	}
	return envelope.Data
}

// 1. A publisher export streams to the requested path and reports its byte
// count.
func TestDownloadExportStreamsToRequestedPath(t *testing.T) {
	fake := downloadTestFake()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "book.pdf")

	stdout, stderr, err := executeWithFake(t, fake, "--agent", "download", extractTestBookURL, "--kind", "pdf", "--output", outPath)
	if err != nil {
		t.Fatalf("download --kind pdf: %v; stderr=%s", err, stderr)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(got) != string(fake.downloadBody) {
		t.Fatalf("downloaded content = %q, want %q", got, fake.downloadBody)
	}
	result := decodeDownloadResult(t, stdout)
	if result.Bytes != int64(len(fake.downloadBody)) {
		t.Fatalf("reported bytes = %d, want %d", result.Bytes, len(fake.downloadBody))
	}
	if result.Kind != "pdf" {
		t.Fatalf("reported kind = %q, want pdf", result.Kind)
	}
	if len(fake.downloadURLs) != 1 || !strings.Contains(fake.downloadURLs[0], "type=pdf") {
		t.Fatalf("expected a download of the pdf export URL, got %#v", fake.downloadURLs)
	}
}

// 2. --page with an explicit --all fails with conflicting_page_selection.
func TestDownloadPageWithExplicitAllConflicts(t *testing.T) {
	fake := downloadTestFake()
	dir := t.TempDir()

	stdout, _, err := executeWithFake(t, fake, "--agent", "download", extractTestBookURL, "--kind", "text",
		"--page", "5", "--all=true", "--output", filepath.Join(dir, "out.txt"))
	if err == nil {
		t.Fatal("expected an error for --page combined with an explicit --all")
	}
	if code := agentErrorCode(t, stdout); code != "conflicting_page_selection" {
		t.Fatalf("error code = %q, want conflicting_page_selection", code)
	}
}

// 3. A failed transfer removes the partial file it started writing.
func TestDownloadFailedTransferRemovesPartialFile(t *testing.T) {
	fake := downloadTestFake()
	fake.downloadErr = errUnexpectedCall // any error; nonempty body below proves partial bytes were written first
	fake.downloadBody = []byte("partial bytes that must not survive")
	dir := t.TempDir()
	outPath := filepath.Join(dir, "book.pdf")

	_, _, err := executeWithFake(t, fake, "download", extractTestBookURL, "--kind", "pdf", "--output", outPath)
	if err == nil {
		t.Fatal("expected the download to fail")
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatalf("partial file %s should have been removed, stat err = %v", outPath, statErr)
	}
}

// 4. Agent mode without --output fails with output_required.
func TestDownloadAgentRequiresOutput(t *testing.T) {
	fake := downloadTestFake()

	stdout, _, err := executeWithFake(t, fake, "--agent", "download", extractTestBookURL, "--kind", "pdf")
	if err == nil {
		t.Fatal("expected an error when agent mode omits --output")
	}
	if code := agentErrorCode(t, stdout); code != "output_required" {
		t.Fatalf("error code = %q, want output_required", code)
	}
}

// 5. An unavailable format fails with export_unavailable and names the
// formats the book does offer.
// TestDownloadExportFormatsErrorIsNotReportedAsUnavailable is the CLI-level
// half of the Important-5 fix: when ExportFormats itself errors (a blocked
// or rate-limited host, not a genuine "book offers nothing"), downloadExport
// must propagate that error as-is rather than reaching export_unavailable,
// which would tell an agent the book offers no such format and that
// retrying is pointless.
func TestDownloadExportFormatsErrorIsNotReportedAsUnavailable(t *testing.T) {
	fake := downloadTestFake()
	fake.exportErr = errBlockedForTest
	stdout, _, err := executeWithFake(t, fake, "--agent", "download", extractTestBookURL, "--kind", "pdf",
		"--output", filepath.Join(t.TempDir(), "book.pdf"))
	if err == nil {
		t.Fatal("expected the download to fail")
	}
	if code := agentErrorCode(t, stdout); code != "blocked_by_host" {
		t.Fatalf("error code = %q, want blocked_by_host (not export_unavailable — the host is blocked, this book's own formats are unknown)", code)
	}
}

func TestDownloadUnavailableExportNamesAvailableFormats(t *testing.T) {
	fake := downloadTestFake()
	dir := t.TempDir()

	stdout, _, err := executeWithFake(t, fake, "--agent", "download", extractTestBookURL, "--kind", "mobi",
		"--output", filepath.Join(dir, "book.mobi"))
	if err == nil {
		t.Fatal("expected an error for an export format the book does not offer")
	}
	if code := agentErrorCode(t, stdout); code != "export_unavailable" {
		t.Fatalf("error code = %q, want export_unavailable", code)
	}
	if !strings.Contains(stdout, "pdf") || !strings.Contains(stdout, "epub") {
		t.Fatalf("expected the available formats (pdf, epub) named in the error: %q", stdout)
	}
	if len(fake.downloadURLs) != 0 {
		t.Fatalf("must not attempt a download of an unavailable format, got %#v", fake.downloadURLs)
	}
}

// 6. --kind text writes extracted content rather than a publisher file.
func TestDownloadKindTextWritesExtractedContent(t *testing.T) {
	fake := downloadTestFake()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "chapter.txt")

	_, stderr, err := executeWithFake(t, fake, "download", extractTestBookURL, "--kind", "text",
		"--page", "5", "--output", outPath)
	if err != nil {
		t.Fatalf("download --kind text: %v; stderr=%s", err, stderr)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if !strings.Contains(string(got), "abçdef") {
		t.Fatalf("expected chapter one's text in %s, got %q", outPath, got)
	}
	if len(fake.downloadURLs) != 0 {
		t.Fatalf("--kind text must not touch the publisher export path, got %#v", fake.downloadURLs)
	}
}

// TestDownloadKindTextDefaultsToAllPages confirms --kind text without --page
// defaults to the whole book, matching extract's own default, rather than
// silently downloading nothing or only the first page.
func TestDownloadKindTextDefaultsToAllPages(t *testing.T) {
	fake := downloadTestFake()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "book.txt")

	_, stderr, err := executeWithFake(t, fake, "download", extractTestBookURL, "--kind", "text", "--output", outPath)
	if err != nil {
		t.Fatalf("download --kind text: %v; stderr=%s", err, stderr)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	for _, want := range []string{"Front matter text", "abçdef", "Appendix text", "Back matter text"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("expected %q in whole-book download, got %q", want, got)
		}
	}
}

// TestDownloadKindTextRootHostedBookUsesHostFilename is the Important-4 fix's
// dotfile test: a root-hosted book (design section 4 — a single-book install
// published at the host root) has an empty BookRef.Slug, so before this fix
// the tool-chosen fallback name was literally ref.Slug + ".txt" == ".txt": a
// hidden file, written silently, with a success envelope reporting it.
func TestDownloadKindTextRootHostedBookUsesHostFilename(t *testing.T) {
	const rootBookURL = "https://books.test/"
	fake := &fakeClient{
		books:    map[string]pressbooks.Book{rootBookURL: {URL: rootBookURL, Title: "Applied Ecology"}},
		tocs:     map[string]pressbooks.TOC{rootBookURL: extractTestTOC()},
		pageHTML: extractTestFake().pageHTML,
	}
	t.Chdir(t.TempDir())

	_, stderr, err := executeWithFake(t, fake, "download", rootBookURL, "--kind", "text")
	if err != nil {
		t.Fatalf("download --kind text (root-hosted book): %v; stderr=%s", err, stderr)
	}
	if _, statErr := os.Stat(".txt"); statErr == nil {
		t.Fatal("must not write a dotfile named \".txt\" for a root-hosted book")
	}
	entries, readErr := os.ReadDir(".")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "books.test.txt" {
		t.Fatalf("directory entries = %v, want exactly one file named %q (fallback to the host, not a dotfile)", entries, "books.test.txt")
	}
}

// TestDownloadKindTextWithNoOutputRefusesToOverwriteExistingFile applies the
// export branch's already-established refuse-to-clobber policy to the
// extracted-content branch too: before this fix, downloadExtracted's
// os.WriteFile silently replaced whatever file already had the tool-chosen
// name, while the sibling export branch (downloadToFile) already refused in
// exactly this situation.
func TestDownloadKindTextWithNoOutputRefusesToOverwriteExistingFile(t *testing.T) {
	fake := extractTestFake()
	t.Chdir(t.TempDir())

	existing := []byte("a file the caller already had, unrelated to this download")
	if err := os.WriteFile("b.txt", existing, 0o644); err != nil {
		t.Fatalf("seeding an existing file: %v", err)
	}

	_, _, err := executeWithFake(t, fake, "download", extractTestBookURL, "--kind", "text")
	if err == nil {
		t.Fatal("expected the download to refuse to overwrite an existing file")
	}
	got, readErr := os.ReadFile("b.txt")
	if readErr != nil {
		t.Fatalf("reading the pre-existing file: %v", readErr)
	}
	if string(got) != string(existing) {
		t.Fatalf("pre-existing file was modified: got %q, want %q", got, existing)
	}
}

// TestDownloadPacesWholeBookFetches proves download --kind text shares
// extractPages' pacing with extract --all: whole-book download is the
// design's "second-largest burst this tool produces," and before this fix
// there was no --delay mechanism at all, so a 104-page book downloaded at
// full, unpaced speed.
func TestDownloadPacesWholeBookFetches(t *testing.T) {
	const testDelay = 20 * time.Millisecond
	withExtractDelay(t, testDelay)
	fake := extractTestFake()
	dir := t.TempDir()

	_, stderr, err := executeWithFake(t, fake, "download", extractTestBookURL, "--kind", "text",
		"--output", filepath.Join(dir, "book.txt"))
	if err != nil {
		t.Fatalf("download --kind text: %v; stderr=%s", err, stderr)
	}
	assertPacedCalls(t, fake.pageContentCalls, testDelay)
}

// TestDownloadInvalidKindIsRejected confirms an unsupported --kind is
// rejected before any network request, in both agent and plain modes.
func TestDownloadInvalidKindIsRejected(t *testing.T) {
	fake := downloadTestFake()
	dir := t.TempDir()

	stdout, _, err := executeWithFake(t, fake, "--agent", "download", extractTestBookURL, "--kind", "docx",
		"--output", filepath.Join(dir, "book.docx"))
	if err == nil {
		t.Fatal("expected an error for an unsupported download kind")
	}
	if code := agentErrorCode(t, stdout); code != "invalid_download_kind" {
		t.Fatalf("error code = %q, want invalid_download_kind", code)
	}
	if len(fake.downloadURLs) != 0 {
		t.Fatalf("must not attempt any network work for an invalid kind, got downloads %#v", fake.downloadURLs)
	}
}

// The following three tests exercise downloadToFile's no-explicit-output
// branch: streaming an export to a temp file, then renaming it to the
// server-suggested name (fakeClient.Download always suggests "book.pdf").
// Each runs in a temporary working directory via t.Chdir so nothing lands in
// the repository or a developer's home, and so it never collides with a
// concurrently running test.

// A successful download with no --output lands under the server-suggested
// name, in the current directory, with the expected bytes — and no leftover
// temp file beside it.
func TestDownloadExportWithNoOutputUsesServerSuggestedName(t *testing.T) {
	fake := downloadTestFake()
	t.Chdir(t.TempDir())

	stdout, stderr, err := executeWithFake(t, fake, "--json", "download", extractTestBookURL, "--kind", "pdf")
	if err != nil {
		t.Fatalf("download --kind pdf (no --output): %v; stderr=%s", err, stderr)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading working directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file in the working directory, got %v", entries)
	}
	if entries[0].Name() != "book.pdf" {
		t.Fatalf("file name = %q, want the server-suggested %q (no leftover temp file)", entries[0].Name(), "book.pdf")
	}
	got, err := os.ReadFile("book.pdf")
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(got) != string(fake.downloadBody) {
		t.Fatalf("downloaded content = %q, want %q", got, fake.downloadBody)
	}
	var result agentDownloadResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode --json download result from %q: %v", stdout, err)
	}
	if result.Bytes != int64(len(fake.downloadBody)) {
		t.Fatalf("reported bytes = %d, want %d", result.Bytes, len(fake.downloadBody))
	}
}

// A transfer that fails after bytes reached the temp file leaves nothing
// behind in the working directory — neither the temp file nor a partial
// target file.
func TestDownloadExportWithNoOutputRemovesTempFileOnFailure(t *testing.T) {
	fake := downloadTestFake()
	fake.downloadErr = errUnexpectedCall
	fake.downloadBody = []byte("partial bytes that must not survive")
	t.Chdir(t.TempDir())

	_, _, err := executeWithFake(t, fake, "download", extractTestBookURL, "--kind", "pdf")
	if err == nil {
		t.Fatal("expected the download to fail")
	}
	entries, readErr := os.ReadDir(".")
	if readErr != nil {
		t.Fatalf("reading working directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no files left behind after a failed download, got %v", entries)
	}
}

// A download whose server-suggested target name already exists in the
// working directory refuses rather than silently overwriting the file that
// was already there, and leaves that existing file untouched.
func TestDownloadExportWithNoOutputRefusesToOverwriteExistingFile(t *testing.T) {
	fake := downloadTestFake()
	t.Chdir(t.TempDir())

	existing := []byte("a file the caller already had, unrelated to this download")
	if err := os.WriteFile("book.pdf", existing, 0o644); err != nil {
		t.Fatalf("seeding an existing file: %v", err)
	}

	_, _, err := executeWithFake(t, fake, "download", extractTestBookURL, "--kind", "pdf")
	if err == nil {
		t.Fatal("expected the download to refuse to overwrite an existing file")
	}

	got, readErr := os.ReadFile("book.pdf")
	if readErr != nil {
		t.Fatalf("reading the pre-existing file: %v", readErr)
	}
	if string(got) != string(existing) {
		t.Fatalf("pre-existing file was modified: got %q, want %q", got, existing)
	}
	entries, readErr := os.ReadDir(".")
	if readErr != nil {
		t.Fatalf("reading working directory: %v", readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the pre-existing file to remain (no leftover temp file), got %v", entries)
	}
}

// TestDownloadKindTextRefusesToOverwriteBeforeFetchingAnyPage pins the order
// of the two operations, not just the outcome. The refusal used to happen
// after extractPages had already walked the whole book: on a 300-page book
// that is 300 paced requests against someone else's server, spent only to
// throw the result away. Being modest with these hosts is a stated design
// property, so the check that needs no request has to run first.
func TestDownloadKindTextRefusesToOverwriteBeforeFetchingAnyPage(t *testing.T) {
	fake := extractTestFake()
	t.Chdir(t.TempDir())

	if err := os.WriteFile("b.txt", []byte("already here"), 0o644); err != nil {
		t.Fatalf("seeding an existing file: %v", err)
	}

	_, _, err := executeWithFake(t, fake, "download", extractTestBookURL, "--kind", "text")
	if err == nil {
		t.Fatal("expected the download to refuse to overwrite an existing file")
	}
	if n := len(fake.pageContentCalls); n != 0 {
		t.Fatalf("download fetched %d page(s) before refusing; a refusal that needs no network request must come first", n)
	}
}
