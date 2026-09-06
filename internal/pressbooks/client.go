package pressbooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// UserAgent is browser-shaped because these hosts answer a bare tool name
	// with 403, and identifying, with a contact URL, because pretending to be
	// someone's browser would be worse.
	UserAgent = "Mozilla/5.0 (compatible; pressbooks-pp-cli/0.1; +https://github.com/johnnylibretexts/pressbooks-cli)"

	// MaxAttempts counts the first try, not just the retries.
	MaxAttempts = 3
	// BaseBackoff is the wait before the second attempt; each further wait doubles.
	BaseBackoff = 500 * time.Millisecond
	// maxRetryAfter caps how long a host's own Retry-After header can make
	// this client wait. These hosts sometimes ask for minutes; blocking a CLI
	// invocation that long is worse than failing, so past this the wait is
	// refused and the error surfaces immediately, leaving the caller to decide
	// when to come back.
	maxRetryAfter = 30 * time.Second

	// forbiddenRetryLimit is how many times a 403 is tried before being
	// reported as ErrBlocked. Deliberately smaller than MaxAttempts: a
	// single 403 is not proof a host refuses automated clients at all — a
	// shared backend under momentary rate-limit pressure (this tool's own
	// doing, or someone else's) answers 403 too, and typically recovers
	// within one retry. A host that is truly Cloudflare-fronted, such as
	// pressbooks.pub itself, will still answer 403 on the retry and end up
	// classified exactly as before.
	forbiddenRetryLimit = 2
)

// ErrBlocked means a 403 survived forbiddenRetryLimit attempts: the host
// refuses automated clients, or is rate-limiting them for long enough that
// this retry budget could not tell the difference. It is treated as
// permanent for the purposes of this crawl — no further retry here turns a
// challenge page into JSON — but is not proof the host will refuse forever.
var ErrBlocked = errors.New("host is refusing automated requests")

// ErrNoAPI means the host answered, but serves no Pressbooks v2 API.
var ErrNoAPI = errors.New("host serves no Pressbooks v2 API")

// ErrBookNotFound means a book's own API endpoints (metadata, toc) answered
// 404: most often because the book's slug does not exist on that host. It is
// a typed sentinel, not a substring match, precisely because a host's own
// 404 page is free to say almost anything — including, on at least one real
// host, an HTML page titled "Page not found" for a missing *book*, which
// would otherwise be misread by classifyAgentError as a missing page.
var ErrBookNotFound = errors.New("book not found")

// ErrPageNotFound means a specific page within an otherwise-found book could
// not be located. Nothing in this package returns it yet — no page-content
// fetch exists until a later task adds one — but it is defined alongside
// ErrBookNotFound so that task can wrap a 404 the same typed way from day
// one, rather than reintroducing the substring fragility this sentinel
// exists to avoid.
var ErrPageNotFound = errors.New("page not found")

// StatusError is a response the client gave up on.
type StatusError struct {
	URL    string
	Status int
	Body   string
}

func (e StatusError) Error() string {
	body := e.Body
	// A host's error page is free to say anything, and some run to
	// kilobytes of doctype, meta tags and inline scripts — none of it useful
	// to an agent parsing this message, and all of it noise compared to the
	// status code and URL. looksLikeHTML is the same sniff Client.do and
	// Download already use to recognize a challenge page; reused here rather
	// than duplicated. A genuinely informative plain-text body is left
	// exactly as it is.
	if looksLikeHTML("", []byte(body)) {
		body = "(HTML error page omitted)"
	}
	if body == "" {
		return fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Status)
	}
	return fmt.Sprintf("GET %s: HTTP %d: %s", e.URL, e.Status, body)
}

type Client struct {
	httpClient *http.Client
	// downloadClient has no whole-request deadline. Book exports run to
	// hundreds of megabytes and http.Client.Timeout covers the body read, so a
	// shared client would abort a large transfer partway through. A response
	// header timeout still fails fast when the server itself is unresponsive.
	downloadClient *http.Client
	cacheTTL       time.Duration
	backoff        time.Duration
	// baseScheme is https everywhere except in tests, which serve plain HTTP
	// from an httptest server and set this to "http".
	baseScheme string
}

