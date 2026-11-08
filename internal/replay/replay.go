// Package replay recomputes the operator catalog content that oc-mirror v2
// (4.18+) would have mirrored for one or more ImageSetConfigurations, and
// merges the results into one valid file-based catalog per target catalog.
package replay

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
	filter "github.com/sherine-k/catalog-filter/pkg/filter/mirror-config/v1alpha1"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/catalog"
	"github.com/dmc5179/olm-catalog-reconstruct/internal/fbc"
	"github.com/dmc5179/olm-catalog-reconstruct/internal/isc"
)

// Options control replay behaviour.
type Options struct {
	// FillGaps fills holes in a channel's upgrade graph that result from
	// merging non-contiguous selections. When false such holes are errors.
	FillGaps bool
}

// Source records one ImageSetConfiguration operator entry feeding a target.
type Source struct {
	ConfigPath string
	Index      int
	Operator   isc.Operator
}

// Gap describes a channel whose merged selection was not a single upgrade graph.
type Gap struct {
	Package string   `json:"package"`
	Channel string   `json:"channel"`
	Problem string   `json:"problem"`
	Filled  []string `json:"filled,omitempty"`
}

// Target is one catalog image oc-mirror would produce.
type Target struct {
	Key        string
	CatalogRef string
	Catalog    *catalog.Catalog
	Sources    []Source
	// Unfiltered is true when at least one source mirrors the full catalog.
	Unfiltered bool
	DC         *declcfg.DeclarativeConfig
	Gaps       []Gap
	Warnings   []string
}

// Run replays all operator entries of the given configurations.
func Run(ctx context.Context, loader *catalog.Loader, configs []*isc.ImageSetConfiguration, opts Options) ([]*Target, error) {
	targets := map[string]*Target{}
	var order []string
	for _, cfg := range configs {
		for i, op := range cfg.Mirror.Operators {
			key := op.TargetKey()
			t, ok := targets[key]
			if !ok {
				t = &Target{Key: key, CatalogRef: op.Catalog}
				targets[key] = t
				order = append(order, key)
			}
			t.Sources = append(t.Sources, Source{ConfigPath: cfg.Path, Index: i, Operator: op})
		}
	}
	out := make([]*Target, 0, len(order))
	for _, key := range order {
		t := targets[key]
		if err := t.build(ctx, loader, opts); err != nil {
			return nil, fmt.Errorf("target %s: %w", key, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func (t *Target) build(ctx context.Context, loader *catalog.Loader, opts Options) error {
	cat, err := loader.Load(ctx, t.CatalogRef)
	if err != nil {
		return err
	}
	t.Catalog = cat

	var results []*declcfg.DeclarativeConfig
	for _, src := range t.Sources {
		where := fmt.Sprintf("%s mirror.operators[%d]", src.ConfigPath, src.Index)
		if src.Operator.IsFullCatalog() {
			t.Unfiltered = true
			results = append(results, fbc.Clone(cat.DC))
			continue
		}
		dc, err := Filter(ctx, cat.DC, src.Operator)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		results = append(results, dc)
		for _, w := range ambiguousExactVersions(cat.DC, src.Operator) {
			t.Warnings = append(t.Warnings, where+": "+w)
		}
	}
	if len(results) == 1 {
		t.DC = results[0]
	} else {
		merged, warnings := fbc.Merge(results)
		t.DC = merged
		t.Warnings = append(t.Warnings, warnings...)
	}
	if err := t.repairChannels(opts); err != nil {
		return err
	}
	if err := fbc.Validate(*t.DC); err != nil {
		return fmt.Errorf("merged catalog is not valid: %w", err)
	}
	return nil
}

// Filter applies one ImageSetConfiguration operator entry to a catalog using
// the same library and conversion as oc-mirror v2. The source is not modified.
func Filter(ctx context.Context, src *declcfg.DeclarativeConfig, op isc.Operator) (*declcfg.DeclarativeConfig, error) {
	cfg, err := op.FilterConfig()
	if err != nil {
		return nil, err
	}
	f := filter.NewMirrorFilter(cfg, filter.InFull(op.Full))
	// The filter trims channel entries in place, so always give it a copy.
	dc, err := f.FilterCatalog(ctx, fbc.Clone(src))
	if err != nil {
		return nil, fmt.Errorf("failed to filter catalog: %w", err)
	}
	return dc, nil
}

// repairChannels checks every merged channel for a single valid upgrade graph
// and, when allowed, fills holes using the original channel from the catalog.
func (t *Target) repairChannels(opts Options) error {
	orig := fbc.IndexChannels(t.Catalog.DC)
	origBundles := fbc.IndexBundles(t.Catalog.DC)
	have := map[string]map[string]bool{}
	for _, b := range t.DC.Bundles {
		if have[b.Package] == nil {
			have[b.Package] = map[string]bool{}
		}
		have[b.Package][b.Name] = true
	}
	var failures []string
	changed := false
	for i := range t.DC.Channels {
		ch := &t.DC.Channels[i]
		problem := fbc.ChannelProblem(*ch)
		if problem == "" {
			continue
		}
		gap := Gap{Package: ch.Package, Channel: ch.Name, Problem: problem}
		if !opts.FillGaps {
			t.Gaps = append(t.Gaps, gap)
			failures = append(failures, fmt.Sprintf("%s/%s: %s", ch.Package, ch.Name, problem))
			continue
		}
		full, ok := orig[ch.Package][ch.Name]
		if !ok {
			return fmt.Errorf("channel %s/%s not found in source catalog", ch.Package, ch.Name)
		}
		added, err := fbc.FillChannel(ch, full)
		if err != nil {
			return fmt.Errorf("channel %s/%s: %w", ch.Package, ch.Name, err)
		}
		for _, name := range added {
			if have[ch.Package][name] {
				continue
			}
			b, ok := origBundles[ch.Package][name]
			if !ok {
				return fmt.Errorf("channel %s/%s: bundle %s missing from source catalog", ch.Package, ch.Name, name)
			}
			t.DC.Bundles = append(t.DC.Bundles, b)
			if have[ch.Package] == nil {
				have[ch.Package] = map[string]bool{}
			}
			have[ch.Package][name] = true
		}
		gap.Filled = added
		t.Gaps = append(t.Gaps, gap)
		if len(added) > 0 {
			fbc.AddBundleDeprecations(t.DC, t.Catalog.DC, ch.Package, added)
			changed = true
		}
		if p := fbc.ChannelProblem(*ch); p != "" {
			return fmt.Errorf("channel %s/%s still invalid after filling: %s", ch.Package, ch.Name, p)
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("merged selections leave gaps in upgrade graphs (rerun with gap filling enabled to repair):\n  %s", strings.Join(failures, "\n  "))
	}
	if changed {
		slices.SortFunc(t.DC.Bundles, fbc.CompareBundles)
	}
	return nil
}
