// Package deps checks that the operator bundles an oc-mirror v2
// ImageSetConfiguration selects have their OLM dependencies mirrored too, and
// proposes ImageSetConfiguration additions for anything missing.
//
// Declared dependencies are read from catalog metadata (olm.package.required,
// olm.gvk.required, olm.constraint). Dependencies an operator installs at
// runtime without declaring them come from a hand-maintained list (known.yaml).
package deps

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/catalog"
	"github.com/dmc5179/olm-catalog-reconstruct/internal/isc"
	"github.com/dmc5179/olm-catalog-reconstruct/internal/replay"
)

// Finding statuses.
const (
	StatusAdded      = "added"      // missing; a fix was added to the suggested additions
	StatusUnresolved = "unresolved" // missing; nothing in the loaded catalogs satisfies it
	StatusPlatform   = "platform"   // required API looks cluster-provided; no package provides it
	StatusManual     = "manual"     // cannot be evaluated automatically (CEL)
	StatusConflict   = "conflict"   // a 'not' constraint is violated; adding content cannot fix it
	// StatusSkipped marks an unmet dependency of a bundle that a newer selected
	// bundle in the same channel skips over (skips / skipRange). OLM upgrades
	// past such bundles, so they are not install targets and their dependencies
	// only matter if someone installs that exact version (startingCSV).
	StatusSkipped = "skipped"
)

// Finding is one dependency that was not satisfied by the configuration as written.
type Finding struct {
	Status      string   `json:"status"`
	Kind        string   `json:"kind"`
	Catalog     string   `json:"catalog"`
	Package     string   `json:"package"`
	Bundle      string   `json:"bundle"`
	Requirement string   `json:"requirement"`
	Resolution  string   `json:"resolution,omitempty"`
	Source      string   `json:"source,omitempty"`
	Have        []string `json:"have,omitempty"`
	// Via is set when the requiring bundle was itself added to satisfy another dependency.
	Via string `json:"via,omitempty"`
	// Hint explains why the requiring bundle is mirrored when that is likely
	// unintended (its package was listed without channels).
	Hint string `json:"hint,omitempty"`
}

// Result is the outcome of a dependency check.
type Result struct {
	Findings  []Finding                  `json:"findings"`
	Checked   int                        `json:"bundlesChecked"`
	Satisfied int                        `json:"requirementsSatisfied"`
	Additions *isc.ImageSetConfiguration `json:"additions,omitempty"`
	Targets   []*replay.Target           `json:"-"`
	added     map[string]map[string]*isc.IncludePackage
}

// HasProblems reports whether anything is missing, unresolved or conflicting.
func (r *Result) HasProblems() bool {
	for _, f := range r.Findings {
		switch f.Status {
		case StatusAdded, StatusUnresolved, StatusConflict:
			return true
		}
	}
	return false
}

type gvk struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

func (g gvk) String() string {
	group := g.Group
	if group == "" {
		group = "core"
	}
	return fmt.Sprintf("%s/%s %s", group, g.Version, g.Kind)
}

type bundleInfo struct {
	cat         string
	b           declcfg.Bundle
	version     semver.Version
	hasVersion  bool
	versionStr  string
	pkgReqs     []property.PackageRequired
	gvkReqs     []gvk
	provides    []gvk
	constraints []constraint
	channels    []string
}

func (bi *bundleInfo) key() string { return bi.cat + "|" + bi.b.Package + "|" + bi.b.Name }

type catIndex struct {
	cat       *catalog.Catalog
	bundles   map[string]map[string]*bundleInfo
	channels  map[string]map[string]declcfg.Channel
	defaults  map[string]string
	providers map[gvk][]*bundleInfo
}

