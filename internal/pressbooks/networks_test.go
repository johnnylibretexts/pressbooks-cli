package pressbooks

import (
	"strings"
	"testing"
)

func TestBundledNetworksAreUsable(t *testing.T) {
	networks := Networks()
	if len(networks) != 28 {
		t.Fatalf("got %d bundled networks, want 28", len(networks))
	}
	seen := map[string]bool{}
	for _, n := range networks {
		switch {
		case n.Host == "":
			t.Errorf("network %#v has no host", n)
		case strings.Contains(n.Host, "/") || strings.Contains(n.Host, ":"):
			t.Errorf("host %q should be a bare hostname", n.Host)
		case n.Name == "":
			t.Errorf("network %q has no name", n.Host)
		case n.Books <= 0:
			t.Errorf("network %q has a nonsense book count %d", n.Host, n.Books)
		case seen[n.Host]:
			t.Errorf("network %q is listed twice", n.Host)
		}
		seen[n.Host] = true
	}
	// Sorted by size, largest first, so `networks` output leads with the
	// networks most likely to hold what someone is looking for.
	for i := 1; i < len(networks); i++ {
		if networks[i-1].Books < networks[i].Books {
			t.Fatalf("networks are not ordered by size: %q before %q", networks[i-1].Host, networks[i].Host)
		}
	}
}

// An excluded host must never also be offered, or a crawl would spend requests
// on a host already known to refuse them.
func TestExcludedNetworksAreNotAlsoOffered(t *testing.T) {
	offered := map[string]bool{}
	for _, n := range Networks() {
		offered[n.Host] = true
	}
	excluded := ExcludedNetworks()
	if len(excluded) != 6 {
		t.Fatalf("got %d excluded hosts, want 6", len(excluded))
	}
	for _, e := range excluded {
		if offered[e.Host] {
			t.Errorf("%q is both offered and excluded", e.Host)
		}
		if e.Reason == "" {
			t.Errorf("%q is excluded with no recorded reason", e.Host)
		}
	}
}

func TestFindNetwork(t *testing.T) {
	got, ok := FindNetwork("NCState.Pressbooks.PUB")
	if !ok || got.Books != 34 {
		t.Fatalf("FindNetwork = (%#v, %v)", got, ok)
	}
	if _, ok := FindNetwork("example.test"); ok {
		t.Fatal("FindNetwork matched a host that is not bundled")
	}
}
