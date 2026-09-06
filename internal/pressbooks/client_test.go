package pressbooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetSendsTheBrowserShapedUserAgent(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	if _, err := New(0).Get(context.Background(), server.URL); err != nil {
		t.Fatal(err)
	}
	if got != UserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, UserAgent)
	}
}

// These hosts answer an unrecognised client with 403 and an HTML block page.
// Reporting that as a missing book would send a caller looking for the wrong
// problem. A single 403 gets one retry (forbiddenRetryLimit) before being
// reported, rather than none: a shared backend under momentary rate-limit
// pressure also answers 403, and a host that is genuinely, permanently
// blocking this client — as this server always does — still ends up
// classified as ErrBlocked once that budget is spent, just not on the first
// response.
//
// NOTE: this replaces a prior assertion that a 403 was never retried at all
// (hits == 1); giving a 403 one retry before declaring it blocked is exactly
// what this fix round asked for, so the expected hit count changes to
// forbiddenRetryLimit (2) rather than being a silently-updated number.
func TestForbiddenIsRetriedOnceBeforeBeingReportedAsBlocked(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<html><body>Request blocked.</body></html>")
	}))
	defer server.Close()

	client := New(0)
	client.backoff = time.Millisecond // keep the test fast
	_, err := client.Get(context.Background(), server.URL)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("error = %v, want ErrBlocked", err)
	}
	if hits.Load() != int32(forbiddenRetryLimit) {
		t.Fatalf("a permanently-blocked host was tried %d times, want forbiddenRetryLimit (%d)", hits.Load(), forbiddenRetryLimit)
	}
}

// A 403 that clears on the very next attempt is what a momentarily
// rate-limited shared backend looks like — this must recover, not be
// reported as a permanent block.
func TestForbiddenRecoversOnRetry(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "recovered")
	}))
	defer server.Close()

	client := New(0)
	client.backoff = time.Millisecond
	body, err := client.Get(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "recovered" {
		t.Fatalf("body = %q", body)
	}
}

// A retryable 5xx before a 403 must not eat into the 403's own retry budget:
// this is what an overloaded shared backend can plausibly do (briefly 500ing,
// then answering 403 once whatever fronts it starts rate-limiting), and an
// earlier version of the forbidden-retry check counted both against the same
// attempt index, so a 403 immediately following any earlier attempt — 5xx or
// otherwise — was reported as ErrBlocked having been retried zero times.
func TestForbiddenIsStillRetriedAfterAPrecedingServerError(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch hits.Add(1) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			w.WriteHeader(http.StatusForbidden)
		default:
			_, _ = io.WriteString(w, "recovered")
		}
	}))
	defer server.Close()

	client := New(0)
	client.backoff = time.Millisecond
	body, err := client.Get(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("a 403 preceded by a 5xx should still get its own retry: %v", err)
	}
	if string(body) != "recovered" {
		t.Fatalf("body = %q", body)
	}
	if hits.Load() != 3 {
		t.Fatalf("got %d requests, want 3 (the 5xx, the 403, and the retry that recovers)", hits.Load())
	}
}

// A challenge page can arrive with HTTP 200 where JSON was expected. Decoding
// it produces a confusing syntax error rather than the real cause.
func TestJSONChallengePageBecomesBlocked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = io.WriteString(w, "<!DOCTYPE html><html><head><title>Just a moment...</title></head><body></body></html>")
	}))
	defer server.Close()

	var out map[string]any
	_, err := New(0).GetJSON(context.Background(), server.URL, &out)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("error = %v, want ErrBlocked", err)
	}
}

func TestMissingAPIBecomesErrNoAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"rest_no_route","message":"No route was found matching the URL"}`)
	}))
	defer server.Close()

	var out map[string]any
	_, err := New(0).GetJSON(context.Background(), server.URL, &out)
	if !errors.Is(err, ErrNoAPI) {
		t.Fatalf("error = %v, want ErrNoAPI", err)
	}
}

func TestServerErrorsAreRetriedThenReported(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(0)
	client.backoff = time.Millisecond // keep the test fast
	_, err := client.Get(context.Background(), server.URL)
	if err == nil {
		t.Fatal("expected an error")
	}
	var statusErr StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusInternalServerError {
		t.Fatalf("error = %v, want a 500 StatusError", err)
	}
	if hits.Load() != int32(MaxAttempts) {
		t.Fatalf("server was tried %d times, want %d", hits.Load(), MaxAttempts)
	}
}

// A host's error page can run to kilobytes of doctype, meta tags and inline
// scripts, none of which belongs in a one-line agent JSON message. Error()
// must replace an HTML body with a short marker; a genuinely informative
// plain-text body is left alone.
func TestStatusErrorOmitsHTMLBodyButKeepsPlainText(t *testing.T) {
	htmlErr := StatusError{URL: "https://books.test/missing/", Status: http.StatusNotFound,
		Body: `<!doctype html><html><head><title>Page not found</title></head><body><script>x()</script></body></html>`}
	if strings.Contains(htmlErr.Error(), "<html") || strings.Contains(htmlErr.Error(), "<script") {
		t.Fatalf("Error() leaked the HTML body: %s", htmlErr.Error())
	}
	if !strings.Contains(htmlErr.Error(), "404") {
		t.Fatalf("Error() lost the status code: %s", htmlErr.Error())
	}

	textErr := StatusError{URL: "https://books.test/api/", Status: http.StatusBadRequest,
		Body: `{"code":"rest_invalid_param","message":"Invalid parameter(s): per_page"}`}
	if !strings.Contains(textErr.Error(), "rest_invalid_param") {
		t.Fatalf("Error() dropped a genuinely informative plain-text body: %s", textErr.Error())
	}
}

func TestTransientFailureSucceedsOnRetry(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "recovered")
	}))
	defer server.Close()

	client := New(0)
	client.backoff = time.Millisecond
	body, err := client.Get(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "recovered" {
		t.Fatalf("body = %q", body)
	}
}

// Two bundled networks are reachable only through a redirect.
func TestRedirectsAreFollowed(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "arrived")
	}))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusMovedPermanently)
	}))
	defer redirector.Close()

	body, err := New(0).Get(context.Background(), redirector.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "arrived" {
		t.Fatalf("body = %q", body)
	}
}

// A book export runs to hundreds of megabytes, so the transfer must not inherit
// a whole-request deadline that a buffered read would need.
func TestDownloadStreamsPastTheRequestTimeout(t *testing.T) {
	const chunks = 8
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="A-Book-1507053296.pdf"`)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		for i := 0; i < chunks; i++ {
			_, _ = io.WriteString(w, "0123456789")
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer server.Close()

	var got bytes.Buffer
	written, filename, err := New(50*time.Millisecond).Download(context.Background(), server.URL, &got)
	if err != nil {
		t.Fatalf("stream download: %v", err)
	}
	if written != int64(chunks*10) || got.Len() != chunks*10 {
		t.Fatalf("wrote %d bytes, buffered %d", written, got.Len())
	}
	if filename != "A-Book-1507053296.pdf" {
		t.Fatalf("filename = %q", filename)
	}
}

func TestHeadReportsStatusWithoutABody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		if r.URL.Query().Get("type") == "odt" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
	}))
	defer server.Close()

	client := New(0)
	if status, err := client.Head(context.Background(), server.URL+"?type=pdf"); err != nil || status != 200 {
		t.Fatalf("Head(pdf) = (%d, %v)", status, err)
	}
	if status, err := client.Head(context.Background(), server.URL+"?type=odt"); err != nil || status != 500 {
		t.Fatalf("Head(odt) = (%d, %v)", status, err)
	}
}

// A body that decodes as JSON is never a challenge page, even mislabeled. An
// earlier version of GetJSON checked Content-Type before attempting to
// decode, so a good host whose own server mislabels a JSON response as
// text/html was permanently and incorrectly blocked.
func TestJSONMislabeledAsHTMLStillDecodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	var out map[string]any
	_, err := New(0).GetJSON(context.Background(), server.URL, &out)
	if err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("out = %v, want {\"ok\":true}", out)
	}
}

