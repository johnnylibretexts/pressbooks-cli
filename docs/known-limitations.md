# Known limitations

Four gaps found during review, judged not to block the first release. Each
names the code, what it costs, and what closing it would take.

## A crawl that stops part-way is not resumable

The design's section 9 says a later run resumes a partial crawl rather than
restarting it. It does not. `Catalog()` only reuses a cache entry when
`Complete()` is true, so a host that keeps failing mid-crawl pays for a full
re-crawl on every call, not just the first.

The partial result is correct and is now clearly labelled — it comes back
wrapped in `ErrPartialCatalog` — so this costs requests against someone
else's server, not correctness. Closing it means reading the cached partial
back as a starting page rather than discarding it.

## A partly-blocked export probe reports fewer formats instead of an error

`ExportFormats` in `internal/pressbooks/exports.go` probes each export kind
and returns an error only when every probe fails. If a host starts refusing
part-way through, the call succeeds with a short list, and a format that
exists is reported as absent.

A host that blocks generally blocks every probe, which is the case the
current guard covers. The gap is the onset of rate limiting mid-probe, made
slightly likelier by the probes now being serialised per backend. Closing it
means failing when any probe is refused for a reason that is not "this
format does not exist".

## A truncated crawl and its error can disagree

`maxCrawlPages` caps a crawl at 1000 pages while leaving `TotalPages`
uncapped, so `Complete()` reports the truncation honestly. But if all 1000
capped pages succeed, the crawl returns no error at all: `Complete()` is
false and the error is nil, and nothing in the command layer reads
`Complete()`.

No bundled network comes close to the cap — the largest is 308 pages — so
this is only reachable via the hostile or broken `X-WP-Total` header that
`maxCrawlPages` exists to defend against. Closing it means returning
`ErrPartialCatalog` on the capped path too.

## An empty network reports itself incomplete

`Complete()` compares synced pages against total pages, and a network with no
books has zero of both, which the current expression reads as incomplete.
Nothing consults `Complete()` today, so this is invisible. It matters if
`Complete()` ever becomes a guard, and closing it is a one-line special case.
