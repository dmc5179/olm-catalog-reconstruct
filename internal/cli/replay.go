package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/fbc"
	"github.com/dmc5179/olm-catalog-reconstruct/internal/isc"
	"github.com/dmc5179/olm-catalog-reconstruct/internal/replay"
)

func newReplayCommand(g *globalOpts) *cobra.Command {
	var (
		outDir     string
		noFillGaps bool
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "replay ISC.yaml [ISC.yaml...]",
		Short: "Rebuild the catalog(s) oc-mirror v2 would have produced for one or more ImageSetConfigurations",
		Long: `replay applies each ImageSetConfiguration's operator filters to the source catalogs
using the same filtering library as oc-mirror v2 (4.18+), merges the results per target
catalog, repairs upgrade-graph gaps created by merging, and validates the result with
opm's own model validation.

For each target catalog it writes:
  <out>/<target>/configs/   file-based catalog, ready for 'opm serve' or an image build
  <out>/<target>/images.txt bundle and related images the catalog references
and <out>/report.json with a summary of every target.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			loader, err := g.loader()
			if err != nil {
				return err
			}
			configs := make([]*isc.ImageSetConfiguration, 0, len(args))
			for _, p := range args {
				c, err := isc.Load(p)
				if err != nil {
					return err
				}
				if len(c.Mirror.Operators) == 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s has no mirror.operators entries\n", p)
				}
				configs = append(configs, c)
			}
			targets, err := replay.Run(cmd.Context(), loader, configs, replay.Options{FillGaps: !noFillGaps})
			if err != nil {
				return err
			}
			var summaries []replay.TargetSummary
			for _, t := range targets {
				s := t.Summary()
				if !dryRun {
					dir := filepath.Join(outDir, replay.DirName(t.Key))
					if err := fbc.WriteDir(*t.DC, filepath.Join(dir, "configs")); err != nil {
						return err
					}
					imgs := strings.Join(replay.Images(t.DC), "\n") + "\n"
					if err := os.WriteFile(filepath.Join(dir, "images.txt"), []byte(imgs), 0o644); err != nil {
						return err
					}
					s.OutputDir = dir
				}
				summaries = append(summaries, s)
			}
			if !dryRun {
				data, err := json.MarshalIndent(summaries, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(outDir, "report.json"), append(data, '\n'), 0o644); err != nil {
					return err
				}
			}
			printReplay(cmd.OutOrStdout(), summaries)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outDir, "output", "o", "replay-out", "output directory")
	cmd.Flags().BoolVar(&noFillGaps, "no-fill-gaps", false, "fail instead of filling upgrade-graph gaps created by merging configs")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only print the summary; write nothing")
	return cmd
}

func printReplay(w io.Writer, summaries []replay.TargetSummary) {
	for _, s := range summaries {
		fmt.Fprintf(w, "Target catalog: %s\n", s.Target)
		if s.CatalogDigest != "" {
			fmt.Fprintf(w, "  source digest: %s\n", s.CatalogDigest)
		}
		fmt.Fprintf(w, "  from: %s\n", strings.Join(s.Sources, ", "))
		if s.Unfiltered {
			fmt.Fprintf(w, "  NOTE: at least one config mirrors this catalog in full (full: true, no packages)\n")
		}
		fmt.Fprintf(w, "  %d packages, %d bundles, %d images\n", len(s.Packages), s.BundleCount, s.ImageCount)
		for _, p := range s.Packages {
			fmt.Fprintf(w, "  - %s (default: %s)\n", p.Name, p.DefaultChannel)
			for _, c := range p.Channels {
				rng := c.MaxVersion
				if c.MinVersion != c.MaxVersion {
					rng = c.MinVersion + " .. " + c.MaxVersion
				}
				fmt.Fprintf(w, "      %-24s %3d bundle(s)  %s  head=%s\n", c.Name, c.Entries, rng, c.Head)
			}
		}
		for _, gp := range s.Gaps {
			if len(gp.Filled) > 0 {
				fmt.Fprintf(w, "  FILLED %s/%s (%s): added %s\n", gp.Package, gp.Channel, gp.Problem, strings.Join(gp.Filled, ", "))
			} else {
				fmt.Fprintf(w, "  GAP    %s/%s: %s\n", gp.Package, gp.Channel, gp.Problem)
			}
		}
		for _, wmsg := range s.Warnings {
			fmt.Fprintf(w, "  WARNING: %s\n", wmsg)
		}
		if s.OutputDir != "" {
			fmt.Fprintf(w, "  written to %s\n", s.OutputDir)
		}
		fmt.Fprintln(w)
	}
}