func indexCatalog(c *catalog.Catalog) (*catIndex, error) {
	ci := &catIndex{
		cat:       c,
		bundles:   map[string]map[string]*bundleInfo{},
		channels:  map[string]map[string]declcfg.Channel{},
		defaults:  map[string]string{},
		providers: map[gvk][]*bundleInfo{},
	}
	for _, p := range c.DC.Packages {
		ci.defaults[p.Name] = p.DefaultChannel
	}
	for _, b := range c.DC.Bundles {
		bi := &bundleInfo{cat: c.Ref, b: b}
		for _, p := range b.Properties {
			switch p.Type {
			case property.TypePackage:
				var v property.Package
				if err := json.Unmarshal(p.Value, &v); err == nil {
					bi.versionStr = v.Version
					if sv, err := semver.Parse(v.Version); err == nil {
						bi.version, bi.hasVersion = sv, true
					}
				}
			case property.TypePackageRequired:
				var v property.PackageRequired
				if err := json.Unmarshal(p.Value, &v); err != nil {
					return nil, fmt.Errorf("bundle %s: bad %s: %w", b.Name, p.Type, err)
				}
				bi.pkgReqs = append(bi.pkgReqs, v)
			case property.TypeGVKRequired:
				var v gvk
				if err := json.Unmarshal(p.Value, &v); err != nil {
					return nil, fmt.Errorf("bundle %s: bad %s: %w", b.Name, p.Type, err)
				}
				bi.gvkReqs = append(bi.gvkReqs, v)
			case property.TypeGVK:
				var v gvk
				if err := json.Unmarshal(p.Value, &v); err == nil {
					bi.provides = append(bi.provides, v)
				}
			case property.TypeConstraint:
				c, err := parseConstraint(p.Value)
				if err != nil {
					return nil, fmt.Errorf("bundle %s: bad %s: %w", b.Name, p.Type, err)
				}
				bi.constraints = append(bi.constraints, c)
			}
		}
		if ci.bundles[b.Package] == nil {
			ci.bundles[b.Package] = map[string]*bundleInfo{}
		}
		ci.bundles[b.Package][b.Name] = bi
		for _, g := range bi.provides {
			ci.providers[g] = append(ci.providers[g], bi)
		}
	}
	for _, ch := range c.DC.Channels {
		if ci.channels[ch.Package] == nil {
			ci.channels[ch.Package] = map[string]declcfg.Channel{}
		}
		ci.channels[ch.Package][ch.Name] = ch
		for _, e := range ch.Entries {
			if bi := ci.bundles[ch.Package][e.Name]; bi != nil {
				bi.channels = append(bi.channels, ch.Name)
			}
		}
	}
	for _, m := range ci.bundles {
		for _, bi := range m {
			sort.Strings(bi.channels)
		}
	}
	return ci, nil
}

type selection struct {
	bi   *bundleInfo
	via  string // "" when selected by the user's configuration
	hint string // why a config-selected bundle is mirrored at all, when that is surprising
}

type checker struct {
	ctx      context.Context
	cats     []*catIndex
	byRef    map[string]*catIndex
	known    map[string][]KnownRequired
	selected map[string]*selection
	byPkg    map[string][]*selection
	queue    []*selection
	ranges   map[string]semver.Range
	res      *Result
	seen     map[string]bool
}

// Options configure Check.
type Options struct {
	Known []KnownDep
}

// Check replays configs, then walks the dependencies of every selected bundle.
func Check(ctx context.Context, loader *catalog.Loader, configs []*isc.ImageSetConfiguration, opts Options) (*Result, error) {
	targets, err := replay.Run(ctx, loader, configs, replay.Options{FillGaps: true})
	if err != nil {
		return nil, err
	}
	c := &checker{
		ctx:      ctx,
		byRef:    map[string]*catIndex{},
		known:    map[string][]KnownRequired{},
		selected: map[string]*selection{},
		byPkg:    map[string][]*selection{},
		ranges:   map[string]semver.Range{},
		seen:     map[string]bool{},
		res:      &Result{Targets: targets, added: map[string]map[string]*isc.IncludePackage{}},
	}
	for _, k := range opts.Known {
		c.known[k.Package] = append(c.known[k.Package], k.Requires...)
	}
	for _, t := range targets {
		if _, ok := c.byRef[t.Catalog.Ref]; ok {
			continue
		}
		ci, err := indexCatalog(t.Catalog)
		if err != nil {
			return nil, fmt.Errorf("catalog %s: %w", t.Catalog.Ref, err)
		}
		c.cats = append(c.cats, ci)
		c.byRef[t.Catalog.Ref] = ci
	}
	for _, t := range targets {
		ci := c.byRef[t.Catalog.Ref]
		for _, b := range t.DC.Bundles {
			if bi := ci.bundles[b.Package][b.Name]; bi != nil {
				c.selectBundle(bi, "")
			}
		}
	}
	// Check bundles from channels the user evidently wants first, so a
	// requirement shared with them is reported (and fixed) without a hint, and
	// hints only remain on requirements that come solely from bundles mirrored
	// because a package was listed without channels.
	scope := newChannelScope(configs)
	for _, s := range c.queue {
		s.hint = c.hint(s, scope)
	}
	slices.SortStableFunc(c.queue, func(a, b *selection) int {
		switch {
		case a.hint == "" && b.hint != "":
			return -1
		case a.hint != "" && b.hint == "":
			return 1
		}
		return 0
	})
	for len(c.queue) > 0 {
		s := c.queue[0]
		c.queue = c.queue[1:]
		c.res.Checked++
		if err := c.checkBundle(s); err != nil {
			return nil, err
		}
	}
	c.res.Additions = c.additions()
	return c.res, nil
}

