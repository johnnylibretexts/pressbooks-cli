package pressbooks

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

//go:embed networks.json
var networksJSON []byte

var (
	networksOnce sync.Once
	networkList  NetworkList
)

func loadNetworks() {
	networksOnce.Do(func() {
		// The resource is compiled in, so a decode failure is a build-time
		// mistake rather than a runtime condition a caller can handle.
		if err := json.Unmarshal(networksJSON, &networkList); err != nil {
			panic("pressbooks: bundled networks.json is invalid: " + err.Error())
		}
		sort.SliceStable(networkList.Networks, func(i, j int) bool {
			return networkList.Networks[i].Books > networkList.Networks[j].Books
		})
	})
}

// Networks returns the bundled networks, largest first.
func Networks() []Network {
	loadNetworks()
	out := make([]Network, len(networkList.Networks))
	copy(out, networkList.Networks)
	return out
}

// ExcludedNetworks returns hosts that were probed and found unusable.
func ExcludedNetworks() []ExcludedNetwork {
	loadNetworks()
	out := make([]ExcludedNetwork, len(networkList.Excluded))
	copy(out, networkList.Excluded)
	return out
}

func FindNetwork(host string) (Network, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, n := range Networks() {
		if n.Host == host {
			return n, true
		}
	}
	return Network{}, false
}
