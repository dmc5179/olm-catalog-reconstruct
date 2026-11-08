// Package cli wires the olm-reconstruct subcommands.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/catalog"
)

type globalOpts struct {
	catalogDirs []string
	authFile    string
	cacheDir    string
	platform    string
	verbose     bool
}

// NewRootCommand builds the CLI.
func NewRootCommand() *cobra.Command {
	g := &globalOpts{}
	root := &cobra.Command{
		Use:   "olm-reconstruct",
		Short: "Reconstruct and check OLM operator catalogs mirrored with oc-mirror v2",
		Long: `olm-reconstruct rebuilds the operator catalog that matches what oc-mirror v2 (4.18+)
mirrored into a registry, and checks ImageSetConfigurations for missing operator dependencies.`,
		SilenceUsage: true,
	}
	pf := root.PersistentFlags()
	pf.StringArrayVar(&g.catalogDirs, "catalog-dir", nil,
		"use a local FBC directory for a catalog reference, as REF=DIR (repeatable); e.g. registry.redhat.io/redhat/redhat-operator-index:v4.20=./configs")
	pf.StringVar(&g.authFile, "authfile", os.Getenv("REGISTRY_AUTH_FILE"),
		"registry auth file (podman/docker format); defaults to the standard docker/podman locations")
	pf.StringVar(&g.cacheDir, "cache-dir", "", "directory for extracted catalog images (default: user cache dir)")
	pf.StringVar(&g.platform, "platform", "linux/amd64", "platform to use when a catalog image is multi-arch")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "print progress messages")

	root.AddCommand(newReplayCommand(g), newDepsCommand(g))
	return root
}

func (g *globalOpts) loader() (*catalog.Loader, error) {
	l := catalog.NewLoader()
	for _, cd := range g.catalogDirs {
		ref, dir, ok := strings.Cut(cd, "=")
		if !ok || ref == "" || dir == "" {
			return nil, fmt.Errorf("--catalog-dir must be REF=DIR, got %q", cd)
		}
		l.Overrides[ref] = dir
	}
	if g.authFile != "" {
		kc, err := catalog.NewAuthFileKeychain(g.authFile)
		if err != nil {
			return nil, err
		}
		l.Keychain = authn.NewMultiKeychain(kc, authn.DefaultKeychain)
	}
	if g.cacheDir != "" {
		l.CacheDir = g.cacheDir
	}
	osName, arch, ok := strings.Cut(g.platform, "/")
	if !ok {
		return nil, fmt.Errorf("--platform must be OS/ARCH, got %q", g.platform)
	}
	l.Platform = v1.Platform{OS: osName, Architecture: arch}
	if g.verbose {
		l.Log = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "==> "+format+"\n", args...) }
	}
	return l, nil
}
