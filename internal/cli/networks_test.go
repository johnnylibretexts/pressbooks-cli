package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
)

func TestNetworksNeedsNoNetworkRequest(t *testing.T) {
	fake := &fakeClient{countErr: errUnexpectedCall}
	stdout, stderr, err := executeWithFake(t, fake, "networks", "--agent")
	if err != nil {
		t.Fatalf("networks --agent: %v; stderr=%s", err, stderr)
	}
	var got struct {
		Data []agentNetworkSummary `json:"data"`
		Meta agentMeta             `json:"meta"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	// The bundled list holds 28 networks, but the agent default page size is a
	// flat 26 regardless — matching the published schema exactly rather than
	// growing with the list — so an unpaginated call sees 26 of 28 with more
	// available.
	if got.Meta.Total != 28 || len(got.Data) != 26 {
		t.Fatalf("got %d of %d networks, want 26 of 28", len(got.Data), got.Meta.Total)
	}
	if !got.Meta.HasMore || got.Meta.NextOffset == nil || *got.Meta.NextOffset != 26 {
		t.Fatalf("expected more networks to be available past the default page: %#v", got.Meta)
	}
	if got.Data[0].Host != "ecampusontario.pressbooks.pub" {
		t.Fatalf("networks should lead with the largest: %#v", got.Data[0])
	}
}

// A user who wonders why a network they know of is missing deserves an answer.
func TestNetworksIncludeExcludedExplainsWhy(t *testing.T) {
	fake := &fakeClient{countErr: errUnexpectedCall}
	stdout, _, err := executeWithFake(t, fake, "networks", "--include-excluded")
	if err != nil {
		t.Fatal(err)
	}
	// oer.hawaii.edu is genuinely excluded (it answers but serves no Pressbooks
	// v2 API); opentextbc.ca is not used here because it turned out to be
	// reachable after all and was promoted into the regular network list.
	if !strings.Contains(stdout, "oer.hawaii.edu") || !strings.Contains(stdout, "rest_no_route") {
		t.Fatalf("excluded hosts and reasons missing from output:\n%s", stdout)
	}
}

func TestDoctorReportsPerHostStatus(t *testing.T) {
	fake := &fakeClient{counts: map[string]int{"ncstate.pressbooks.pub": 34}}
	stdout, _, err := executeWithFake(t, fake, "doctor", "--host", "ncstate.pressbooks.pub", "--agent")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK   bool              `json:"ok"`
		Data []agentHostStatus `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || len(got.Data) != 1 || got.Data[0].Status != "reachable" || got.Data[0].Books != 34 {
		t.Fatalf("unexpected doctor payload: %#v", got)
	}
}

// doctor exists to prove a host is reachable, so answering it from cache would
// be a lie.
func TestDoctorIgnoresTheCache(t *testing.T) {
	fake := &fakeClient{counts: map[string]int{"ncstate.pressbooks.pub": 34}}
	if _, _, err := executeWithFake(t, fake, "doctor", "--host", "ncstate.pressbooks.pub"); err != nil {
		t.Fatal(err)
	}
	if !fake.cacheDisabled {
		t.Fatal("doctor must disable the cache before checking reachability")
	}
}

func TestDoctorFailsWhenEveryHostFails(t *testing.T) {
	fake := &fakeClient{countErr: errBlockedForTest}
	stdout, _, err := executeWithFake(t, fake, "doctor", "--host", "blocked.example.test", "--agent")
	if err == nil {
		t.Fatal("doctor should fail when no host is reachable")
	}
	if code := agentErrorCode(t, stdout); code != "blocked_by_host" {
		t.Fatalf("error code = %q, want blocked_by_host", code)
	}
}

// This tool tries to be modest with other people's servers: a host given
// twice must be probed once, not once per repetition.
func TestDoctorDeduplicatesRepeatedHosts(t *testing.T) {
	fake := &fakeClient{counts: map[string]int{"ncstate.pressbooks.pub": 34}}
	stdout, _, err := executeWithFake(t, fake, "doctor",
		"--host", "ncstate.pressbooks.pub", "--host", "NCState.Pressbooks.pub", "--agent")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data []agentHostStatus `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("got %d statuses for one host repeated (with different casing), want 1: %#v", len(got.Data), got.Data)
	}
}

// The human-readable table must report the same facts as JSON and agent mode,
// including the network name.
func TestDoctorHumanTableIncludesName(t *testing.T) {
	fake := &fakeClient{counts: map[string]int{"ncstate.pressbooks.pub": 34}}
	stdout, _, err := executeWithFake(t, fake, "doctor", "--host", "ncstate.pressbooks.pub")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "NC State University Libraries Sites") {
		t.Fatalf("doctor's human table should include the network name:\n%s", stdout)
	}
}

// With only one host, nothing exercises the semaphore or contends over the
// results slice, so a single-host run passing under -race proves nothing
// about the concurrent probe itself. This drives more hosts than
// doctorConcurrency at once, mixes every classification, and checks that
// results land in request order (not arrival order) with no host missing or
// duplicated.
func TestDoctorProbesManyHostsConcurrently(t *testing.T) {
	const (
		reachableCount   = 12
		blockedCount     = 4
		unreachableCount = 4
	)
	fake := &fakeClient{counts: map[string]int{}, countErrs: map[string]error{}}
	var hosts []string
	args := []string{"doctor", "--agent"}

	addHost := func(host string) {
		hosts = append(hosts, host)
		args = append(args, "--host", host)
	}
	for i := 0; i < reachableCount; i++ {
		host := fmt.Sprintf("reachable%d.example.test", i)
		fake.counts[host] = i + 1
		addHost(host)
	}
	for i := 0; i < blockedCount; i++ {
		host := fmt.Sprintf("blocked%d.example.test", i)
		fake.countErrs[host] = errBlockedForTest
		addHost(host)
	}
	for i := 0; i < unreachableCount; i++ {
		host := fmt.Sprintf("down%d.example.test", i)
		fake.countErrs[host] = errors.New("dial tcp: connection refused")
		addHost(host)
	}
	if len(hosts) <= doctorConcurrency {
		t.Fatalf("test setup: %d hosts does not exceed doctorConcurrency (%d)", len(hosts), doctorConcurrency)
	}

	stdout, _, err := executeWithFake(t, fake, args...)
	if err != nil {
		t.Fatalf("doctor with a mix of hosts: %v", err)
	}
	var got struct {
		Data []agentHostStatus `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != len(hosts) {
		t.Fatalf("got %d statuses, want %d", len(got.Data), len(hosts))
	}

	seen := make(map[string]bool, len(hosts))
	for i, s := range got.Data {
		if s.Host != hosts[i] {
			t.Fatalf("result %d = host %q, want %q (results must be in request order, not arrival order)", i, s.Host, hosts[i])
		}
		if seen[s.Host] {
			t.Fatalf("host %q appeared more than once", s.Host)
		}
		seen[s.Host] = true
	}
	for i := 0; i < reachableCount; i++ {
		if got.Data[i].Status != "reachable" {
			t.Errorf("%s: status = %q, want reachable", got.Data[i].Host, got.Data[i].Status)
		}
	}
	for i := reachableCount; i < reachableCount+blockedCount; i++ {
		if got.Data[i].Status != "blocked" {
			t.Errorf("%s: status = %q, want blocked", got.Data[i].Host, got.Data[i].Status)
		}
	}
	for i := reachableCount + blockedCount; i < len(hosts); i++ {
		if got.Data[i].Status != "unreachable" {
			t.Errorf("%s: status = %q, want unreachable", got.Data[i].Host, got.Data[i].Status)
		}
	}
}

// doctorConcurrencyTrackingClient is a pressbooksClient double built to prove
// probeHosts routes every probe through backendLimiter, the way
// concurrencyTrackingClient in search_test.go proves it for sweepNetworks. It
// records, per backend key, how many LiveBookCount calls are simultaneously
// in flight and the peak observed.
type doctorConcurrencyTrackingClient struct {
	mu       sync.Mutex
	inFlight map[string]int
	peak     map[string]int
}

func newDoctorConcurrencyTrackingClient() *doctorConcurrencyTrackingClient {
	return &doctorConcurrencyTrackingClient{inFlight: map[string]int{}, peak: map[string]int{}}
}

func (c *doctorConcurrencyTrackingClient) LiveBookCount(_ context.Context, host string) (int, error) {
	key := backendKey(host)
	c.mu.Lock()
	c.inFlight[key]++
	if c.inFlight[key] > c.peak[key] {
		c.peak[key] = c.inFlight[key]
	}
	c.mu.Unlock()

	// Long enough that two overlapping probes sharing a backend would
	// unmistakably both be in flight at once if nothing serialized them.
	time.Sleep(20 * time.Millisecond)

	c.mu.Lock()
	c.inFlight[key]--
	c.mu.Unlock()
	return 1, nil
}

func (c *doctorConcurrencyTrackingClient) Catalog(context.Context, string, func(int, int)) (pressbooks.Catalog, error) {
	return pressbooks.Catalog{}, errUnexpectedCall
}
func (c *doctorConcurrencyTrackingClient) BookMetadata(context.Context, pressbooks.BookRef) (pressbooks.Book, error) {
	return pressbooks.Book{}, errUnexpectedCall
}
func (c *doctorConcurrencyTrackingClient) TOC(context.Context, pressbooks.BookRef) (pressbooks.TOC, error) {
	return pressbooks.TOC{}, errUnexpectedCall
}
func (c *doctorConcurrencyTrackingClient) PageContent(context.Context, pressbooks.BookRef, pressbooks.Page) ([]byte, string, error) {
	return nil, "", errUnexpectedCall
}
func (c *doctorConcurrencyTrackingClient) ExportFormats(context.Context, pressbooks.BookRef) ([]string, error) {
	return nil, errUnexpectedCall
}
func (c *doctorConcurrencyTrackingClient) Download(context.Context, string, io.Writer) (int64, string, error) {
	return 0, "", errUnexpectedCall
}

// TestDoctorSerializesProbesSharingABackend is the Important-2 fix's test:
// before it, probeHosts had no notion of backendKey at all, so a bare doctor
// run against fifteen *.pressbooks.pub hosts put up to doctorConcurrency (8)
// concurrent requests on that one shared backend — precisely the
// concentration pattern that rate-limited this project's own all-network
// sweep, recreated on the one command a user runs after being blocked.
func TestDoctorSerializesProbesSharingABackend(t *testing.T) {
	withBackendPace(t, time.Millisecond)
	client := newDoctorConcurrencyTrackingClient()
	var args []string
	const sharedHosts = 6
	for i := 0; i < sharedHosts; i++ {
		args = append(args, "--host", fmt.Sprintf("shared%d.pressbooks.pub", i))
	}
	// An unrelated host, its own backend key, must still probe concurrently
	// with the shared-backend hosts rather than being serialized behind them.
	args = append(args, "--host", "unrelated.example.test")

	var stdout, stderr bytes.Buffer
	err := executeArgsWithClient(append([]string{"doctor", "--agent"}, args...), &stdout, &stderr,
		func(time.Duration) pressbooksClient { return client })
	if err != nil {
		t.Fatalf("doctor: %v; stderr=%s", err, stderr.String())
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if got := client.peak["pressbooks.pub"]; got != 1 {
		t.Fatalf("peak concurrent probes on the shared pressbooks.pub backend = %d, want 1 (backendLimiter should serialize them)", got)
	}
}
