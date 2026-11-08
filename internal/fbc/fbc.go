// Package fbc holds helpers for manipulating declarative (file-based) catalogs.
package fbc

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
)

// Clone returns a copy of dc that is safe to filter: every slice the
// catalog-filter library mutates in place is duplicated.
func Clone(dc *declcfg.DeclarativeConfig) *declcfg.DeclarativeConfig {
	out := &declcfg.DeclarativeConfig{
		Packages: slices.Clone(dc.Packages),
		Bundles:  slices.Clone(dc.Bundles),
		Others:   slices.Clone(dc.Others),
	}
	if dc.Channels != nil {
		out.Channels = make([]declcfg.Channel, len(dc.Channels))
		for i, ch := range dc.Channels {
			ch.Entries = slices.Clone(ch.Entries)
			out.Channels[i] = ch
		}
	}
	if dc.Deprecations != nil {
		out.Deprecations = make([]declcfg.Deprecation, len(dc.Deprecations))
		for i, d := range dc.Deprecations {
			d.Entries = slices.Clone(d.Entries)
			out.Deprecations[i] = d
		}
	}
	return out
}

// CompareBundles orders bundles the same way catalog-filter does.
func CompareBundles(a, b declcfg.Bundle) int {
	if c := strings.Compare(a.Package, b.Package); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

func compareChannels(a, b declcfg.Channel) int {
	if c := strings.Compare(a.Package, b.Package); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// IndexChannels maps package -> channel name -> channel.
func IndexChannels(dc *declcfg.DeclarativeConfig) map[string]map[string]declcfg.Channel {
	out := map[string]map[string]declcfg.Channel{}
	for _, ch := range dc.Channels {
		if out[ch.Package] == nil {
			out[ch.Package] = map[string]declcfg.Channel{}
		}
		out[ch.Package][ch.Name] = ch
	}
	return out
}

// IndexBundles maps package -> bundle name -> bundle.
func IndexBundles(dc *declcfg.DeclarativeConfig) map[string]map[string]declcfg.Bundle {
	out := map[string]map[string]declcfg.Bundle{}
	for _, b := range dc.Bundles {
		if out[b.Package] == nil {
			out[b.Package] = map[string]declcfg.Bundle{}
		}
		out[b.Package][b.Name] = b
	}
	return out
}

// BundleVersion returns the olm.package version of a bundle.
func BundleVersion(b declcfg.Bundle) (semver.Version, error) {
	for _, p := range b.Properties {
		if p.Type != property.TypePackage {
			continue
		}
		var pkg property.Package
		if err := json.Unmarshal(p.Value, &pkg); err != nil {
			return semver.Version{}, err
		}
		return semver.Parse(pkg.Version)
	}
	return semver.Version{}, fmt.Errorf("bundle %q has no olm.package property", b.Name)
}

// Merge unions several filtered catalogs that came from the same source
// catalog. The first occurrence of a package wins for package-level fields.
func Merge(dcs []*declcfg.DeclarativeConfig) (*declcfg.DeclarativeConfig, []string) {
	var warnings []string
	out := &declcfg.DeclarativeConfig{}

	pkgs := map[string]int{}
	type chAcc struct {
		idx  int
		seen map[string]bool
	}
	chans := map[string]*chAcc{}
	bundles := map[string]bool{}
	deps := map[string]int{}
	depSeen := map[string]bool{}
	others := map[string]bool{}

	for _, dc := range dcs {
		for _, p := range dc.Packages {
			if i, ok := pkgs[p.Name]; ok {
				if out.Packages[i].DefaultChannel != p.DefaultChannel {
					warnings = append(warnings, fmt.Sprintf("package %s: configs disagree on default channel (%q vs %q); keeping %q",
						p.Name, out.Packages[i].DefaultChannel, p.DefaultChannel, out.Packages[i].DefaultChannel))
				}
				continue
			}
			pkgs[p.Name] = len(out.Packages)
			out.Packages = append(out.Packages, p)
		}
		for _, ch := range dc.Channels {
			key := ch.Package + "/" + ch.Name
			acc, ok := chans[key]
			if !ok {
				acc = &chAcc{idx: len(out.Channels), seen: map[string]bool{}}
				chans[key] = acc
				c := ch
				c.Entries = nil
				out.Channels = append(out.Channels, c)
			}
			for _, e := range ch.Entries {
				if acc.seen[e.Name] {
					continue
				}
				acc.seen[e.Name] = true
				out.Channels[acc.idx].Entries = append(out.Channels[acc.idx].Entries, e)
			}
		}
		for _, b := range dc.Bundles {
			key := b.Package + "/" + b.Name
			if bundles[key] {
				continue
			}
			bundles[key] = true
			out.Bundles = append(out.Bundles, b)
		}
		for _, d := range dc.Deprecations {
			i, ok := deps[d.Package]
			if !ok {
				i = len(out.Deprecations)
				deps[d.Package] = i
				out.Deprecations = append(out.Deprecations, declcfg.Deprecation{Schema: d.Schema, Package: d.Package})
			}
			for _, e := range d.Entries {
				key := d.Package + "|" + e.Reference.Schema + "|" + e.Reference.Name
				if depSeen[key] {
					continue
				}
				depSeen[key] = true
				out.Deprecations[i].Entries = append(out.Deprecations[i].Entries, e)
			}
		}
		for _, m := range dc.Others {
			key := m.Schema + "|" + m.Package + "|" + m.Name + "|" + string(m.Blob)
			if others[key] {
				continue
			}
			others[key] = true
			out.Others = append(out.Others, m)
		}
	}
	slices.SortFunc(out.Packages, func(a, b declcfg.Package) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(out.Channels, compareChannels)
	slices.SortFunc(out.Bundles, CompareBundles)
	return out, warnings
}

// ChannelProblem returns "" when the channel forms a valid upgrade graph per
// opm's model validation (single head, no cycles, no stranded bundles), or a
// description of the problem. It is a port of model.Channel.validateReplacesChain.
func ChannelProblem(ch declcfg.Channel) string {
	if len(ch.Entries) == 0 {
		return "channel has no entries"
	}
	entries := map[string]declcfg.ChannelEntry{}
	incoming := map[string]int{}
	skipped := map[string]bool{}
	for _, e := range ch.Entries {
		entries[e.Name] = e
		if e.Replaces != "" {
			incoming[e.Replaces]++
		}
		for _, s := range e.Skips {
			incoming[s]++
			skipped[s] = true
		}
	}
	var heads []string
	for _, e := range ch.Entries {
		if incoming[e.Name] == 0 {
			heads = append(heads, e.Name)
		}
	}
	sort.Strings(heads)
	switch {
	case len(heads) == 0:
		return "no channel head found (cycle)"
	case len(heads) > 1:
		return "multiple channel heads: " + strings.Join(heads, ", ")
	}
	inChain := map[string]bool{heads[0]: true}
	cur, ok := entries[heads[0]]
	for ok {
		if skipped[cur.Replaces] {
			break
		}
		if inChain[cur.Replaces] {
			return "cycle in replaces chain at " + cur.Replaces
		}
		inChain[cur.Replaces] = true
		cur, ok = entries[cur.Replaces]
	}
	var stranded []string
	for name := range entries {
		if !inChain[name] && !skipped[name] {
			stranded = append(stranded, name)
		}
	}
	if len(stranded) > 0 {
		sort.Strings(stranded)
		return "stranded bundles: " + strings.Join(stranded, ", ")
	}
	return ""
}

// FillChannel adds entries from full (the original, unfiltered channel) to ch
// so that every entry ch already has is connected through one contiguous
// stretch of the original replaces chain. It returns the names it added.
func FillChannel(ch *declcfg.Channel, full declcfg.Channel) ([]string, error) {
	fullEntries := map[string]declcfg.ChannelEntry{}
	for _, e := range full.Entries {
		fullEntries[e.Name] = e
	}
	if p := ChannelProblem(full); strings.HasPrefix(p, "multiple") || strings.HasPrefix(p, "no channel head") {
		return nil, fmt.Errorf("source channel itself is invalid: %s", p)
	}
	head := headOf(full)

	// Walk the full replaces chain from the head.
	var chain []string
	pos := map[string]int{}
	for name := head; name != ""; {
		if _, dup := pos[name]; dup {
			break
		}
		e, ok := fullEntries[name]
		if !ok {
			break
		}
		pos[name] = len(chain)
		chain = append(chain, name)
		name = e.Replaces
	}
	// Bundles off the chain are reachable through the first chain member that skips them.
	skipPos := map[string]int{}
	for i, name := range chain {
		for _, s := range fullEntries[name].Skips {
			if _, ok := skipPos[s]; !ok {
				skipPos[s] = i
			}
		}
	}

	have := map[string]bool{}
	top, bottom := len(chain), -1
	for _, e := range ch.Entries {
		have[e.Name] = true
		p, ok := pos[e.Name]
		if !ok {
			p, ok = skipPos[e.Name]
		}
		if !ok {
			return nil, fmt.Errorf("bundle %s is not reachable from the source channel head", e.Name)
		}
		top = min(top, p)
		bottom = max(bottom, p)
	}
	var added []string
	for i := top; i <= bottom; i++ {
		name := chain[i]
		if have[name] {
			continue
		}
		ch.Entries = append(ch.Entries, fullEntries[name])
		have[name] = true
		added = append(added, name)
	}
	return added, nil
}

func headOf(ch declcfg.Channel) string {
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

// AddBundleDeprecations copies deprecation entries for the named bundles from
// orig into dc (used after gap filling adds bundles).
func AddBundleDeprecations(dc, orig *declcfg.DeclarativeConfig, pkg string, names []string) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var add []declcfg.DeprecationEntry
	for _, d := range orig.Deprecations {
		if d.Package != pkg {
			continue
		}
		for _, e := range d.Entries {
			if e.Reference.Schema == declcfg.SchemaBundle && want[e.Reference.Name] {
				add = append(add, e)
			}
		}
	}
	if len(add) == 0 {
		return
	}
	for i := range dc.Deprecations {
		if dc.Deprecations[i].Package == pkg {
			dc.Deprecations[i].Entries = append(dc.Deprecations[i].Entries, add...)
			return
		}
	}
	dc.Deprecations = append(dc.Deprecations, declcfg.Deprecation{Schema: declcfg.SchemaDeprecation, Package: pkg, Entries: add})
}
