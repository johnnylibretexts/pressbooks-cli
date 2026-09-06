# Pressbooks CLI

[![CI](https://github.com/johnnylibretexts/pressbooks-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/johnnylibretexts/pressbooks-cli/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/johnnylibretexts/pressbooks-cli.svg)](https://pkg.go.dev/github.com/johnnylibretexts/pressbooks-cli)
[![Go Report Card](https://goreportcard.com/badge/github.com/johnnylibretexts/pressbooks-cli)](https://goreportcard.com/report/github.com/johnnylibretexts/pressbooks-cli)
[![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Find and read open textbooks published on Pressbooks networks from the
command line: list the networks, search across them, walk a book's table of
contents, pull the full text of any page, or save an entire book as text,
JSON, HTML, or a publisher export (PDF, EPUB, and the rest).

Pressbooks is used by dozens of universities and open-education consortia to
publish free textbooks, and each installation exposes its own public REST
API. All of it is public, so **this tool needs no account, API key, or
token.**

## Why the API is awkward

Pressbooks is WordPress: every book is a WordPress site, every network is a
WordPress multisite, and the Pressbooks REST API v2 is exposed on each of
them. Four properties of the real service, each measured against live hosts,
mean this cannot be a thin API wrapper:

- **There is no directory API.** Nothing enumerates Pressbooks networks — the
  public directory answers an automated client with a block page. So the set
  of networks this tool can search ships bundled with it.
- **A network ignores its own search parameter.** The upstream `search` query
  parameter is accepted and discarded. Searching happens locally, which means
  a network has to be enumerated (and cached) before it can be searched.
- **A page holds ten books, and that is a hard cap.** `per_page` over 10 is
  refused. Crawling a large network is hundreds of requests, which is why
  catalogs are cached and crawled progress is reported.
- **These hosts reject an unrecognised client.** A bare tool name in the
  User-Agent draws an HTTP 403 block page from hosts that answer a
  browser-shaped one normally — which is why the bundled network list has
  more entries than a naive probe would find.

The bundled list ships 28 networks holding 9,601 books at last probe, and it
distinguishes networks that are usable from ones this tool tried and could
not reach — see [The network list](#the-network-list).

## Requirements

Go 1.26 or newer, and network access to whichever Pressbooks host you want to
read. No credentials.

## Install

```bash
go install github.com/johnnylibretexts/pressbooks-cli/cmd/pressbooks-pp-cli@latest
```

Or build from a clone:

```bash
make build && ./bin/pressbooks-pp-cli --version
```

The binary is named `pressbooks-pp-cli`. `make install` also links it as
`pressbooks-cli`; both names run the same tool. The `-pp-` marks it as a CLI
Printing Press tool, matching `openstax-pp-cli` and its siblings.

## Try it

List the largest bundled networks:

```console
$ pressbooks-pp-cli networks | head -5
HOST                             NAME                                                       BOOKS
ecampusontario.pressbooks.pub    eCampusOntario Open Authoring Platform                     3078
pressbooks.online.ucf.edu        University of Central Florida Pressbooks                   1939
pressbooks.bccampus.ca           British Columbia/Yukon Open Authoring Platform             862
uw.pressbooks.pub                University of Washington Libraries                         372
```

Search one network for a book (searching every bundled network at once is
also supported — see [Searching every network](#searching-every-network) —
but it crawls hundreds of catalogs on first run, so scope to `--host` when
you already know where to look):

```console
$ pressbooks-pp-cli search "logic" --host milnepublishing.geneseo.edu
TITLE                            HOST                         AUTHORS         LICENSE
A Concise Introduction to Logic  milnepublishing.geneseo.edu  Craig DeLancey  CC BY-NC-SA (Attribution NonCommercial ShareAlike)
```

Inspect it — note that `license` is the *book's* license, not this tool's:

```console
$ pressbooks-pp-cli info https://milnepublishing.geneseo.edu/concise-introduction-to-logic/
A Concise Introduction to Logic
url: https://milnepublishing.geneseo.edu/concise-introduction-to-logic/
authors: Craig DeLancey
license: CC BY-NC-SA (Attribution NonCommercial ShareAlike) (https://creativecommons.org/licenses/by-nc-sa/4.0/)
language: en
pages: 25
exports: pdf, print_pdf, epub, wxr
```

List the pages you can extract:

```console
$ pressbooks-pp-cli toc https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --flat | head -5
174	front-matter	about-the-textbook	About the Textbook
18	front-matter	reviewers-notes	Reviewer's Notes
196	front-matter	dedication	Dedication
19	front-matter	0-introduction	0. Introduction
21	chapters	1-developing-a-precise-language	1. Developing a Precise Language	(part: Part I: Propositional Logic)
```

Read one, by numeric id, slug, or full URL:

```console
$ pressbooks-pp-cli extract https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --page 1-developing-a-precise-language | head -6
# 1. Developing a Precise Language
https://milnepublishing.geneseo.edu/concise-introduction-to-logic/chapter/1-developing-a-precise-language/

1.1 Starting with sentences

We begin the study of logic by building a precise logical language. This will allow us to do at least two things: first, to say some things more precisely than we otherwise would be able to do; second, to study reasoning. We will use a natural language—English—as our guide, but our logical language will be far simpler, far weaker, but more rigorous than English.
```

Save the publisher's PDF, or just one chapter as text:

```console
$ pressbooks-pp-cli download https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --kind pdf -o logic.pdf
wrote logic.pdf (2307180 bytes)
$ pressbooks-pp-cli download https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --kind text --page 1-developing-a-precise-language -o chapter1.txt
wrote chapter1.txt (24407 bytes)
```

## Quick Start

```bash
pressbooks-pp-cli doctor --host milnepublishing.geneseo.edu
pressbooks-pp-cli search "logic" --host milnepublishing.geneseo.edu
pressbooks-pp-cli info https://milnepublishing.geneseo.edu/concise-introduction-to-logic/
pressbooks-pp-cli toc https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --flat
pressbooks-pp-cli extract https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --page 1-developing-a-precise-language
pressbooks-pp-cli extract https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --all --format json --include-html > logic.json
pressbooks-pp-cli download https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --kind pdf -o logic.pdf
pressbooks-pp-cli download https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --kind text --all -o logic.txt
pressbooks-pp-cli download https://milnepublishing.geneseo.edu/concise-introduction-to-logic/ --kind text --page 1-developing-a-precise-language -o chapter1.txt
```

`download` saves the whole book by default. Passing `--page` narrows it to a
single page; passing both `--page` and an explicit `--all` is an error.
Publisher exports are streamed straight to disk rather than buffered, so a
large book does not have to fit in memory, and `--timeout` bounds the
response headers rather than the transfer itself. A download that fails
partway through removes its partial file.

`BOOK` accepts a full URL (`https://host/slug/`), a bare host and slug
(`host/slug`), or any URL inside the book, including a page URL — the book
root is derived from it. A root-hosted, single-book install resolves to the
host itself. `info` and `toc` work against any Pressbooks host, bundled or
not; the bundled list only governs which networks `search` and `books`
crawl.

## Commands

- `schema` describes the compact, versioned agent contract without a network
  request.
- `networks` lists the bundled networks and their probed book counts. Add
  `--include-excluded` for hosts this tool tried and could not reach.
- `doctor [--host HOST]` checks live reachability, one network or all of
  them.
- `books --host HOST` lists a network's catalog, crawling and caching it if
  needed.
- `search QUERY [--host HOST]` searches book titles, subtitles, and authors,
  locally, across one network or every bundled one. **It does not search
  chapter titles or book text.** Pressbooks offers no full-text search API, so
  a subject that appears only inside a book returns nothing: search for the
  book, then use `toc` to find the chapter. Searching "logical fallacies"
  finds no book called that, even though chapters on the topic exist.
- `info BOOK` shows metadata, license, page count, and available export
  formats.
- `toc BOOK [--flat]` prints the table of contents; `--flat` lists only
  extractable pages.
- `extract BOOK [--page P | --all] [--delay MS]` extracts one page or the
  whole book as `text`, `json`, or `html`. `--delay` (default 100ms) paces
  requests between pages during whole-book extraction; `--delay 0` disables
  pacing. `download --kind text|json|html` shares this pacing at the same
  default, since it walks a book the same way.
- `download BOOK --kind KIND` saves a publisher export or extracted content
  to disk.

## Searching every network

Run without `--host`, `search` crawls every bundled network's catalog the
first time — roughly a thousand requests across 28 networks, which takes
tens of seconds. Progress goes to **stderr**, so a script capturing stdout
still gets exactly one line of output (or, in agent mode, one line of JSON).
Subsequent searches are answered from the cache and cost nothing until it
expires.

A network that fails mid-crawl does not fail the whole search: it is skipped,
named in a stderr warning, and listed in the response's
`meta.skipped_networks` — an array of `{"host": ..., "reason": ...}` objects,
not bare hostnames — so a caller can tell a genuinely empty result from a
partial one.

## Agent Mode

`--json` preserves the complete upstream-shaped JSON for scripts and
inspection. `--agent` uses a smaller, versioned contract for fast,
lower-capability tool-using models; it implies `--json`.

Every agent response is a single line, so a harness can read one line and
parse it:

```console
$ pressbooks-pp-cli search "logic" --host milnepublishing.geneseo.edu --limit 3 --agent
{"schema_version":"1","ok":true,"data":[{"url":"https://milnepublishing.geneseo.edu/concise-introduction-to-logic/","title":"A Concise Introduction to Logic","authors":["Craig DeLancey"],"license_name":"CC BY-NC-SA (Attribution NonCommercial ShareAlike)","host":"milnepublishing.geneseo.edu","network_name":"Milne Publishing","word_count":68574}],"meta":{"command":"search","count":1,"offset":0,"limit":3,"total":1,"has_more":false}}
```

Discover the contract without a network request:

```bash
pressbooks-pp-cli schema --agent
```

Agent responses are one compact JSON object with `schema_version`, `ok`, and
either `data` or `error`. Lists include pagination metadata; follow
`next_offset` when `has_more` is true, and follow `next_start_char` to
continue a long page:

```bash
pressbooks-pp-cli search "logic" --limit 5 --agent
pressbooks-pp-cli extract BOOK --page PAGE --agent
pressbooks-pp-cli extract BOOK --page PAGE --start-char 12000 --agent
```

`next_start_char` is a cursor into one page, so `--start-char` cannot be
combined with `--all`; paginate whole-book extraction with `--offset`
instead. Agent defaults are intentionally bounded: `networks` returns 26 of
the 28 bundled networks by default (raise `--limit` or page with
`--offset` to see the rest), `search` returns 5, `books` returns 10, `toc`
returns 20, whole-book `extract --all` returns 1 page, and each extracted
page returns at most 12,000 characters.

Agent mode intentionally excludes raw HTML; use `--json --include-html` when
complete HTML is required. Because `download` writes to the filesystem,
agent mode requires an explicit `--output` path (or `--dir` for
`extract --agent`) and returns the path, kind, and byte count after writing.

Agent errors are written to stdout alongside successful responses, so a
harness that captures only stdout still receives a structured failure; the
exit status is still nonzero. Errors carry a stable `code`, a human
`message`, a `suggestion` for what to do next, and whether the error is
`retryable`. The complete, authoritative list of codes this tool can emit —
regenerated from the same source `schema --agent`'s own `error_codes` array
comes from, so it cannot silently drift out of date — is:

| Code | Retryable | Meaning |
|---|---|---|
| `book_not_found` | no | The URL is not a Pressbooks book, or no such book exists at that host. |
| `page_not_found` | no | No such page in this book. |
| `network_not_found` | no | `--host` names a host with no Pressbooks v2 API. |
| `blocked_by_host` | yes | The host is refusing this client. Usually temporary rate limiting, which clears on its own; occasionally a permanent block. Wait before retrying, and give up after a few attempts rather than immediately. |
| `export_unavailable` | no | This book offers no such export format. |
| `invalid_arguments` | no | Conflicting, malformed, or unrecognized flags or subcommand. |
| `unknown_command` | no | The subcommand named is not one this tool has. |
| `timeout` | yes | Deadline exceeded. |
| `rate_limited` | yes | HTTP 429 from the host. |
| `upstream_error` | yes | HTTP 5xx from the host. |
| `network_error` | yes | A transport failure (DNS, connection, TLS) reaching the host. |
| `file_error` | no | Could not write the output path. |
| `invalid_offset` | no | `--offset` must be zero or greater. |
| `invalid_limit` | no | `--limit` must be zero or greater. |
| `limit_too_large` | no | `--limit` exceeds this command's agent maximum. |
| `output_required` | no | Agent mode requires an explicit `--output` or `--dir` path before writing a file. |
| `invalid_download_kind` | no | `--kind` is not a supported export or extraction kind. |
| `conflicting_page_selection` | no | `--page` and `--all` were both given. |
| `conflicting_text_offset` | no | `--start-char` and `--all` were both given. |
| `invalid_start_char` | no | `--start-char` must be zero or greater. |
| `start_char_out_of_range` | no | `--start-char` exceeds the page's total character count. |
| `invalid_max_chars` | no | `--max-chars` must be at least 1. |
| `max_chars_too_large` | no | `--max-chars` exceeds the agent maximum of 50000. |
| `invalid_format` | no | `--format` is not text, json, or html. |
| `invalid_delay` | no | `--delay` must be zero or greater. |
| `unsupported_agent_option` | no | A flag (such as `--include-html`) was combined with `--agent`, which does not support it. |
| `operation_failed` | no | An uncategorized failure; no more specific code applies. |

`SKILL.md` packages all of this as an agent skill.

## Caching

Only network catalogs are cached, one JSON file per host under your user
cache directory (`~/Library/Caches/pressbooks-pp-cli/catalogs/{host}.json` on
macOS). Page content is always fetched fresh, so an extract never silently
returns a stale chapter. Catalogs are fresh for seven days; staleness is
checked with a single cheap request before falling back to a full re-crawl.
`--no-cache` forces a fresh crawl. `doctor` always ignores the cache, since
its job is to prove a host is reachable right now.

## The network list

The bundled list (`internal/pressbooks/networks.json`) ships 28 usable
networks holding 9,601 books at last probe:

```console
$ pressbooks-pp-cli networks
HOST                             NAME                                                       BOOKS
ecampusontario.pressbooks.pub    eCampusOntario Open Authoring Platform                     3078
pressbooks.online.ucf.edu        University of Central Florida Pressbooks                   1939
pressbooks.bccampus.ca           British Columbia/Yukon Open Authoring Platform             862
uw.pressbooks.pub                University of Washington Libraries                         372
boisestate.pressbooks.pub        Boise State Pressbooks                                     320
pressbooks.cuny.edu              CUNY Pressbooks Network                                    263
open.maricopa.edu                Maricopa Open Digital Press                                257
uen.pressbooks.pub               UEN Pressbooks Consortium                                  243
pressbooks.library.torontomu.ca  Toronto Metropolitan University Pressbooks                 215
opentextbc.ca                    BCcampus Open Publishing                                   212
pressbooks.nebraska.edu          University of Nebraska Pressbooks                          211
idaho.pressbooks.pub             Idaho Open Press                                           195
kpu.pressbooks.pub               Kwantlen Polytechnic University Publishing                 188
open.lib.umn.edu                 University of Minnesota Libraries Publishing               174
openoregon.pressbooks.pub        Open Oregon Educational Resources                          174
ohiostate.pressbooks.pub         The Ohio State University Pressbooks                       114
wsu.pressbooks.pub               Open Text WSU                                              104
louis.pressbooks.pub             LOUIS Pressbooks                                           103
milnepublishing.geneseo.edu      Milne Publishing                                           90
iastate.pressbooks.pub           Iowa State University Open Books                           85
viva.pressbooks.pub              VIVA Open Publishing                                       80
pressbooks.usnh.edu              University System of New Hampshire Pressbooks              74
open.library.okstate.edu         Open OKState                                               63
pressbooks.lib.vt.edu            Pressbooks at Virginia Tech                                46
oer.pressbooks.pub               PressbooksOER                                              43
rotel.pressbooks.pub             Remixing Open Textbooks through an Equity Lens (ROTEL)     38
ncstate.pressbooks.pub           NC State University Libraries Sites                        34
odp.library.tamu.edu             Texas A&M University System Open Digital Publishing Sites  24
```

A handful of hosts were probed and found unusable — Cloudflare-blocked, no
Pressbooks API, or unreachable outright. They stay out of the default list
so a search sweep never wastes a request on them, but they are recorded, not
silently forgotten. `--include-excluded` shows them and why:

```console
$ pressbooks-pp-cli networks --include-excluded | tail -9

Excluded (probed and found unusable):
HOST                           REASON
pressbooks.pub                 HTTP 403 Cloudflare managed challenge. Unlike pressbooks.bccampus.ca and opentextbc.ca, which are also Cloudflare-fronted but reachable, this host blocks a Go client's fingerprint too. This is the Pressbooks Directory host; its api.pressbooks.com backend is an undocumented JavaScript application, not a public API.
oer.hawaii.edu                 HTTP 404 rest_no_route. Reachable, but serves no Pressbooks v2 API.
oer.galileo.usg.edu            HTTP 404 with an ordinary HTML error page, not rest_no_route JSON. Reachable, but serves no Pressbooks v2 API.
fanshawe.pressbooks.pub        DNS lookup fails, no such host. The domain no longer resolves.
library.achievingthedream.org  DNS lookup fails, no such host.
montana.pressbooks.net         TCP connection to 216.40.34.41:443 times out.
```

`--host` is not restricted to this list — any Pressbooks host with a working
v2 API can be read directly with `info`, `toc`, `extract`, `download`, or
`books`/`search --host`. The bundled list only decides which networks an
all-network `search` sweeps.

## Project notes

This is a CLI Printing Press `*-pp-cli` tool: `spec.yaml` is the API spec
accepted by CLI Printing Press, and `tools-manifest.json` is the
generated-style tool manifest. It is the third in a family — `openstax-cli`
and `libretexts-cli` already exist — and is built to read alike and to be
driven by the same agent contract as `openstax-cli`.

## Contributing

CI runs `gofmt`, `go vet` (with and without the `integration` build tag),
`go build`, `go test -race`, and `golangci-lint` on every push and pull
request, plus a nightly job that runs the live integration suite against
real Pressbooks hosts so a moved or dead endpoint fails the build instead of
shipping. Reproduce it locally with `make verify` for everything that runs
offline, or `make verify-live` for that plus the live suite — `make verify`
alone only type-checks the integration tests, so a tree can pass it while
every live test in it has never once run. `make test`, `make
test-integration` and `make lint` run the pieces individually.

## License

This CLI is released under the [MIT License](LICENSE).

**Pressbooks textbook content retrieved by this tool is not MIT licensed.**
Every Pressbooks book carries its own license, chosen by its author or
publisher — most often a Creative Commons variant, but not always. It varies
book by book within a single network: a single host in this bundled list has
been observed serving CC BY, CC BY-NC-SA, and All Rights Reserved titles
side by side. `info` reports the license name and URL for any book. You are
responsible for checking that license and preserving it, and its
attribution, when you reuse extracted content.
