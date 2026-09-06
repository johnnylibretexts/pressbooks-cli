package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
	"github.com/spf13/cobra"
)

// agentNetworkSummary is one entry in the networks listing: a bundled network,
// or, with --include-excluded, a host that was probed and found unusable.
// Books is the count observed when the bundled list was last probed, not a
// live count — networks answers entirely from the embedded list and makes no
// request of its own.
type agentNetworkSummary struct {
	Host     string `json:"host"`
	Name     string `json:"name,omitempty"`
	Books    int    `json:"books,omitempty"`
	Excluded bool   `json:"excluded,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// agentHostStatus is one host's doctor result. Status is one of "reachable",
// "blocked", "no_api", or "unreachable".
type agentHostStatus struct {
	Host   string `json:"host"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
	Books  int    `json:"books,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// networksDefaultLimit matches the published agent schema's "limit=26"
// exactly. It never changes with --include-excluded: a caller who wants the
// excluded hosts too pages for them via next_offset, rather than the default
// silently growing past what the contract documents.
const networksDefaultLimit = 26

func networksCmd(f *flags) *cobra.Command {
	var includeExcluded bool
	var limit int
	var offset int
	cmd := &cobra.Command{
		Use:   "networks",
		Short: "List the bundled Pressbooks networks.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			networks := pressbooks.Networks()
			entries := make([]agentNetworkSummary, 0, len(networks))
			for _, n := range networks {
				entries = append(entries, agentNetworkSummary{Host: n.Host, Name: n.Name, Books: n.Books})
			}
			var excluded []pressbooks.ExcludedNetwork
			if includeExcluded {
				excluded = pressbooks.ExcludedNetworks()
				for _, e := range excluded {
					entries = append(entries, agentNetworkSummary{Host: e.Host, Excluded: true, Reason: e.Reason})
				}
			}
			if f.agent {
				// The default limit is a flat networksDefaultLimit in every flag
				// combination, matching the published schema's "limit=26" exactly:
				// an agent that read the contract must never be surprised by more
				// rows than it asked for, even when --include-excluded pushes the
				// full list past that. Pagination is how it gets the rest.
				start, end, meta, err := agentWindow(len(entries), offset, limit, networksDefaultLimit, 100)
				if err != nil {
					return err
				}
				return writeAgentSuccess(cmd.OutOrStdout(), "networks", entries[start:end], &meta)
			}
			if f.asJSON {
				return pressbooks.WriteJSON(cmd.OutOrStdout(), entries)
			}
			return writeNetworksTable(cmd.OutOrStdout(), networks, excluded)
		},
	}
	cmd.Flags().BoolVar(&includeExcluded, "include-excluded", false, "Also list hosts that were probed and found unusable, and why")
	cmd.Flags().IntVar(&limit, "limit", 0, "Agent result limit (default 26)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Agent result offset")
	return cmd
}

func writeNetworksTable(w io.Writer, networks []pressbooks.Network, excluded []pressbooks.ExcludedNetwork) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "HOST\tNAME\tBOOKS"); err != nil {
		return err
	}
	for _, n := range networks {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%d\n", n.Host, n.Name, n.Books); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(excluded) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nExcluded (probed and found unusable):"); err != nil {
		return err
	}
	etw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(etw, "HOST\tREASON"); err != nil {
		return err
	}
	for _, e := range excluded {
		if _, err := fmt.Fprintf(etw, "%s\t%s\n", e.Host, e.Reason); err != nil {
			return err
		}
	}
	return etw.Flush()
}

// doctorConcurrency bounds how many hosts are probed at once, globally,
// across every backend combined — the same role sweepConcurrency plays for
// search. Deliberately modest, for the same reason the catalog crawl is:
// these are university servers, not a load-testing target. It is not, on
// its own, a per-backend limit: probeHosts also routes every probe through
// backendLimiter (backend.go), because fifteen of the twenty-eight bundled
// hosts share the pressbooks.pub backend and doctor is precisely the
// command a user runs after being blocked — it must not recreate the
// concentrated-request pattern that caused this project's own rate-limiting
// incident, on the one command whose whole job is to check for exactly that
// kind of trouble.
const doctorConcurrency = 8

func doctorCmd(f *flags) *cobra.Command {
	var hosts []string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check network reachability right now.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, ctx := clientAndContext(f)
			// doctor exists to prove a host is reachable right now, so an answer
			// served from the cache would be a lie. Disable it unconditionally,
			// regardless of whether --no-cache was also passed.
			disableCache(c)

			// Deduplicated, preserving the order --host was given: this tool
			// tries to be modest with other people's servers, so a host repeated
			// by mistake is probed once, not once per repetition.
			targets := make([]string, 0, len(hosts))
			seen := make(map[string]bool, len(hosts))
			for _, host := range hosts {
				host = strings.ToLower(strings.TrimSpace(host))
				if host == "" || seen[host] {
					continue
				}
				seen[host] = true
				targets = append(targets, host)
			}
			if len(targets) == 0 {
				for _, n := range pressbooks.Networks() {
					targets = append(targets, n.Host)
				}
			}

			probes := probeHosts(ctx, c, targets)
			statuses := make([]agentHostStatus, len(probes))
			var firstErr error
			reachable := 0
			for i, p := range probes {
				statuses[i] = p.status
				if p.err != nil {
					if firstErr == nil {
						firstErr = p.err
					}
					continue
				}
				reachable++
			}
			// Every host failed: there is nothing to report but the failure
			// itself, so surface the first one rather than a synthetic summary.
			if reachable == 0 {
				return firstErr
			}

			if f.agent {
				return writeAgentSuccess(cmd.OutOrStdout(), "doctor", statuses, nil)
			}
			if f.asJSON {
				return pressbooks.WriteJSON(cmd.OutOrStdout(), statuses)
			}
			return writeDoctorTable(cmd.OutOrStdout(), statuses)
		},
	}
	cmd.Flags().StringArrayVar(&hosts, "host", nil, "Host to check; repeatable. Defaults to every bundled network.")
	return cmd
}

type hostProbe struct {
	status agentHostStatus
	err    error
}

// probeHosts checks every host concurrently, bounded by doctorConcurrency
// globally and, per backend, by backendLimiter (backend.go) — the same
// per-backend courtesy sweepNetworks applies to search, and for the same
// reason: without it, a bare doctor run puts up to doctorConcurrency
// concurrent requests on pressbooks.pub alone, the shared backend behind
// fifteen of the twenty-eight bundled hosts. Results are returned in the
// same order hosts were given. Each goroutine writes only to its own index
// of a pre-sized slice, so there is no shared mutable state for -race to
// catch.
func probeHosts(ctx context.Context, c pressbooksClient, hosts []string) []hostProbe {
	results := make([]hostProbe, len(hosts))
	sem := make(chan struct{}, doctorConcurrency)
	limiter := newBackendLimiter()
	var wg sync.WaitGroup
	wg.Add(len(hosts))
	for i, host := range hosts {
		go func(i int, host string) {
			defer wg.Done()
			// Backend lock acquired before the doctorConcurrency semaphore,
			// deliberately, matching sweepNetworks: waiting your turn on a
			// shared backend must not itself consume one of only
			// doctorConcurrency global slots.
			if err := limiter.acquire(ctx, host); err != nil {
				results[i] = hostProbe{
					status: agentHostStatus{Host: host, Status: "unreachable", Detail: err.Error()},
					err:    err,
				}
				return
			}
			defer limiter.release(host)
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = probeHost(ctx, c, host)
		}(i, host)
	}
	wg.Wait()
	return results
}

// probeHost classifies one host's reachability using the domain package's
// sentinel errors rather than matching on message text, so a wording change
// upstream can never silently turn a blocked host into an "unreachable" one.
func probeHost(ctx context.Context, c pressbooksClient, host string) hostProbe {
	name := ""
	if n, ok := pressbooks.FindNetwork(host); ok {
		name = n.Name
	}
	count, err := c.LiveBookCount(ctx, host)
	status := agentHostStatus{Host: host, Name: name}
	if err == nil {
		status.Status = "reachable"
		status.Books = count
		return hostProbe{status: status}
	}
	switch {
	case errors.Is(err, pressbooks.ErrBlocked):
		status.Status = "blocked"
	case errors.Is(err, pressbooks.ErrNoAPI):
		status.Status = "no_api"
	default:
		status.Status = "unreachable"
	}
	status.Detail = err.Error()
	return hostProbe{status: status, err: err}
}

func writeDoctorTable(w io.Writer, statuses []agentHostStatus) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "HOST\tNAME\tSTATUS\tBOOKS\tDETAIL"); err != nil {
		return err
	}
	for _, s := range statuses {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", s.Host, s.Name, s.Status, s.Books, s.Detail); err != nil {
			return err
		}
	}
	return tw.Flush()
}