func (c *checker) selectBundle(bi *bundleInfo, via string) {
	if _, ok := c.selected[bi.key()]; ok {
		return
	}
	s := &selection{bi: bi, via: via}
	c.selected[bi.key()] = s
	c.byPkg[bi.b.Package] = append(c.byPkg[bi.b.Package], s)
	c.queue = append(c.queue, s)
}

func (c *checker) parseRange(r string) (semver.Range, error) {
	if rng, ok := c.ranges[r]; ok {
		return rng, nil
	}
	rng, err := semver.ParseRange(r)
	if err != nil {
		return nil, err
	}
	c.ranges[r] = rng
	return rng, nil
}

// pkgSatisfied reports whether a selected bundle of pkg is in rng, plus the selected versions.
func (c *checker) pkgSatisfied(pkg, rng string) (bool, []string, error) {
	var match func(semver.Version) bool
	if rng != "" {
		r, err := c.parseRange(rng)
		if err != nil {
			return false, nil, fmt.Errorf("version range %q: %w", rng, err)
		}
		match = r
	}
	var have []string
	ok := false
	for _, s := range c.byPkg[pkg] {
		have = append(have, s.bi.versionStr)
		if match == nil || (s.bi.hasVersion && match(s.bi.version)) {
			ok = true
		}
	}
	sort.Strings(have)
	return ok, slices.Compact(have), nil
}

func (c *checker) gvkSatisfied(g gvk) bool {
	for _, s := range c.selected {
		if slices.Contains(s.bi.provides, g) {
			return true
		}
	}
	return false
}

func (c *checker) record(f Finding, s *selection) {
	f.Catalog, f.Package, f.Bundle, f.Via, f.Hint = s.bi.cat, s.bi.b.Package, s.bi.b.Name, s.via, s.hint
	key := f.Status + "|" + f.Kind + "|" + f.Package + "|" + f.Requirement
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.res.Findings = append(c.res.Findings, f)
}

func (c *checker) checkBundle(s *selection) error {
	bi := s.bi
	// Unmet dependencies of a bundle that a newer selected bundle skips over are
	// reported for information only and never trigger additions.
	skippedBy := c.skippedBy(s)
	resolve := func(f *Finding, fix func()) {
		if skippedBy != "" {
			f.Status = StatusSkipped
			f.Resolution = "not an install target: skipped by " + skippedBy + "; only needed if this exact version is installed"
			return
		}
		fix()
	}
	for _, r := range bi.pkgReqs {
		ok, have, err := c.pkgSatisfied(r.PackageName, r.VersionRange)
		if err != nil {
			return fmt.Errorf("%s: %w", bi.b.Name, err)
		}
		if ok {
			c.res.Satisfied++
			continue
		}
		f := Finding{Kind: property.TypePackageRequired, Requirement: pkgReq(r.PackageName, r.VersionRange), Have: have}
		resolve(&f, func() { c.fixPackage(&f, s, r.PackageName, r.VersionRange, "") })
		c.record(f, s)
	}
	for _, g := range bi.gvkReqs {
		if c.gvkSatisfied(g) {
			c.res.Satisfied++
			continue
		}
		f := Finding{Kind: property.TypeGVKRequired, Requirement: "API " + g.String()}
		resolve(&f, func() { c.fixGVK(&f, s, g) })
		c.record(f, s)
	}
	for _, con := range bi.constraints {
		switch c.eval(con) {
		case sat:
			c.res.Satisfied++
		case unknown:
			f := Finding{Status: StatusManual, Kind: property.TypeConstraint, Requirement: con.String(),
				Source: con.FailureMessage, Resolution: "CEL constraints cannot be evaluated offline; review manually"}
			resolve(&f, func() {})
			c.record(f, s)
		case unsat:
			f := Finding{Kind: property.TypeConstraint, Requirement: con.String(), Source: con.FailureMessage}
			resolve(&f, func() { c.fixConstraint(&f, s, con) })
			c.record(f, s)
		}
	}
	for _, k := range c.known[bi.b.Package] {
		ok, have, _ := c.pkgSatisfied(k.Package, "")
		if ok {
			c.res.Satisfied++
			continue
		}
		hint := ""
		if k.Channel == "same" {
			hint = c.sameChannel(s, k.Package)
		}
		f := Finding{Kind: "known (undeclared)", Requirement: pkgReq(k.Package, ""), Source: k.Source, Have: have}
		resolve(&f, func() { c.fixPackage(&f, s, k.Package, "", hint) })
		c.record(f, s)
	}
	return nil
}

