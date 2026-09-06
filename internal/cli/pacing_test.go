package cli

import (
	"os"
	"testing"
	"time"
)

// TestMain defaults extractDelayDefault to zero for this package's whole
// test binary: every test here drives a fakeClient, not the real host
// --delay exists to be gentle with, and should not pay real wall-clock time
// for a pace that only matters against one. Without this, every existing
// extract --all and download --kind text/json/html test that walks more than
// one page pays extractDelayDefault (100ms in production) per page.
// TestExtractPacesWholeBookFetches and TestDownloadPacesWholeBookFetches,
// which actually measure the mechanism, override this locally to a small,
// real value and restore it via t.Cleanup — the same pattern
// pressbooks.TestMain already uses for crawlPace.
func TestMain(m *testing.M) {
	extractDelayDefault = 0
	os.Exit(m.Run())
}

// withExtractDelay temporarily overrides extractDelayDefault for a test's
// duration and restores TestMain's zero default afterward.
func withExtractDelay(t *testing.T, delay time.Duration) {
	t.Helper()
	original := extractDelayDefault
	extractDelayDefault = delay
	t.Cleanup(func() { extractDelayDefault = original })
}

// assertPacedCalls checks that every gap between successive timestamps in
// calls is at least ~delay, with a generous tolerance for scheduling noise —
// the same shape of assertion pressbooks.TestCatalogPacesPageDispatchWithinACrawl
// uses for crawlPace.
func assertPacedCalls(t *testing.T, calls []time.Time, delay time.Duration) {
	t.Helper()
	if len(calls) < 2 {
		t.Fatalf("test setup: need at least 2 calls to measure pacing between them, got %d", len(calls))
	}
	tolerance := delay / 2
	for i := 1; i < len(calls); i++ {
		gap := calls[i].Sub(calls[i-1])
		if gap < delay-tolerance {
			t.Fatalf("gap between page fetch %d and %d = %v, want at least ~%v", i-1, i, gap, delay)
		}
	}
}
