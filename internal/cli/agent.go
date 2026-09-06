package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
)

const agentSchemaVersion = "1"

type agentEnvelope struct {
	SchemaVersion string             `json:"schema_version"`
	OK            bool               `json:"ok"`
	Data          any                `json:"data,omitempty"`
	Meta          *agentMeta         `json:"meta,omitempty"`
	Error         *agentErrorPayload `json:"error,omitempty"`
}

type agentMeta struct {
	Command    string `json:"command,omitempty"`
	Count      int    `json:"count"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	Total      int    `json:"total"`
	HasMore    bool   `json:"has_more"`
	NextOffset *int   `json:"next_offset,omitempty"`
	// SkippedNetworks names each network a sweep could not read, with the
	// classified reason why (see skippedNetwork in search.go): a user must be
	// able to tell "this host is blocked" from "we hammered it" from "it timed
	// out" rather than an unexplained gap in the result.
	SkippedNetworks []skippedNetwork `json:"skipped_networks,omitempty"`
}

type agentErrorPayload struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
	Retryable  bool   `json:"retryable"`
}

type agentInputError struct {
	Code       string
	Message    string
	Suggestion string
}

func (e *agentInputError) Error() string { return e.Message }

type reportedError struct{ err error }

func (e *reportedError) Error() string { return e.err.Error() }
func (e *reportedError) Unwrap() error { return e.err }

func ErrorAlreadyReported(err error) bool {
	var reported *reportedError
	return errors.As(err, &reported)
}

type agentCapability struct {
	Name       string   `json:"name"`
	Purpose    string   `json:"purpose"`
	Usage      string   `json:"usage"`
	Returns    string   `json:"returns"`
	SideEffect string   `json:"side_effect,omitempty"`
	Defaults   []string `json:"defaults,omitempty"`
}

// agentErrorCodeDoc names one error code the tool can emit, with a one-line
// meaning and whether a caller should retry. This is currentAgentSchema's
// own list of every code classifyAgentError and the commands' agentInputError
// values can produce — the single source every documentation artifact
// (README, SKILL.md, spec.yaml, tools-manifest.json) is meant to be
// generated from, rather than each hand-listing its own partial, drifting
// subset. See TestErrorCodesCoverEveryEmittedCode for the guard that keeps
// this list itself honest.
type agentErrorCodeDoc struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Meaning   string `json:"meaning"`
}

type agentSchema struct {
	AgentFlag  string              `json:"agent_flag"`
	Rules      []string            `json:"rules"`
	Commands   []agentCapability   `json:"commands"`
	ErrorCodes []agentErrorCodeDoc `json:"error_codes"`
}

func currentAgentSchema() agentSchema {
	return agentSchema{
		AgentFlag: "--agent",
		Rules: []string{
			"Every response is one compact JSON object on stdout, including errors.",
			"Follow next_offset or next_start_char when present.",
			"Reuse returned book URLs and page ids exactly.",
			"Use download or extract --dir only when a file is explicitly requested.",
		},
		Commands: []agentCapability{
			{Name: "schema", Purpose: "Describe this contract.", Usage: "schema --agent", Returns: "the command contract"},
			{Name: "networks", Purpose: "List the bundled Pressbooks networks.", Usage: "networks [--include-excluded] [--limit N] [--offset N] --agent", Returns: "host, name and probed book count", Defaults: []string{"limit=26"}},
			{Name: "doctor", Purpose: "Check network reachability.", Usage: "doctor [--host HOST] --agent", Returns: "per-host status and live book count"},
			{Name: "books", Purpose: "List one network's books.", Usage: "books --host HOST [--limit N] [--offset N] --agent", Returns: "compact book summaries", Defaults: []string{"limit=10"}},
			{Name: "search", Purpose: "Find books whose TITLE, SUBTITLE or AUTHOR matches the query, across all bundled networks or one with --host. Does not search chapter titles or book text: a subject that appears only inside a book will return nothing. Search for the book, then use toc to find the chapter.", Usage: "search QUERY [--host HOST] [--limit N] [--offset N] --agent", Returns: "compact book summaries", Defaults: []string{"limit=5"}},
			{Name: "info", Purpose: "Get one book's metadata, license and available export formats.", Usage: "info BOOK [--no-exports] --agent", Returns: "book metadata, license, page count and export formats"},
			{Name: "toc", Purpose: "List a book's extractable pages.", Usage: "toc BOOK [--limit N] [--offset N] --agent", Returns: "compact page summaries", Defaults: []string{"limit=20"}},
			{Name: "extract", Purpose: "Read one page or a bounded page range.", Usage: "extract BOOK [--page PAGE | --all] [--limit N] [--offset N] [--max-chars N] [--start-char N] [--dir DIR] [--delay MS] --agent", Returns: "page text with continuation metadata", Defaults: []string{"limit=1", "max-chars=12000", "delay=100"}},
			{Name: "download", Purpose: "Write a publisher export or extracted content to a file.", Usage: "download BOOK --output PATH [--kind KIND] [--page PAGE] --agent", Returns: "written path, kind and byte count", SideEffect: "writes_file", Defaults: []string{"kind=pdf", "whole book unless --page"}},
		},
		ErrorCodes: []agentErrorCodeDoc{
			{Code: "book_not_found", Meaning: "The URL is not a Pressbooks book, or no such book exists at that host."},
			{Code: "page_not_found", Meaning: "No such page in this book."},
			{Code: "network_not_found", Meaning: "--host names a host with no Pressbooks v2 API."},
			{Code: "blocked_by_host", Retryable: true, Meaning: "The host is refusing this client. Usually temporary rate limiting, which clears on its own; occasionally a permanent block. Wait before retrying, and give up after a few attempts rather than immediately."},
			{Code: "export_unavailable", Meaning: "This book offers no such export format."},
			{Code: "invalid_arguments", Meaning: "Conflicting, malformed, or unrecognized flags or subcommand."},
			{Code: "unknown_command", Meaning: "The subcommand named is not one this tool has."},
			{Code: "timeout", Retryable: true, Meaning: "Deadline exceeded."},
			{Code: "rate_limited", Retryable: true, Meaning: "HTTP 429 from the host."},
			{Code: "upstream_error", Retryable: true, Meaning: "HTTP 5xx from the host."},
			{Code: "network_error", Retryable: true, Meaning: "A transport failure (DNS, connection, TLS) reaching the host."},
			{Code: "file_error", Meaning: "Could not write the output path."},
			{Code: "invalid_offset", Meaning: "--offset must be zero or greater."},
			{Code: "invalid_limit", Meaning: "--limit must be zero or greater."},
			{Code: "limit_too_large", Meaning: "--limit exceeds this command's agent maximum."},
			{Code: "output_required", Meaning: "Agent mode requires an explicit --output or --dir path before writing a file."},
			{Code: "invalid_download_kind", Meaning: "--kind is not a supported export or extraction kind."},
			{Code: "conflicting_page_selection", Meaning: "--page and --all were both given."},
			{Code: "conflicting_text_offset", Meaning: "--start-char and --all were both given."},
			{Code: "invalid_start_char", Meaning: "--start-char must be zero or greater."},
			{Code: "start_char_out_of_range", Meaning: "--start-char exceeds the page's total character count."},
			{Code: "invalid_max_chars", Meaning: "--max-chars must be at least 1."},
			{Code: "max_chars_too_large", Meaning: "--max-chars exceeds the agent maximum of 50000."},
			{Code: "invalid_format", Meaning: "--format is not text, json, or html."},
			{Code: "invalid_delay", Meaning: "--delay must be zero or greater."},
			{Code: "unsupported_agent_option", Meaning: "A flag (such as --include-html) was combined with --agent, which does not support it."},
			{Code: "operation_failed", Meaning: "An uncategorized failure; no more specific code applies."},
		},
	}
}

func writeCompactJSON(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}

func writeAgentSuccess(w io.Writer, command string, data any, meta *agentMeta) error {
	if meta != nil {
		meta.Command = command
	}
	return writeCompactJSON(w, agentEnvelope{
		SchemaVersion: agentSchemaVersion,
		OK:            true,
		Data:          data,
		Meta:          meta,
	})
}

func writeAgentError(w io.Writer, err error) error {
	payload := classifyAgentError(err)
	return writeCompactJSON(w, agentEnvelope{
		SchemaVersion: agentSchemaVersion,
		OK:            false,
		Error:         &payload,
	})
}

func classifyAgentError(err error) agentErrorPayload {
	var inputErr *agentInputError
	if errors.As(err, &inputErr) {
		return agentErrorPayload{Code: inputErr.Code, Message: inputErr.Message, Suggestion: inputErr.Suggestion}
	}

	message := err.Error()
	lower := strings.ToLower(message)
	payload := agentErrorPayload{Code: "operation_failed", Message: message}
	switch {
	case errors.Is(err, pressbooks.ErrBlocked):
		payload.Code = "blocked_by_host"
		payload.Suggestion = "This host is refusing this client right now, which can mean either a permanent block or temporary rate limiting from too many recent requests. Wait and retry before concluding the network is unavailable."
	case errors.Is(err, pressbooks.ErrNoAPI):
		payload.Code = "network_not_found"
		payload.Suggestion = "That host serves no Pressbooks v2 API. Run 'networks' for the hosts this tool can read."
	case errors.Is(err, pressbooks.ErrBookNotFound):
		payload.Code = "book_not_found"
		payload.Suggestion = "Run search with the same title, then reuse the returned book URL."
	case errors.Is(err, pressbooks.ErrPageNotFound):
		payload.Code = "page_not_found"
		payload.Suggestion = "Run toc for the book, then reuse a returned page id."
	case strings.Contains(lower, "unknown command"):
		payload.Code = "unknown_command"
		payload.Suggestion = "Run 'pressbooks-pp-cli schema --agent' and choose one listed command."
	case strings.Contains(lower, "unknown flag"), strings.Contains(lower, "accepts ") && strings.Contains(lower, "arg"):
		payload.Code = "invalid_arguments"
		payload.Suggestion = "Run 'pressbooks-pp-cli schema --agent' for the compact command contract."
	case strings.Contains(lower, "book ") && strings.Contains(lower, "not found"):
		payload.Code = "book_not_found"
		payload.Suggestion = "Run search with the same title, then reuse the returned book URL."
	case strings.Contains(lower, "page ") && strings.Contains(lower, "not found"):
		payload.Code = "page_not_found"
		payload.Suggestion = "Run toc for the book, then reuse a returned page id."
	case strings.Contains(lower, "export"):
		payload.Code = "export_unavailable"
		payload.Suggestion = "Run info for the book and choose one of the formats it lists."
	case errors.Is(err, context.DeadlineExceeded):
		payload.Code = "timeout"
		payload.Retryable = true
		payload.Suggestion = "Retry once or increase --timeout."
	case strings.Contains(lower, "http 429"):
		payload.Code = "rate_limited"
		payload.Retryable = true
		payload.Suggestion = "Wait before retrying."
	case strings.Contains(lower, "http 5"):
		payload.Code = "upstream_error"
		payload.Retryable = true
		payload.Suggestion = "Retry later."
	default:
		var netErr net.Error
		var pathErr *os.PathError
		if errors.As(err, &netErr) {
			payload.Code = "network_error"
			payload.Retryable = true
			payload.Suggestion = "Check connectivity with doctor, then retry."
		} else if errors.As(err, &pathErr) {
			payload.Code = "file_error"
			payload.Suggestion = "Use an explicit writable path with --output."
		}
	}
	return payload
}

// agentWindowBounds validates the offset/limit bounds that do not depend on
// a result's total — a negative offset, a negative limit, or a limit above
// the agent maximum — and resolves an unset (zero) limit to defaultLimit.
// Splitting this out of agentWindow lets a command check these bounds
// before it pays for a network request that would compute the total: a
// cold, all-network sweep is roughly 974 requests, so a caller's typo in
// --limit must not be discovered only after all of them have already run.
func agentWindowBounds(offset, limit, defaultLimit, maxLimit int) (int, error) {
	if offset < 0 {
		return 0, &agentInputError{Code: "invalid_offset", Message: "offset must be zero or greater", Suggestion: "Use --offset 0 for the first result page."}
	}
	if limit < 0 {
		return 0, &agentInputError{Code: "invalid_limit", Message: "limit must be zero or greater", Suggestion: fmt.Sprintf("Omit --limit to use the default of %d.", defaultLimit)}
	}
	if limit == 0 {
		limit = defaultLimit
	}
	if maxLimit > 0 && limit > maxLimit {
		return 0, &agentInputError{Code: "limit_too_large", Message: fmt.Sprintf("limit %d exceeds the agent maximum of %d", limit, maxLimit), Suggestion: fmt.Sprintf("Use --limit %d or less and follow next_offset.", maxLimit)}
	}
	return limit, nil
}

func agentWindow(total, offset, limit, defaultLimit, maxLimit int) (int, int, agentMeta, error) {
	limit, err := agentWindowBounds(offset, limit, defaultLimit, maxLimit)
	if err != nil {
		return 0, 0, agentMeta{}, err
	}
	start := offset
	if start > total {
		start = total
	}
	end := total
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	meta := agentMeta{Count: end - start, Offset: start, Limit: limit, Total: total, HasMore: end < total}
	if meta.HasMore {
		next := end
		meta.NextOffset = &next
	}
	return start, end, meta, nil
}

// argsRequestAgent is a fallback for the case where flag parsing itself failed
// and the parsed flag value is therefore unset. pflag accepts every strconv
// boolean spelling, so match on the parsed value rather than on "--agent=true".
func argsRequestAgent(args []string) bool {
	for _, arg := range args {
		if arg == "--agent" {
			return true
		}
		if value, ok := strings.CutPrefix(arg, "--agent="); ok {
			if enabled, err := strconv.ParseBool(value); err == nil && enabled {
				return true
			}
		}
	}
	return false
}

func executeArgs(args []string, stdout, stderr io.Writer) error {
	return executeArgsWithClient(args, stdout, stderr, func(timeout time.Duration) pressbooksClient {
		return pressbooks.New(timeout)
	})
}

func executeArgsWithClient(args []string, stdout, stderr io.Writer, factory clientFactory) error {
	root, f := rootCmdWithClient(factory)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil || (!f.agent && !argsRequestAgent(args)) {
		return err
	}
	// The envelope goes to stdout, alongside successful responses, so that a
	// harness capturing only stdout still receives a structured failure instead
	// of nothing. Fall back to stderr if stdout is unusable, rather than losing
	// the error entirely.
	if writeErr := writeAgentError(stdout, err); writeErr != nil {
		if fallbackErr := writeAgentError(stderr, err); fallbackErr != nil {
			return fmt.Errorf("%w; report agent error: %v", err, writeErr)
		}
	}
	return &reportedError{err: err}
}