// skippedBy reports which newer selected bundle skips over s in a channel they
// share, via the channel entry's skips list or skipRange. It returns "" when
// no selected bundle does.
func (c *checker) skippedBy(s *selection) string {
	ci := c.byRef[s.bi.cat]
	if ci == nil {
		return ""
	}
	pkg, name := s.bi.b.Package, s.bi.b.Name
	for _, chName := range s.bi.channels {
		ch := ci.channels[pkg][chName]
		for _, e := range ch.Entries {
			if e.Name == name {
				continue
			}
			if _, ok := c.selected[s.bi.cat+"|"+pkg+"|"+e.Name]; !ok {
				continue
			}
			other := ci.bundles[pkg][e.Name]
			if other == nil || (other.hasVersion && s.bi.hasVersion && !other.version.GT(s.bi.version)) {
				continue
			}
			if slices.Contains(e.Skips, name) {
				return fmt.Sprintf("%s (channel %s, skips)", e.Name, chName)
			}
			if e.SkipRange != "" && s.bi.hasVersion {
				if r, err := c.parseRange(e.SkipRange); err == nil && r(s.bi.version) {
					return fmt.Sprintf("%s (channel %s, skipRange %q)", e.Name, chName, e.SkipRange)
				}
			}
		}
	}
	return ""
}

// sameChannel returns the dependent package's channel matching one of the
// requiring bundle's channels, if any.
func (c *checker) sameChannel(s *selection, depPkg string) string {
	for _, ci := range c.catalogsFor(s.bi.cat) {
		for _, ch := range s.bi.channels {
			if _, ok := ci.channels[depPkg][ch]; ok {
				return ch
			}
		}
	}
	return ""
}

func (c *checker) eval(con constraint) tri {
	switch {
	case con.Package != nil:
		ok, _, err := c.pkgSatisfied(con.Package.PackageName, con.Package.VersionRange)
		if err != nil {
			return unknown
		}
		return boolTri(ok)
	case con.GVK != nil:
		return boolTri(c.gvkSatisfied(*con.GVK))
	case con.All != nil:
		res := sat
		for _, ch := range con.All.Constraints {
			switch c.eval(ch) {
			case unsat:
				return unsat
			case unknown:
				res = unknown
			}
		}
		return res
	case con.Any != nil:
		res := unsat
		for _, ch := range con.Any.Constraints {
			switch c.eval(ch) {
			case sat:
				return sat
			case unknown:
				res = unknown
			}
		}
		return res
	case con.Not != nil:
		res := sat
		for _, ch := range con.Not.Constraints {
			switch c.eval(ch) {
			case sat:
				return unsat
			case unknown:
				res = unknown
			}
		}
		return res
	}
	return unknown
}

func boolTri(b bool) tri {
	if b {
		return sat
	}
	return unsat
}

// fixConstraint tries to satisfy an unsatisfied constraint by adding content.
func (c *checker) fixConstraint(f *Finding, s *selection, con constraint) bool {
	switch {
	case con.Package != nil:
		return c.fixPackage(f, s, con.Package.PackageName, con.Package.VersionRange, "")
	case con.GVK != nil:
		return c.fixGVK(f, s, *con.GVK)
	case con.All != nil:
		var parts []string
		all := true
		for _, ch := range con.All.Constraints {
			if c.eval(ch) == sat {
				continue
			}
			sub := Finding{}
			if c.fixConstraint(&sub, s, ch) {
				parts = append(parts, sub.Resolution)
			} else {
				all = false
				parts = append(parts, "cannot satisfy "+ch.String())
			}
		}
		f.Resolution = strings.Join(parts, "; ")
		f.Status = StatusAdded
		if !all {
			f.Status = StatusUnresolved
		}
		return all
	case con.Any != nil:
		for _, ch := range con.Any.Constraints {
			sub := Finding{}
			if c.fixConstraint(&sub, s, ch) {
				f.Status, f.Resolution = StatusAdded, sub.Resolution
				return true
			}
		}
		f.Status, f.Resolution = StatusUnresolved, "no alternative in any(...) can be satisfied from the loaded catalogs"
		return false
	case con.Not != nil:
		f.Status, f.Resolution = StatusConflict, "a selected bundle violates this 'not' constraint; remove the conflicting content"
		return false
	}
	f.Status, f.Resolution = StatusManual, "constraint type not evaluated; review manually"
	return false
}

