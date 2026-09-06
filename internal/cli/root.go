package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/johnnylibretexts/pressbooks-cli/internal/pressbooks"
	"github.com/spf13/cobra"
)

const version = "0.1.0"

type flags struct {
	asJSON        bool
	agent         bool
	noCache       bool
	timeout       time.Duration
	clientFactory clientFactory
}

// pressbooksClient is the seam every command is tested through. It lists only
// the methods pressbooks.Client implements today, so the real client and the
// test fake both satisfy it and go build ./... stays clean; when a later task
// adds a method to pressbooks.Client (BookMetadata and TOC in Task 8,
// PageContent in Task 9, ExportFormats in Task 11), it adds that method here
// in the same commit, so the interface and the client can never drift apart.
// ExportFormats, added alongside the download command in Task 11, is the
// last such method: every command this tool has now has its client method
// listed here.
type pressbooksClient interface {
	Catalog(ctx context.Context, host string, progress func(current, total int)) (pressbooks.Catalog, error)
	LiveBookCount(ctx context.Context, host string) (int, error)
	BookMetadata(ctx context.Context, ref pressbooks.BookRef) (pressbooks.Book, error)
	TOC(ctx context.Context, ref pressbooks.BookRef) (pressbooks.TOC, error)
	PageContent(ctx context.Context, ref pressbooks.BookRef, page pressbooks.Page) ([]byte, string, error)
	ExportFormats(ctx context.Context, ref pressbooks.BookRef) ([]string, error)
	Download(ctx context.Context, rawURL string, w io.Writer) (int64, string, error)
}

type clientFactory func(time.Duration) pressbooksClient

func Execute() error { return executeArgs(os.Args[1:], os.Stdout, os.Stderr) }

func RootCmd() *cobra.Command {
	root, _ := rootCmdWithClient(func(timeout time.Duration) pressbooksClient {
		return pressbooks.New(timeout)
	})
	return root
}

func rootCmdWithClient(factory clientFactory) (*cobra.Command, *flags) {
	f := &flags{clientFactory: factory}
	root := &cobra.Command{
		Use:           "pressbooks-pp-cli",
		Short:         "Search and extract open textbooks from Pressbooks networks.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.PersistentFlags().BoolVar(&f.asJSON, "json", false, "Output complete JSON")
	root.PersistentFlags().BoolVar(&f.agent, "agent", false, "Use the compact, versioned agent JSON contract")
	root.PersistentFlags().BoolVar(&f.noCache, "no-cache", false, "Ignore the cached catalog and fetch a fresh copy")
	root.PersistentFlags().DurationVar(&f.timeout, "timeout", 60*time.Second, "HTTP request timeout; for downloads this bounds the response headers, not the transfer")
	root.PersistentPreRun = func(*cobra.Command, []string) {
		if f.agent {
			f.asJSON = true
		}
	}
	root.AddCommand(schemaCmd(f), networksCmd(f), doctorCmd(f), booksCmd(f), searchCmd(f), infoCmd(f), tocCmd(f), extractCmd(f), downloadCmd(f))
	return root, f
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

type cacheConfigurable interface{ SetCacheTTL(time.Duration) }

func disableCache(c pressbooksClient) {
	if configurable, ok := c.(cacheConfigurable); ok {
		configurable.SetCacheTTL(0)
	}
}

func clientAndContext(f *flags) (pressbooksClient, context.Context) {
	c := f.clientFactory(f.timeout)
	if f.noCache {
		disableCache(c)
	}
	return c, context.Background()
}

func schemaCmd(f *flags) *cobra.Command {
	return &cobra.Command{
		Use:     "schema",
		Aliases: []string{"capabilities"},
		Short:   "Describe the compact agent command contract.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			schema := currentAgentSchema()
			if f.agent {
				return writeAgentSuccess(cmd.OutOrStdout(), "schema", schema, nil)
			}
			if f.asJSON {
				return pressbooks.WriteJSON(cmd.OutOrStdout(), schema)
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Use --agent for one-line JSON. Commands:"); err != nil {
				return err
			}
			for _, capability := range schema.Commands {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s\t%s\n", capability.Name, capability.Purpose); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
