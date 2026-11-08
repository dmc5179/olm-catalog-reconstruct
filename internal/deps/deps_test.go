package deps

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/catalog"
	"github.com/dmc5179/olm-catalog-reconstruct/internal/isc"
)

const catRef = "example.com/test/index:v1"

// pkg writes a one-channel package whose single bundle has the given extra properties.
func pkg(name, channel, version, props string) string {
	b := name + ".v" + version
	if props != "" {
		props = "," + props
	}
	return `{"schema":"olm.package","name":"` + name + `","defaultChannel":"` + channel + `"}
{"schema":"olm.channel","package":"` + name + `","name":"` + channel + `","entries":[{"name":"` + b + `"}]}
{"schema":"olm.bundle","package":"` + name + `","name":"` + b + `","image":"quay.io/x/` + name + `:` + version + `","properties":[{"type":"olm.package","value":{"packageName":"` + name + `","version":"` + version + `"}}` + props + `]}
`
}

func setup(t *testing.T, pkgs map[string]string) *catalog.Loader {
	t.Helper()
	dir := t.TempDir()
	for name, content := range pkgs {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "catalog.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := catalog.NewLoader()
	l.Overrides[catRef] = dir
	return l
}

func cfg(names ...string) []*isc.ImageSetConfiguration {
	op := isc.Operator{Catalog: catRef}
	for _, n := range names {
		op.Packages = append(op.Packages, isc.IncludePackage{Name: n})
	}
	return []*isc.ImageSetConfiguration{{Path: "test.yaml", Mirror: isc.Mirror{Operators: []isc.Operator{op}}}}
}

func addedPackages(r *Result) []string {
	var out []string
	if r.Additions == nil {
		return out
	}
	for _, op := range r.Additions.Mirror.Operators {
		for _, p := range op.Packages {
			out = append(out, p.Name)
		}
	}
	slices.Sort(out)
	return out
}

func findings(r *Result, status string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Status == status {
			out = append(out, f)
		}
	}
	return out
}

