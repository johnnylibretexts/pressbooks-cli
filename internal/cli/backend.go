// Package cli's single answer to "how hard may this tool hit one backend
// across a fan-out of many hosts." Every command that fans out requests
// across several bundled networks — sweepNetworks (search.go) and
// probeHosts (networks.go) so far — routes through backendKey and
// backendLimiter defined here, rather than picking its own concurrency
// number.
//
// This file exists because that was not always true: doctor originally ran
// at doctorConcurrency with no backend awareness at all, recreating on
// every run the exact concentration pattern (many concurrent requests
// landing on one shared origin) that got this project's own all-network
// sweep rate-limited out of fifteen networks at once. backendKey and
// backendLimiter lived only in search.go at the time, a file doctor had no
// reason to import, which is exactly why it slipped. If you add a third
// command that fans out across bundled hosts, it must acquire this
// limiter too — do not let it rediscover the problem by hand.
package cli

import (
	"context"
	"strings"
	"sync"
	"time"
)

// backendKey groups hosts that share one physical serving backend, so
// per-backend concurrency and pacing (backendLimiter) can be enforced
// independently of DNS naming. Every *.pressbooks.pub host is an individual
// subdomain of one shared, centrally operated Pressbooks Network Manager
// instance, and answers as a single origin to that provider's rate
// limiting: a cold sweep that treated them as unrelated hosts put up to
// sweepConcurrency x crawlConcurrency concurrent requests onto that one
// origin at once and got the whole sweep rate-limited — including hosts
// such as ncstate.pressbooks.pub that had nothing to do with whichever
// concurrent crawl actually tripped it. Do not "simplify" this back to
// grouping by hostname: every *.pressbooks.pub subdomain here genuinely is
// the same backend, not merely a similarly named one.
func backendKey(host string) string {
	if host == "pressbooks.pub" || strings.HasSuffix(host, ".pressbooks.pub") {
		return "pressbooks.pub"
	}
	return host
}

// backendPace is the minimum gap enforced between one crawl (or probe) for a
// shared backend finishing and the next one for that same backend starting.
// Serializing crawls per backend (backendLimiter) is not enough on its own:
// without this, the next subdomain's turn could still start back-to-back
// with the previous one's last request. This exists purely to stay welcome
// on other people's servers, not to fix any bug in the crawl itself.
//
// This is a var, not a const, for the same reason Client.backoff is a field
// rather than a hardcoded use of BaseBackoff: production always wants the
// real value, but a test exercising a sweep or a doctor run with a fake
// client is not actually talking to anyone this pace is meant to protect,
// and should not pay wall-clock for a delay that only matters against a
// real server.
var backendPace = 300 * time.Millisecond

// backendLimiter enforces two per-backend courtesies across a fan-out: at
// most one request in flight per backend at a time, and a minimum pace
// (backendPace) between one turn for a backend finishing and the next one
// for that same backend starting. A host that is its own backend key (every
// bundled network outside *.pressbooks.pub) is unaffected by either: it
// never contends with itself, so it still proceeds in parallel with
// everything else up to whatever global concurrency cap the caller applies
// (sweepConcurrency for search, doctorConcurrency for doctor).
type backendLimiter struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
	last  map[string]time.Time
}

func newBackendLimiter() *backendLimiter {
	return &backendLimiter{slots: map[string]chan struct{}{}, last: map[string]time.Time{}}
}

// acquire blocks until it is this host's backend's turn, then reserves that
// turn; the caller must call release exactly once afterward, whether its
// request succeeded or failed.
func (b *backendLimiter) acquire(ctx context.Context, host string) error {
	key := backendKey(host)
	b.mu.Lock()
	slot, ok := b.slots[key]
	if !ok {
		slot = make(chan struct{}, 1)
		b.slots[key] = slot
	}
	b.mu.Unlock()

	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	b.mu.Lock()
	wait := backendPace - time.Since(b.last[key])
	b.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		<-slot
		return ctx.Err()
	}
}

func (b *backendLimiter) release(host string) {
	key := backendKey(host)
	b.mu.Lock()
	b.last[key] = time.Now()
	slot := b.slots[key]
	b.mu.Unlock()
	<-slot
}