// catalogsFor returns catalogs with the given ref first.
func (c *checker) catalogsFor(ref string) []*catIndex {
	out := []*catIndex{}
	if ci, ok := c.byRef[ref]; ok {
		out = append(out, ci)
	}
	for _, ci := range c.cats {
		if ci.cat.Ref != ref {
			out = append(out, ci)
		}
	}
	return out
}

// fixPackage selects the highest bundle of pkg within rng and adds it.
func (c *checker) fixPackage(f *Finding, s *selection, pkg, rng, channelHint string) bool {
	var match func(semver.Version) bool
	if rng != "" {
		r, err := c.parseRange(rng)
		if err != nil {
			f.Status, f.Resolution = StatusUnresolved, fmt.Sprintf("invalid version range %q", rng)
			return false
		}
		match = r
	}
	for _, ci := range c.catalogsFor(s.bi.cat) {
		var best *bundleInfo
		for _, bi := range ci.bundles[pkg] {
			if len(bi.channels) == 0 || (match != nil && (!bi.hasVersion || !match(bi.version))) {
				continue
			}
			if best == nil || newer(bi, best) {
				best = bi
			}
		}
		if best != nil {
			return c.add(f, s, ci, best, channelHint)
		}
	}
	f.Status = StatusUnresolved
	f.Resolution = fmt.Sprintf("no bundle of %s in the loaded catalogs satisfies %q", pkg, rng)
	return false
}

// fixGVK finds a package that provides g and adds it.
func (c *checker) fixGVK(f *Finding, s *selection, g gvk) bool {
	for _, ci := range c.catalogsFor(s.bi.cat) {
		provs := ci.providers[g]
		if len(provs) == 0 {
			continue
		}
		// Prefer a provider that is the head of its package's default channel,
		// then the highest version.
		var best *bundleInfo
		bestHead := false
		for _, p := range provs {
			if len(p.channels) == 0 {
				continue
			}
			isHead := c.isHead(ci, p, ci.defaults[p.b.Package])
			switch {
			case best == nil,
				isHead && !bestHead,
				isHead == bestHead && p.b.Package == best.b.Package && newer(p, best),
				isHead == bestHead && p.b.Package < best.b.Package:
				best, bestHead = p, isHead
			}
		}
		if best != nil {
			return c.add(f, s, ci, best, "")
		}
	}
	if platformGroup(g.Group) {
		f.Status = StatusPlatform
		f.Resolution = "no package in the loaded catalogs provides this API; it is normally provided by OpenShift itself"
	} else {
		f.Status = StatusUnresolved
		f.Resolution = "no package in the loaded catalogs provides this API"
	}
	return false
}

func platformGroup(g string) bool {
	switch g {
	case "", "apps", "batch", "policy", "autoscaling", "extensions", "operators.coreos.com", "monitoring.coreos.com":
		return true
	}
	return strings.HasSuffix(g, ".k8s.io") || strings.HasSuffix(g, ".openshift.io") || g == "k8s.io" || g == "openshift.io"
}