func TestTransitivePackageAndGVK(t *testing.T) {
	l := setup(t, map[string]string{
		// app -> lib (package, range) -> provider of example.com/v1 Widget (gvk)
		"app":      pkg("app", "stable", "1.0.0", `{"type":"olm.package.required","value":{"packageName":"lib","versionRange":">=2.0.0 <3.0.0"}}`),
		"lib":      pkg("lib", "stable", "2.1.0", `{"type":"olm.gvk.required","value":{"group":"example.com","version":"v1","kind":"Widget"}}`),
		"provider": pkg("provider", "stable", "0.1.0", `{"type":"olm.gvk","value":{"group":"example.com","version":"v1","kind":"Widget"}}`),
	})
	r, err := Check(context.Background(), l, cfg("app"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := addedPackages(r); !slices.Equal(got, []string{"lib", "provider"}) {
		t.Fatalf("added %v", got)
	}
	var gvkFinding *Finding
	for i := range r.Findings {
		if r.Findings[i].Kind == "olm.gvk.required" {
			gvkFinding = &r.Findings[i]
		}
	}
	if gvkFinding == nil || gvkFinding.Via != "app.v1.0.0" {
		t.Fatalf("expected gvk finding pulled in via app.v1.0.0, got %+v", gvkFinding)
	}
}

func TestVersionRangeUnresolved(t *testing.T) {
	l := setup(t, map[string]string{
		"app": pkg("app", "stable", "1.0.0", `{"type":"olm.package.required","value":{"packageName":"lib","versionRange":"<2.0.0"}}`),
		"lib": pkg("lib", "stable", "2.1.0", ""),
	})
	r, err := Check(context.Background(), l, cfg("app", "lib"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	u := findings(r, StatusUnresolved)
	if len(u) != 1 || !slices.Equal(u[0].Have, []string{"2.1.0"}) {
		t.Fatalf("expected one unresolved finding with have=[2.1.0], got %+v", r.Findings)
	}
	if !r.HasProblems() {
		t.Fatal("HasProblems should be true")
	}
}

func TestConstraints(t *testing.T) {
	anyOf := `{"type":"olm.constraint","value":{"failureMessage":"needs a backend","any":{"constraints":[{"package":{"packageName":"missing-pkg","versionRange":">=1.0.0"}},{"package":{"packageName":"backend","versionRange":">=1.0.0"}}]}}}`
	notOf := `{"type":"olm.constraint","value":{"not":{"constraints":[{"package":{"packageName":"legacy","versionRange":">=0.0.0"}}]}}}`
	cel := `{"type":"olm.constraint","value":{"cel":{"rule":"properties.exists(p, p.type == \"certified\")"}}}`
	l := setup(t, map[string]string{
		"app":     pkg("app", "stable", "1.0.0", anyOf+","+notOf+","+cel),
		"backend": pkg("backend", "stable", "1.2.0", ""),
		"legacy":  pkg("legacy", "stable", "0.5.0", ""),
	})
	r, err := Check(context.Background(), l, cfg("app", "legacy"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := addedPackages(r); !slices.Equal(got, []string{"backend"}) {
		t.Fatalf("any(): expected backend to be added, got %v", got)
	}
	if len(findings(r, StatusConflict)) != 1 {
		t.Fatalf("not(): expected one conflict, got %+v", r.Findings)
	}
	if m := findings(r, StatusManual); len(m) != 1 || !strings.HasPrefix(m[0].Requirement, "cel(") {
		t.Fatalf("cel: expected one manual finding, got %+v", r.Findings)
	}
}

func TestKnownUndeclared(t *testing.T) {
	l := setup(t, map[string]string{
		"product": pkg("product", "stable-1.0", "1.0.0", ""),
		"parts":   pkg("parts", "stable-1.0", "1.0.0", ""),
	})
	known := []KnownDep{{Package: "product", Requires: []KnownRequired{{Package: "parts", Channel: "same", Source: "test"}}}}
	r, err := Check(context.Background(), l, cfg("product"), Options{Known: known})
	if err != nil {
		t.Fatal(err)
	}
	if got := addedPackages(r); !slices.Equal(got, []string{"parts"}) {
		t.Fatalf("added %v", got)
	}
	if r.Findings[0].Source != "test" {
		t.Fatalf("source not carried: %+v", r.Findings[0])
	}
}

func TestPlatformGVK(t *testing.T) {
	l := setup(t, map[string]string{
		"app": pkg("app", "stable", "1.0.0", `{"type":"olm.gvk.required","value":{"group":"config.openshift.io","version":"v1","kind":"ClusterVersion"}}`),
	})
	r, err := Check(context.Background(), l, cfg("app"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings(r, StatusPlatform)) != 1 || r.HasProblems() {
		t.Fatalf("expected one informational platform finding, got %+v", r.Findings)
	}
}

func TestBuiltinKnownDepsParse(t *testing.T) {
	k, err := DefaultKnownDeps()
	if err != nil {
		t.Fatal(err)
	}
	if len(k) == 0 {
		t.Fatal("built-in known deps list is empty")
	}
}

// app 1.0.0 needs a package that no longer exists; app 2.0.0 (head) skips it
// via skipRange and has no dependencies. 1.0.0 -> 1.1.0 -> 2.0.0 by replaces.
const skipCatalog = `{"schema":"olm.package","name":"app","defaultChannel":"stable"}
{"schema":"olm.channel","package":"app","name":"stable","entries":[
 {"name":"app.v1.0.0"},
 {"name":"app.v1.1.0","replaces":"app.v1.0.0"},
 {"name":"app.v2.0.0","replaces":"app.v1.1.0","skipRange":">=0.0.0 <2.0.0"}]}
{"schema":"olm.bundle","package":"app","name":"app.v1.0.0","image":"quay.io/x/app:1.0.0","properties":[{"type":"olm.package","value":{"packageName":"app","version":"1.0.0"}},{"type":"olm.package.required","value":{"packageName":"gone","versionRange":">=1.0.0"}}]}
{"schema":"olm.bundle","package":"app","name":"app.v1.1.0","image":"quay.io/x/app:1.1.0","properties":[{"type":"olm.package","value":{"packageName":"app","version":"1.1.0"}}]}
{"schema":"olm.bundle","package":"app","name":"app.v2.0.0","image":"quay.io/x/app:2.0.0","properties":[{"type":"olm.package","value":{"packageName":"app","version":"2.0.0"}}]}
`

func rangeCfg(minV, maxV string) []*isc.ImageSetConfiguration {
	op := isc.Operator{Catalog: catRef, IncludeConfig: isc.IncludeConfig{Packages: []isc.IncludePackage{{
		Name: "app", Channels: []isc.IncludeChannel{{Name: "stable", IncludeBundle: isc.IncludeBundle{MinVersion: minV, MaxVersion: maxV}}},
	}}}}
	return []*isc.ImageSetConfiguration{{Path: "test.yaml", Mirror: isc.Mirror{Operators: []isc.Operator{op}}}}
}

func TestSkippedBundleIsInformational(t *testing.T) {
	l := setup(t, map[string]string{"app": skipCatalog})
	r, err := Check(context.Background(), l, rangeCfg("1.0.0", ""), Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := findings(r, StatusSkipped)
	if len(s) != 1 || s[0].Bundle != "app.v1.0.0" || !strings.Contains(s[0].Resolution, "app.v2.0.0") {
		t.Fatalf("expected app.v1.0.0 reported as skipped by app.v2.0.0, got %+v", r.Findings)
	}
	if r.HasProblems() || r.Additions != nil {
		t.Fatalf("skipped bundles must not count as problems or produce additions: %+v", r.Findings)
	}
}

func TestOldBundleWithoutSkipperIsStillAProblem(t *testing.T) {
	l := setup(t, map[string]string{"app": skipCatalog})
	// Only 1.0.0 is mirrored, so nothing skips it: it is an install target.
	r, err := Check(context.Background(), l, rangeCfg("1.0.0", "1.0.0"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings(r, StatusUnresolved)) != 1 || !r.HasProblems() {
		t.Fatalf("expected an unresolved finding for app.v1.0.0, got %+v", r.Findings)
	}
}

// multi has two channels: stable (default, head needs "shared") and old (head
// needs "shared" and "legacy").
const multiChannel = `{"schema":"olm.package","name":"multi","defaultChannel":"stable"}
{"schema":"olm.channel","package":"multi","name":"stable","entries":[{"name":"multi.v2.0.0"}]}
{"schema":"olm.channel","package":"multi","name":"old","entries":[{"name":"multi.v1.0.0"}]}
{"schema":"olm.bundle","package":"multi","name":"multi.v1.0.0","image":"quay.io/x/multi:1.0.0","properties":[{"type":"olm.package","value":{"packageName":"multi","version":"1.0.0"}},{"type":"olm.package.required","value":{"packageName":"shared","versionRange":">=1.0.0"}},{"type":"olm.package.required","value":{"packageName":"legacy","versionRange":">=1.0.0"}}]}
{"schema":"olm.bundle","package":"multi","name":"multi.v2.0.0","image":"quay.io/x/multi:2.0.0","properties":[{"type":"olm.package","value":{"packageName":"multi","version":"2.0.0"}},{"type":"olm.package.required","value":{"packageName":"shared","versionRange":">=1.0.0"}}]}
`

func hintSetup(t *testing.T) *catalog.Loader {
	return setup(t, map[string]string{
		"multi":  multiChannel,
		"shared": pkg("shared", "stable", "1.0.0", ""),
		"legacy": pkg("legacy", "stable", "1.0.0", ""),
	})
}

func findingFor(r *Result, requirementPrefix string) *Finding {
	for i := range r.Findings {
		if strings.HasPrefix(r.Findings[i].Requirement, requirementPrefix) {
			return &r.Findings[i]
		}
	}
	return nil
}

func TestHintForPackageListedWithoutChannels(t *testing.T) {
	r, err := Check(context.Background(), hintSetup(t), cfg("multi"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	legacy := findingFor(r, "package legacy")
	if legacy == nil || legacy.Bundle != "multi.v1.0.0" || !strings.Contains(legacy.Hint, "listed without channels") || !strings.Contains(legacy.Hint, "only in old") {
		t.Fatalf("expected a hint on the legacy requirement from the old-channel head, got %+v", legacy)
	}
	// "shared" is also needed by the default-channel head, so it must be
	// reported against that bundle without a hint.
	shared := findingFor(r, "package shared")
	if shared == nil || shared.Bundle != "multi.v2.0.0" || shared.Hint != "" {
		t.Fatalf("expected the shared requirement reported on multi.v2.0.0 with no hint, got %+v", shared)
	}
}

func TestNoHintWhenChannelListed(t *testing.T) {
	op := isc.Operator{Catalog: catRef, IncludeConfig: isc.IncludeConfig{Packages: []isc.IncludePackage{{
		Name: "multi", Channels: []isc.IncludeChannel{{Name: "stable"}, {Name: "old"}},
	}}}}
	r, err := Check(context.Background(), hintSetup(t), []*isc.ImageSetConfiguration{{Path: "t.yaml", Mirror: isc.Mirror{Operators: []isc.Operator{op}}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		if f.Hint != "" {
			t.Fatalf("no hint expected when the channel is listed explicitly, got %+v", f)
		}
	}
	if findingFor(r, "package legacy") == nil {
		t.Fatal("legacy requirement should still be reported")
	}
}
