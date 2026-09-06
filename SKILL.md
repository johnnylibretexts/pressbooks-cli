---
name: pp-pressbooks
description: Search Pressbooks open textbook networks and retrieve a book's metadata, table of contents, page content, publisher exports, or complete extracted text, JSON, and HTML using pressbooks-pp-cli or pressbooks-cli.
---

# Pressbooks Printing Press CLI

Use `pressbooks-pp-cli` (or `pressbooks-cli`) for read-only access to public
Pressbooks networks: WordPress multisite installations that publish open
textbooks. It requires no account or API key.

## Setup

Verify the binary before use:

```bash
pressbooks-pp-cli --version
pressbooks-pp-cli schema --agent
pressbooks-pp-cli doctor --host milnepublishing.geneseo.edu --agent
```

From this repository, build with `make build` or install with `make
install`.

## Common Workflows

List the bundled networks, then search one (searching every bundled network
at once works too, but crawls hundreds of catalogs on first run — scope to
`--host` when the network is already known):

```bash
pressbooks-pp-cli networks --limit 10 --agent
pressbooks-pp-cli search "logic" --host milnepublishing.geneseo.edu --limit 5 --agent
```

Inspect a book and its extractable pages. `BOOK` accepts a full URL, a bare
`host/slug`, or any URL inside the book:

```bash
pressbooks-pp-cli info https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --agent
pressbooks-pp-cli toc https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --limit 20 --agent
```

Extract one page or continue a long one:

```bash
pressbooks-pp-cli extract BOOK --page PAGE --agent
pressbooks-pp-cli extract BOOK --page PAGE --start-char 12000 --agent
pressbooks-pp-cli extract BOOK --all --limit 1 --offset 0 --agent
```

Whole-book extraction (`extract --all`, and `download --kind text|json|html`,
which shares the same code path) paces its page fetches with `--delay`
(default 100ms); pass `--delay 0` only if you have a specific reason to run
unpaced.

Save a complete book or its publisher PDF (writing to disk requires an
explicit path even in agent mode):

```bash
pressbooks-pp-cli download BOOK --kind text --output book.txt --agent
pressbooks-pp-cli download BOOK --kind html --output book.html --agent
pressbooks-pp-cli download BOOK --kind pdf --output book.pdf --agent
```

Network catalogs are cached for seven days; pass `--no-cache` to force a
fresh crawl. Extracted image and link targets are made absolute against the
book host. Preserve the license and attribution returned by `info` when
reusing textbook content — it is the *book's* license, not this tool's, and
it varies book by book within a single network.

## Agent Contract

Use `--agent` for a single compact JSON envelope. Check `ok` before reading
`data`. When list metadata contains `has_more: true`, repeat the command
with `next_offset`. When extracted text contains `next_start_char`, repeat
the same page request with that value as `--start-char`.

Agent defaults are intentionally bounded: `networks` returns 26 of the 28
bundled networks (page or raise `--limit` for the rest), `search` returns 5,
`books` returns 10, `toc` returns 20, whole-book `extract --all` returns 1
page, and each extracted page returns at most 12,000 characters.

Do not combine `--start-char` with `--all`; it is a cursor into a single
page, so paginate whole-book extraction with `--offset` and continue
individual pages with `--page PAGE --start-char N`. Do not combine `--agent`
with `--include-html`. Use raw `--json` when complete upstream records or
HTML are required. `download --agent` and `extract --agent` with `--dir`
require an explicit output path, because they write files.

A search sweeping every bundled network reports partial failures in
`meta.skipped_networks` — an array of `{"host", "reason"}` objects, not bare
hostnames — so an empty result and a partial one are never confused.

Errors are compact JSON on stdout, the same stream as successful responses,
so **stdout alone always holds the result** — capture it separately and parse
it directly. Do not merge stderr into it with `2>&1`: progress lines such as
`crawling HOST (page N/M)` and the skipped-network summary go to stderr, and
merging them puts non-JSON text in front of the envelope. Errors return a
nonzero exit status and include `code`, `message`, `suggestion`, and
`retryable`. Parse stdout and branch on `ok` rather than on the exit status
alone.

`schema --agent` returns the complete, authoritative list of codes this tool
can emit as its `error_codes` array — every code, its `retryable` value, and
a one-line meaning. Do not rely on any hardcoded list of "for example" codes
in documentation (including this file); if you only branch on a fixed
subset, an unhandled code should still be treated as a non-retryable failure
by default unless `error_codes` says otherwise. As of this writing the codes
are: `book_not_found`, `page_not_found`, `network_not_found`,
`blocked_by_host`, `export_unavailable`, `invalid_arguments`,
`unknown_command`, `timeout`, `rate_limited`, `upstream_error`,
`network_error`, `file_error`, `invalid_offset`, `invalid_limit`,
`limit_too_large`, `output_required`, `invalid_download_kind`,
`conflicting_page_selection`, `conflicting_text_offset`,
`invalid_start_char`, `start_char_out_of_range`, `invalid_max_chars`,
`max_chars_too_large`, `invalid_format`, `invalid_delay`,
`unsupported_agent_option`, and `operation_failed`.