// Download only has status codes to go on before this test's fix; a 200
// challenge page for a pdf export would be streamed to disk as if it were the
// book. It must be rejected, and it must not have written anything first.
func TestDownloadRejectsAChallengePage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = io.WriteString(w, "<!DOCTYPE html><html><head><title>Just a moment...</title></head><body></body></html>")
	}))
	defer server.Close()

	var got bytes.Buffer
	_, _, err := New(0).Download(context.Background(), server.URL+"?type=pdf", &got)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("error = %v, want ErrBlocked", err)
	}
	if got.Len() != 0 {
		t.Fatalf("wrote %d bytes, want 0", got.Len())
	}
}

// xhtml and htmlbook are the two export kinds whose legitimate output is
// itself HTML-shaped, so requesting either must not trip the same
// challenge-page rejection that a pdf or epub response would.
func TestDownloadAllowsGenuineHTMLShapedExports(t *testing.T) {
	const body = "<!DOCTYPE html><html><body><p>Chapter One</p></body></html>"
	for _, kind := range []string{"xhtml", "htmlbook"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=UTF-8")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()

			var got bytes.Buffer
			written, _, err := New(0).Download(context.Background(), server.URL+"?type="+kind, &got)
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			if written != int64(len(body)) || got.String() != body {
				t.Fatalf("wrote %d bytes = %q, want %d bytes = %q", written, got.String(), len(body), body)
			}
		})
	}
}

// mime.ParseMediaType does no path sanitization: a hostile or merely broken
// host can send a traversal, an absolute path, or a bare "..", and a caller
// that joins the result onto an output directory would write wherever the
// server said to.
func TestFilenameFromSanitizesHostileDispositions(t *testing.T) {
	tests := []struct {
		name        string
		disposition string
		want        string
	}{
		{"ordinary", `attachment; filename="A-Book-1507053296.pdf"`, "A-Book-1507053296.pdf"},
		{"relative traversal", `attachment; filename="../../../../tmp/pwned.pdf"`, "pwned.pdf"},
		{"absolute path", `attachment; filename="/etc/cron.d/evil"`, "evil"},
		{"bare dot-dot", `attachment; filename=".."`, ""},
		{"rfc5987 encoded traversal", `attachment; filename*=UTF-8''..%2F..%2Fescape.pdf`, "escape.pdf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := filenameFrom(tt.disposition); got != tt.want {
				t.Fatalf("filenameFrom(%q) = %q, want %q", tt.disposition, got, tt.want)
			}
		})
	}
}

// TestRetryAfterIsHonored covers the server's own backoff instruction. These
// hosts throttle with a Retry-After header saying how long to wait; ignoring
// it and guessing with exponential backoff means retrying too early (wasting
// a request against a server that already said no) or too late. The header is
// the one piece of authoritative timing information the host gives us.
func TestRetryAfterIsHonored(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "1") // seconds
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	// Far shorter than the header's 1s, so honoring the header is the only
	// way the elapsed time can exceed it.
	client.backoff = time.Millisecond

	start := time.Now()
	_, err := client.Get(context.Background(), server.URL)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Get after a Retry-After 429: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	if elapsed < time.Second {
		t.Fatalf("waited %v, want at least the 1s the server asked for — Retry-After was ignored in favour of the %v backoff", elapsed, client.backoff)
	}
}

// TestRetryAfterBeyondTheCapFailsFastInsteadOfSleeping pins the other half.
// A host may ask for minutes. Blocking a CLI invocation that long is worse
// than failing, so past the cap the wait is refused and the error returned
// immediately, leaving the caller to decide when to come back.
func TestRetryAfterBeyondTheCapFailsFastInsteadOfSleeping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := New(0)
	client.baseScheme = "http"
	client.backoff = time.Millisecond

	start := time.Now()
	_, err := client.Get(context.Background(), server.URL)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the request to fail rather than wait an hour")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("slept %v; a Retry-After beyond the cap must not be waited out", elapsed)
	}
}
