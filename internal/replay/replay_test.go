package replay

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/catalog"
	"github.com/dmc5179/olm-catalog-reconstruct/internal/isc"
)

// testCatalog is a tiny FBC: package foo, channel stable, 1.0.0 -> 1.0.1 -> ... -> 1.0.5.
const testCatalog = `{"schema":"olm.package","name":"foo","defaultChannel":"stable"}
{"schema":"olm.channel","package":"foo","name":"stable","entries":[
 {"name":"foo.v1.0.0"},
 {"name":"foo.v1.0.1","replaces":"foo.v1.0.0"},
 {"name":"foo.v1.0.2","replaces":"foo.v1.0.1"},
 {"name":"foo.v1.0.3","replaces":"foo.v1.0.2"},
 {"name":"foo.v1.0.4","replaces":"foo.v1.0.3"},
 {"name":"foo.v1.0.5","replaces":"foo.v1.0.4"}]}
{"schema":"olm.bundle","package":"foo","name":"foo.v1.0.0","image":"quay.io/x/foo-bundle:1.0.0","properties":[{"type":"olm.package","value":{"packageName":"foo","version":"1.0.0"}}]}
{"schema":"olm.bundle","package":"foo","name":"foo.v1.0.1","image":"quay.io/x/foo-bundle:1.0.1","properties":[{"type":"olm.package","value":{"packageName":"foo","version":"1.0.1"}}]}
{"schema":"olm.bundle","package":"foo","name":"foo.v1.0.2","image":"quay.io/x/foo-bundle:1.0.2","properties":[{"type":"olm.package","value":{"packageName":"foo","version":"1.0.2"}}]}
{"schema":"olm.bundle","package":"foo","name":"foo.v1.0.3","image":"quay.io/x/foo-bundle:1.0.3","properties":[{"type":"olm.package","value":{"packageName":"foo","version":"1.0.3"}}]}
{"schema":"olm.bundle","package":"foo","name":"foo.v1.0.4","image":"quay.io/x/foo-bundle:1.0.4","properties":[{"type":"olm.package","value":{"packageName":"foo","version":"1.0.4"}}]}
{"schema":"olm.bundle","package":"foo","name":"foo.v1.0.5","image":"quay.io/x/foo-bundle:1.0.5","properties":[{"type":"olm.package","value":{"packageName":"foo","version":"1.0.5"}}]}
`

const catRef = "example.com/test/index:v1"

func setup(t *testing.T) *catalog.Loader {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "foo", "catalog.json"), []byte(testCatalog), 0o644); err != nil {
		t.Fatal(err)
	}
	l := catalog.NewLoader()
	l.Overrides[catRef] = dir
	return l
}

func cfg(path string, pkgs ...isc.IncludePackage) *isc.ImageSetConfiguration {
	return &isc.ImageSetConfiguration{Path: path, Mirror: isc.Mirror{Operators: []isc.Operator{
		{Catalog: catRef, IncludeConfig: isc.IncludeConfig{Packages: pkgs}},
	}}}
}

func bundleNames(t *Target) []string {
	var n []string
	for _, b := range t.DC.Bundles {
		n = append(n, b.Name)
	}
	slices.Sort(n)
	return n
}

func TestHeadOnly(t *testing.T) {
	targets, err := Run(context.Background(), setup(t), []*isc.ImageSetConfiguration{cfg("a", isc.IncludePackage{Name: "foo"})}, Options{FillGaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := bundleNames(targets[0]); !slices.Equal(got, []string{"foo.v1.0.5"}) {
		t.Fatalf("got %v", got)
	}
}

func TestMergeWithGap(t *testing.T) {
	a := cfg("a", isc.IncludePackage{Name: "foo", Channels: []isc.IncludeChannel{{Name: "stable", IncludeBundle: isc.IncludeBundle{MinVersion: "1.0.0", MaxVersion: "1.0.1"}}}})
	b := cfg("b", isc.IncludePackage{Name: "foo", Channels: []isc.IncludeChannel{{Name: "stable", IncludeBundle: isc.IncludeBundle{MinVersion: "1.0.4"}}}})

	if _, err := Run(context.Background(), setup(t), []*isc.ImageSetConfiguration{a, b}, Options{FillGaps: false}); err == nil {
		t.Fatal("expected a gap error with FillGaps=false")
	}

	targets, err := Run(context.Background(), setup(t), []*isc.ImageSetConfiguration{a, b}, Options{FillGaps: true})
	if err != nil {
		t.Fatal(err)
	}
	tg := targets[0]
	want := []string{"foo.v1.0.0", "foo.v1.0.1", "foo.v1.0.2", "foo.v1.0.3", "foo.v1.0.4", "foo.v1.0.5"}
	if got := bundleNames(tg); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(tg.Gaps) != 1 || !slices.Equal(tg.Gaps[0].Filled, []string{"foo.v1.0.3", "foo.v1.0.2"}) {
		t.Fatalf("unexpected gaps: %+v", tg.Gaps)
	}
}

func TestFilterDoesNotMutateSource(t *testing.T) {
	l := setup(t)
	a := cfg("a", isc.IncludePackage{Name: "foo"})
	b := cfg("b", isc.IncludePackage{Name: "foo", Channels: []isc.IncludeChannel{{Name: "stable", IncludeBundle: isc.IncludeBundle{MinVersion: "1.0.3"}}}})
	targets, err := Run(context.Background(), l, []*isc.ImageSetConfiguration{a, b}, Options{FillGaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := bundleNames(targets[0]); !slices.Equal(got, []string{"foo.v1.0.3", "foo.v1.0.4", "foo.v1.0.5"}) {
		t.Fatalf("got %v", got)
	}
	src, _ := l.Load(context.Background(), catRef)
	if n := len(src.DC.Channels[0].Entries); n != 6 {
		t.Fatalf("source catalog channel was mutated: %d entries", n)
	}
}