func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	return &Client{
		httpClient:     &http.Client{Timeout: timeout},
		downloadClient: &http.Client{Transport: transport},
		cacheTTL:       DefaultCacheTTL,
		backoff:        BaseBackoff,
	}
}

// SetCacheTTL bounds how long a cached catalog may be reused. Zero disables
// reading from the cache, which is what --no-cache and doctor need.
func (c *Client) SetCacheTTL(ttl time.Duration) { c.cacheTTL = ttl }

func (c *Client) do(ctx context.Context, method, rawURL, accept string) (*http.Response, error) {
	var lastErr error
	// forbidden counts 403 responses specifically, separately from the loop's
	// general attempt index. forbiddenRetryLimit is meant to give a 403 itself
	// one retry regardless of what came before it in this call — a retryable
	// 5xx on an earlier attempt must not eat into that budget. Checking
	// attempt+1 against forbiddenRetryLimit, as an earlier version of this
	// method did, conflated the two: a 5xx on attempt 0 followed by a 403 on
	// attempt 1 hit attempt+1 == forbiddenRetryLimit on the very first 403 and
	// reported ErrBlocked having retried it zero times — exactly the
	// misbehavior this limit exists to prevent, and exactly what an
	// overloaded shared backend can plausibly do (a 5xx while it's briefly
	// down, then a 403 once whatever fronts it starts rate-limiting).
	var forbidden int
	for attempt := 0; attempt < MaxAttempts; attempt++ {
		wait := c.backoff * (1 << attempt)
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("Accept", accept)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if attempt+1 >= MaxAttempts {
				return nil, err
			}
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		} else {
			status := resp.StatusCode
			// Read before closing: the host's own backoff instruction is the
			// only authoritative timing information it gives us, and guessing
			// with exponential backoff instead means retrying either too
			// early — spending a request on a server that already said no —
			// or needlessly late.
			serverWait, serverAsked := retryAfter(resp.Header.Get("Retry-After"))
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			_ = resp.Body.Close()
			if serverAsked && serverWait > maxRetryAfter {
				if status == http.StatusForbidden {
					return nil, fmt.Errorf("%s: %w", rawURL, ErrBlocked)
				}
				return nil, statusErrorFor(rawURL, status, body)
			}
			if serverAsked {
				wait = serverWait
			}
			statusErr := StatusError{URL: rawURL, Status: status, Body: strings.TrimSpace(string(body))}

			if status == http.StatusNotFound && strings.Contains(string(body), "rest_no_route") {
				return nil, fmt.Errorf("%s: %w", rawURL, ErrNoAPI)
			}
			if status == http.StatusForbidden {
				forbidden++
				lastErr = fmt.Errorf("%s: %w", rawURL, ErrBlocked)
				if forbidden >= forbiddenRetryLimit || attempt+1 >= MaxAttempts {
					return nil, lastErr
				}
			} else {
				lastErr = statusErr
				if !retryableStatus(status) || attempt+1 >= MaxAttempts {
					return nil, statusErr
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

// retryAfter parses the Retry-After header, which RFC 9110 allows in either
// form: a count of seconds, or an HTTP date. A malformed or absent value is
// reported as absent rather than as zero, so the caller keeps its own backoff
// instead of retrying instantly. A date already in the past yields zero, which
// is the server saying "now".
func retryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func statusErrorFor(rawURL string, status int, body []byte) error {
	return StatusError{URL: rawURL, Status: status, Body: strings.TrimSpace(string(body))}
}

// retryableStatus is true when the server asked us to slow down or broke.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func (c *Client) Get(ctx context.Context, rawURL string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, rawURL, "application/json, text/html;q=0.9, */*;q=0.8")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// GetJSON decodes a JSON response and returns its headers, which is how the
// catalog crawl reads X-WP-Total.
func (c *Client) GetJSON(ctx context.Context, rawURL string, v any) (http.Header, error) {
	resp, err := c.do(ctx, http.MethodGet, rawURL, "application/json, */*;q=0.8")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// Decode before classifying. A body that decodes as JSON is never a
	// challenge page, even when a mangled server header mislabels it
	// text/html — checking shape first, as an earlier version of this method
	// did, would turn that mislabel into a permanent, non-retryable
	// ErrBlocked and discard a perfectly good response. Only a body that
	// fails to decode is checked for the challenge shape, so the confusing
	// JSON syntax error a real challenge page produces is still replaced with
	// the real cause.
	if err := json.Unmarshal(body, v); err != nil {
		if looksLikeHTML(resp.Header.Get("Content-Type"), body) {
			return nil, fmt.Errorf("%s: %w", rawURL, ErrBlocked)
		}
		return nil, fmt.Errorf("decode %s: %w", rawURL, err)
	}
	return resp.Header, nil
}

func looksLikeHTML(contentType string, body []byte) bool {
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil && mediaType == "text/html" {
		return true
	}
	trimmed := bytes.TrimSpace(body)
	return bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<!doctype html")) ||
		bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<html"))
}

// Head reports a status without transferring a body. Export availability is
// probed this way: a format a book offers answers 200, one it does not
// answers 500. A 403 is different from either: it means the host is refusing
// this client altogether, not answering honestly about this one format, so it
// is reported as ErrBlocked rather than folded into "not offered" the way a
// bare non-200 status otherwise would be. Without this, a rate-limited or
// blocked host answering every export probe with 403 was indistinguishable
// from a book that genuinely offers nothing.
func (c *Client) Head(ctx context.Context, rawURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, fmt.Errorf("%s: %w", rawURL, ErrBlocked)
	}
	return resp.StatusCode, nil
}

// Download streams rawURL into w rather than buffering it, and returns the
// bytes written and the filename the server suggested.
func (c *Client) Download(ctx context.Context, rawURL string, w io.Writer) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := c.downloadClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		return 0, "", fmt.Errorf("%s: %w", rawURL, ErrBlocked)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, "", StatusError{URL: rawURL, Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	body := io.Reader(resp.Body)
	// A 200 challenge page is indistinguishable from a real export by status
	// alone. The xhtml export is itself genuinely HTML-shaped, so only a
	// response to a request for some other kind is second-guessed; sniffing
	// a bounded prefix, rather than buffering the whole response, keeps a
	// hundred-megabyte export streaming.
	if !expectsHTMLExport(rawURL) {
		peek := make([]byte, htmlSniffLen)
		n, _ := io.ReadFull(resp.Body, peek)
		peek = peek[:n]
		if looksLikeHTML(resp.Header.Get("Content-Type"), peek) {
			return 0, "", fmt.Errorf("%s: %w", rawURL, ErrBlocked)
		}
		body = io.MultiReader(bytes.NewReader(peek), resp.Body)
	}

	written, err := io.Copy(w, body)
	return written, filenameFrom(resp.Header.Get("Content-Disposition")), err
}

// htmlSniffLen bounds how much of a response Download peeks before deciding
// whether it looks like a challenge page: enough to catch a doctype or an
// opening html tag, far smaller than any real export.
const htmlSniffLen = 512

// expectsHTMLExport reports whether rawURL requested an export kind that is
// itself HTML-shaped, per BookRef.ExportURL's "type" query parameter. This is
// the complete set of this tool's export kinds whose legitimate output is
// HTML: xhtml and htmlbook are, so a real response to either legitimately
// trips looksLikeHTML the same way a challenge page does. Every other kind —
// pdf, print_pdf, epub, mobi, odt, wxr — is not, so an HTML response to one of
// those is always a challenge page, never a legitimate export. This set must
// stay in step with the export kinds the tool offers: a kind whose output is
// HTML but that is missing here fails closed, reported as a blocked host
// rather than downloaded.
func expectsHTMLExport(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Query().Get("type")) {
	case "xhtml", "htmlbook":
		return true
	default:
		return false
	}
}

// filenameFrom returns the filename the server suggested, sanitized to a bare
// base name. mime.ParseMediaType does no path sanitization at all: a hostile
// or merely broken host can send "../../../../tmp/pwned.pdf", an absolute
// path, or an RFC 5987 percent-encoded traversal, and a caller that joins the
// result onto an output directory would write wherever the server said to.
// Returning "" for anything that is not a plain, visible file name lets the
// caller fall back to a name it derives itself, the same as a missing header.
func filenameFrom(disposition string) string {
	if disposition == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		return ""
	}
	name := filepath.Base(params["filename"])
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return ""
	}
	return name
}
