package replay

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	mmsemver "github.com/Masterminds/semver/v3"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/isc"
)

// ambiguousExactVersions finds minVersion == maxVersion selections that match
// more than one bundle. catalog-filter v0.0.5 (specificBundle, OCPBUGS-138615) resolves these
// by iterating a Go map over every bundle in the package, comparing with
// semver equality (which ignores build metadata), so oc-mirror picks one of
// the candidates at random on every run. Replay cannot know which one was
// actually mirrored; only a registry scan can.
func ambiguousExactVersions(dc *declcfg.DeclarativeConfig, op isc.Operator) []string {
	var exact []struct{ pkg, where, version string }
	for _, p := range op.Packages {
		if p.MinVersion != "" && p.MinVersion == p.MaxVersion {
			exact = append(exact, struct{ pkg, where, version string }{p.Name, "package " + p.Name, p.MinVersion})
		}
		for _, ch := range p.Channels {
			if ch.MinVersion != "" && ch.MinVersion == ch.MaxVersion {
				exact = append(exact, struct{ pkg, where, version string }{p.Name, p.Name + "/" + ch.Name, ch.MinVersion})
			}
		}
	}
	if len(exact) == 0 {
		return nil
	}
	var out []string
	for _, e := range exact {
		want, err := mmsemver.NewVersion(e.version)
		if err != nil {
			continue
		}
		var matches []string
		for _, b := range dc.Bundles {
			if b.Package != e.pkg {
				continue
			}
			v, ok := packageVersion(b)
			if !ok {
				continue
			}
			if got, err := mmsemver.StrictNewVersion(v); err == nil && got.Equal(want) {
				matches = append(matches, b.Name)
			}
		}
		if len(matches) > 1 {
			sort.Strings(matches)
			out = append(out, fmt.Sprintf(
				"%s pins version %s, which matches %d bundles (%s); oc-mirror v2 picks one of them at random, so the bundle actually mirrored is unknown — confirm with a registry scan",
				e.where, e.version, len(matches), strings.Join(matches, ", ")))
		}
	}
	return out
}

func packageVersion(b declcfg.Bundle) (string, bool) {
	for _, p := range b.Properties {
		if p.Type != property.TypePackage {
			continue
		}
		var pkg property.Package
		if err := json.Unmarshal(p.Value, &pkg); err != nil {
			return "", false
		}
		return pkg.Version, true
	}
	return "", false
}