func (c *checker) isHead(ci *catIndex, bi *bundleInfo, ch string) bool {
	channel, ok := ci.channels[bi.b.Package][ch]
	if !ok {
		return false
	}
	return headOf(channel) == bi.b.Name
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

// add builds the ImageSetConfiguration entry that selects target, runs it
// through the real filter to learn exactly which bundles it brings in, and
// queues those bundles for their own dependency checks.
func (c *checker) add(f *Finding, s *selection, ci *catIndex, target *bundleInfo, channelHint string) bool {
	pkg := target.b.Package
	ch := ""
	switch {
	case channelHint != "" && slices.Contains(target.channels, channelHint):
		ch = channelHint
	case slices.Contains(target.channels, ci.defaults[pkg]):
		ch = ci.defaults[pkg]
	default:
		ch = target.channels[0]
	}
	if channelHint != "" && ch != channelHint {
		// The hinted channel exists but the chosen bundle isn't in it; prefer the hinted channel's head.
		if channel, ok := ci.channels[pkg][channelHint]; ok {
			if hb := ci.bundles[pkg][headOf(channel)]; hb != nil {
				target, ch = hb, channelHint
			}
		}
	}
	ic := isc.IncludeChannel{Name: ch}
	desc := fmt.Sprintf("add %s channel %s (head %s)", pkg, ch, target.versionStr)
	if !c.isHead(ci, target, ch) {
		ic.MinVersion = target.versionStr
		if c.uniqueVersion(ci, target) {
			ic.MaxVersion = target.versionStr
			desc = fmt.Sprintf("add %s channel %s version %s", pkg, ch, target.versionStr)
		} else {
			// minVersion == maxVersion would hit catalog-filter's random pick among
			// bundles that share this version, so mirror from here to the head.
			desc = fmt.Sprintf("add %s channel %s from version %s to head", pkg, ch, target.versionStr)
		}
	}
	ip := isc.IncludePackage{Name: pkg, Channels: []isc.IncludeChannel{ic}}
	if ch != ci.defaults[pkg] {
		ip.DefaultChannel = ch
	}
	op := isc.Operator{Catalog: ci.cat.Ref, IncludeConfig: isc.IncludeConfig{Packages: []isc.IncludePackage{ip}}}
	dc, err := replay.Filter(c.ctx, ci.cat.DC, op)
	if err != nil {
		f.Status, f.Resolution = StatusUnresolved, fmt.Sprintf("could not build a selection for %s: %v", pkg, err)
		return false
	}
	via := s.bi.b.Name
	if s.via != "" {
		via = s.via + " -> " + via
	}
	for _, b := range dc.Bundles {
		if bi := ci.bundles[b.Package][b.Name]; bi != nil {
			c.selectBundle(bi, via)
		}
	}
	c.mergeAddition(ci.cat.Ref, ip)
	f.Status, f.Resolution = StatusAdded, desc
	return true
}

func (c *checker) mergeAddition(ref string, ip isc.IncludePackage) {
	if c.res.added[ref] == nil {
		c.res.added[ref] = map[string]*isc.IncludePackage{}
	}
	cur, ok := c.res.added[ref][ip.Name]
	if !ok {
		cp := ip
		c.res.added[ref][ip.Name] = &cp
		return
	}
	for _, ch := range ip.Channels {
		if !slices.ContainsFunc(cur.Channels, func(x isc.IncludeChannel) bool { return x.Name == ch.Name }) {
			cur.Channels = append(cur.Channels, ch)
		}
	}
}

func (c *checker) additions() *isc.ImageSetConfiguration {
	if len(c.res.added) == 0 {
		return nil
	}
	out := &isc.ImageSetConfiguration{}
	out.Kind, out.APIVersion = isc.Kind, isc.APIVersion
	for _, ci := range c.cats {
		pkgs := c.res.added[ci.cat.Ref]
		if len(pkgs) == 0 {
			continue
		}
		op := isc.Operator{Catalog: ci.cat.Ref}
		names := make([]string, 0, len(pkgs))
		for n := range pkgs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			op.Packages = append(op.Packages, *pkgs[n])
		}
		out.Mirror.Operators = append(out.Mirror.Operators, op)
	}
	return out
}

// newer orders bundles by version, then name, so choices are deterministic.
func newer(a, b *bundleInfo) bool {
	switch {
	case a.hasVersion && !b.hasVersion:
		return true
	case !a.hasVersion && b.hasVersion:
		return false
	case a.hasVersion && b.hasVersion && !a.version.EQ(b.version):
		return a.version.GT(b.version)
	}
	return a.b.Name > b.b.Name
}

// uniqueVersion reports whether no other bundle in the package shares the
// target's version (semver equality ignores build metadata).
func (c *checker) uniqueVersion(ci *catIndex, target *bundleInfo) bool {
	if !target.hasVersion {
		return false
	}
	for _, bi := range ci.bundles[target.b.Package] {
		if bi != target && bi.hasVersion && bi.version.EQ(target.version) {
			return false
		}
	}
	return true
}
