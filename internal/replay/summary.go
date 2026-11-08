package replay

import (
	"regexp"
	"sort"

	"github.com/blang/semver/v4"
	"github.com/operator-framework/operator-registry/alpha/declcfg"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/fbc"
)

// TargetSummary is the JSON-serialisable report for one target catalog.
type TargetSummary struct {
	Target        string           `json:"target"`
	Catalog       string           `json:"catalog"`
	CatalogDigest string           `json:"catalogDigest,omitempty"`
	CatalogDir    string           `json:"catalogDir"`
	Sources       []string         `json:"sources"`
	Unfiltered    bool             `json:"unfiltered,omitempty"`
	OutputDir     string           `json:"outputDir,omitempty"`
	Packages      []PackageSummary `json:"packages"`
	BundleCount   int              `json:"bundleCount"`
	ImageCount    int              `json:"imageCount"`
	Gaps          []Gap            `json:"gaps,omitempty"`
	Warnings      []string         `json:"warnings,omitempty"`
}

// PackageSummary describes one package in the output catalog.
type PackageSummary struct {
	Name           string           `json:"name"`
	DefaultChannel string           `json:"defaultChannel"`
	Channels       []ChannelSummary `json:"channels"`
	Bundles        []string         `json:"bundles"`
}

// ChannelSummary describes one channel in the output catalog.
type ChannelSummary struct {
	Name       string `json:"name"`
	Head       string `json:"head"`
	Entries    int    `json:"entries"`
	MinVersion string `json:"minVersion,omitempty"`
	MaxVersion string `json:"maxVersion,omitempty"`
}

// Summary builds the report for a target.
func (t *Target) Summary() TargetSummary {
	s := TargetSummary{
		Target:      t.Key,
		Catalog:     t.CatalogRef,
		CatalogDir:  t.Catalog.Dir,
		Unfiltered:  t.Unfiltered,
		Gaps:        t.Gaps,
		Warnings:    t.Warnings,
		BundleCount: len(t.DC.Bundles),
		ImageCount:  len(Images(t.DC)),
	}
	s.CatalogDigest = t.Catalog.Digest
	for _, src := range t.Sources {
		s.Sources = append(s.Sources, src.ConfigPath)
	}
	s.Sources = uniq(s.Sources)
	s.Packages = Packages(t.DC)
	return s
}

// Packages summarises every package in dc.
func Packages(dc *declcfg.DeclarativeConfig) []PackageSummary {
	bundles := fbc.IndexBundles(dc)
	byPkg := map[string]*PackageSummary{}
	var names []string
	for _, p := range dc.Packages {
		byPkg[p.Name] = &PackageSummary{Name: p.Name, DefaultChannel: p.DefaultChannel}
		names = append(names, p.Name)
	}
	for _, ch := range dc.Channels {
		ps := byPkg[ch.Package]
		if ps == nil {
			continue
		}
		cs := ChannelSummary{Name: ch.Name, Entries: len(ch.Entries)}
		var lo, hi *semver.Version
		for _, e := range ch.Entries {
			b, ok := bundles[ch.Package][e.Name]
			if !ok {
				continue
			}
			v, err := fbc.BundleVersion(b)
			if err != nil {
				continue
			}
			if lo == nil || v.LT(*lo) {
				vv := v
				lo = &vv
			}
			if hi == nil || v.GT(*hi) {
				vv := v
				hi = &vv
			}
		}
		if lo != nil {
			cs.MinVersion, cs.MaxVersion = lo.String(), hi.String()
		}
		cs.Head = headName(ch)
		ps.Channels = append(ps.Channels, cs)
	}
	for _, b := range dc.Bundles {
		if ps := byPkg[b.Package]; ps != nil {
			ps.Bundles = append(ps.Bundles, b.Name)
		}
	}
	sort.Strings(names)
	out := make([]PackageSummary, 0, len(names))
	for _, n := range names {
		ps := byPkg[n]
		sort.Slice(ps.Channels, func(i, j int) bool { return ps.Channels[i].Name < ps.Channels[j].Name })
		sort.Strings(ps.Bundles)
		out = append(out, *ps)
	}
	return out
}

func headName(ch declcfg.Channel) string {
	incoming := map[string]bool{}
	for _, e := range ch.Entries {
		if e.Replaces != "" {
			incoming[e.Replaces] = true
		}
		for _, s := range e.Skips {
			incoming[s] = true
		}
	}
	for _, e := range ch.Entries {
		if !incoming[e.Name] {
			return e.Name
		}
	}
	return ""
}

// Images returns the sorted, de-duplicated bundle and related images in dc.
func Images(dc *declcfg.DeclarativeConfig) []string {
	set := map[string]bool{}
	for _, b := range dc.Bundles {
		if b.Image != "" {
			set[b.Image] = true
		}
		for _, ri := range b.RelatedImages {
			if ri.Image != "" {
				set[ri.Image] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for img := range set {
		out = append(out, img)
	}
	sort.Strings(out)
	return out
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// DirName turns a target key into a safe directory name.
func DirName(key string) string {
	return unsafeChars.ReplaceAllString(key, "_")
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
