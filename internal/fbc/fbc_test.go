package fbc

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
)

// chain builds a linear channel foo.v1.0.<n-1> -> ... -> foo.v1.0.0 (head first).
func chain(n int) declcfg.Channel {
	ch := declcfg.Channel{Schema: declcfg.SchemaChannel, Package: "foo", Name: "stable"}
	for i := range n {
		e := declcfg.ChannelEntry{Name: fmt.Sprintf("foo.v1.0.%d", i)}
		if i > 0 {
			e.Replaces = fmt.Sprintf("foo.v1.0.%d", i-1)
		}
		ch.Entries = append(ch.Entries, e)
	}
	return ch
}

func pick(full declcfg.Channel, names ...string) declcfg.Channel {
	out := full
	out.Entries = nil
	for _, e := range full.Entries {
		if slices.Contains(names, e.Name) {
			out.Entries = append(out.Entries, e)
		}
	}
	return out
}

func entryNames(ch declcfg.Channel) []string {
	var n []string
	for _, e := range ch.Entries {
		n = append(n, e.Name)
	}
	slices.Sort(n)
	return n
}

func TestChannelProblem(t *testing.T) {
	full := chain(5)
	if p := ChannelProblem(full); p != "" {
		t.Fatalf("full chain: unexpected problem %q", p)
	}
	// Contiguous tail of the chain is valid (replaces may point outside the channel).
	if p := ChannelProblem(pick(full, "foo.v1.0.2", "foo.v1.0.3", "foo.v1.0.4")); p != "" {
		t.Fatalf("contiguous: unexpected problem %q", p)
	}
	// A hole creates two heads.
	p := ChannelProblem(pick(full, "foo.v1.0.1", "foo.v1.0.3", "foo.v1.0.4"))
	if !strings.HasPrefix(p, "multiple channel heads") {
		t.Fatalf("gap: expected multiple heads, got %q", p)
	}
}

func TestFillChannel(t *testing.T) {
	full := chain(6)
	ch := pick(full, "foo.v1.0.1", "foo.v1.0.4")
	added, err := FillChannel(&ch, full)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"foo.v1.0.3", "foo.v1.0.2"}; !slices.Equal(added, want) {
		t.Fatalf("added %v, want %v", added, want)
	}
	if p := ChannelProblem(ch); p != "" {
		t.Fatalf("still invalid after fill: %s", p)
	}
	// Must not pull in entries above the highest selected or below the lowest.
	if got := entryNames(ch); slices.Contains(got, "foo.v1.0.5") || slices.Contains(got, "foo.v1.0.0") {
		t.Fatalf("fill went outside the selected range: %v", got)
	}
}

func TestFillChannelWithSkips(t *testing.T) {
	// head v3 replaces v2 and skips v2.5 (an off-chain hotfix); v2 replaces v1.
	full := declcfg.Channel{Package: "foo", Name: "stable", Entries: []declcfg.ChannelEntry{
		{Name: "foo.v3", Replaces: "foo.v2", Skips: []string{"foo.v2.5"}},
		{Name: "foo.v2.5"},
		{Name: "foo.v2", Replaces: "foo.v1"},
		{Name: "foo.v1"},
	}}
	if p := ChannelProblem(full); p != "" {
		t.Fatalf("fixture invalid: %s", p)
	}
	// Selecting only the skipped hotfix and v1 must pull in v3 (its only route) and v2.
	ch := pick(full, "foo.v2.5", "foo.v1")
	added, err := FillChannel(&ch, full)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(added)
	if want := []string{"foo.v2", "foo.v3"}; !slices.Equal(added, want) {
		t.Fatalf("added %v, want %v", added, want)
	}
	if p := ChannelProblem(ch); p != "" {
		t.Fatalf("still invalid after fill: %s", p)
	}
}

func bundle(name, version string) declcfg.Bundle {
	v, _ := json.Marshal(property.Package{PackageName: "foo", Version: version})
	return declcfg.Bundle{Schema: declcfg.SchemaBundle, Package: "foo", Name: name, Image: "quay.io/x/foo-bundle:" + version,
		Properties: []property.Property{{Type: property.TypePackage, Value: v}}}
}

func TestMerge(t *testing.T) {
	full := chain(4)
	pkg := declcfg.Package{Schema: declcfg.SchemaPackage, Name: "foo", DefaultChannel: "stable"}
	a := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{pkg},
		Channels: []declcfg.Channel{pick(full, "foo.v1.0.3")},
		Bundles:  []declcfg.Bundle{bundle("foo.v1.0.3", "1.0.3")},
	}
	b := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{pkg},
		Channels: []declcfg.Channel{pick(full, "foo.v1.0.2", "foo.v1.0.3")},
		Bundles:  []declcfg.Bundle{bundle("foo.v1.0.2", "1.0.2"), bundle("foo.v1.0.3", "1.0.3")},
	}
	m, warnings := Merge([]*declcfg.DeclarativeConfig{a, b})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(m.Packages) != 1 || len(m.Channels) != 1 || len(m.Bundles) != 2 {
		t.Fatalf("got %d packages, %d channels, %d bundles", len(m.Packages), len(m.Channels), len(m.Bundles))
	}
	if got := entryNames(m.Channels[0]); !slices.Equal(got, []string{"foo.v1.0.2", "foo.v1.0.3"}) {
		t.Fatalf("entries %v", got)
	}
	if err := Validate(*m); err != nil {
		t.Fatalf("merged catalog invalid: %v", err)
	}
}

func TestCloneIsolatesEntries(t *testing.T) {
	dc := &declcfg.DeclarativeConfig{Channels: []declcfg.Channel{chain(3)}}
	c := Clone(dc)
	c.Channels[0].Entries = slices.DeleteFunc(c.Channels[0].Entries, func(e declcfg.ChannelEntry) bool { return e.Name != "foo.v1.0.2" })
	if len(dc.Channels[0].Entries) != 3 || dc.Channels[0].Entries[0].Name != "foo.v1.0.0" {
		t.Fatalf("original catalog was modified: %+v", dc.Channels[0].Entries)
	}
}
