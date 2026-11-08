# Filed as OCPBUGS-138615 (2026-11-08)

| Field | Value |
|---|---|
| Project | OCPBUGS |
| Issue type | Bug |
| Component | oc-mirror |
| Affects Version/s | 4.21.0, 4.22, 4.20.z (builds containing the OCPBUGS-61497 backport, openshift/oc-mirror PR #1306) |
| Severity | Moderate |
| Labels | v2 |
| Links | is caused by OCPBUGS-61497 (fix bumped github.com/sherine-k/catalog-filter v0.0.4 → v0.0.5) |

## Summary

[v2] minVersion == maxVersion mirrors a random bundle when several bundles share that version (catalog-filter specificBundle iterates a Go map)

## Description of problem

When an ImageSetConfiguration pins an operator to a single version (`minVersion` and
`maxVersion` set to the same value), oc-mirror v2 picks **one bundle at random** whenever
more than one bundle in the package has that semantic version. Repeated runs with the same
ImageSetConfiguration and the same catalog digest mirror different bundles.

This happens routinely with Red Hat operators that publish rebuilds of a release as
separate bundles whose versions differ only in semver build metadata. For example, in
redhat-operator-index v4.20, web-terminal has four bundles that are all "1.12.1" under
semver equality:

| Bundle | olm.package version | Bundle image |
|---|---|---|
| web-terminal.v1.12.1 | 1.12.1 | ...web-terminal-operator-bundle@sha256:ad3b9e716d3b... |
| web-terminal.v1.12.1-0.1742484227.p | 1.12.1+0.1742484227.p | ...@sha256:f80f5dc17ee6... |
| web-terminal.v1.12.1-0.1743657317.p | 1.12.1+0.1743657317.p | ...@sha256:a2ff58b97b2a... |
| web-terminal.v1.12.1-0.1745393748.p | 1.12.1+0.1745393748.p | ...@sha256:679eb2b1b47d... |

### Root cause

The OCPBUGS-61497 fix (catalog-filter v0.0.5) added a special case for
`minVersion == maxVersion`: `isSpecificVersion()` detects it and `specificBundle()`
returns a single bundle:

```go
// github.com/sherine-k/catalog-filter@v0.0.5/pkg/filter/mirror-config/v1alpha1/mirror-filter.go:423
func specificBundle(specificVersion string, bundles map[string]*mmsemver.Version) (string, error) {
	specificSemVer, err := mmsemver.NewVersion(specificVersion)
	...
	for bundle, bundleVersion := range bundles {      // Go map: iteration order is randomized
		if bundleVersion.Equal(specificSemVer) {      // semver equality ignores build metadata
			return bundle, nil                          // first match wins
		}
	}
```

`bundles` is `catalogIndex.BundleVersionsByPkgAndName[ch.Package]`, a map. Go deliberately
randomizes map iteration order, and `Equal` ignores build metadata (as the semver spec
requires), so every bundle above matches and whichever comes first in that run's
iteration order is returned.

openshift/oc-mirror main (bb98f7ce, 2026-11-04) still pins catalog-filter v0.0.5, and
v0.0.5 is the latest catalog-filter release.

## Version-Release number of selected component

oc-mirror 4.22.0-202610061703.p2.ged9ff0d.assembly.stream.el9 (`--v2`). Expected to affect
every build with catalog-filter v0.0.5: 4.21.0+ and 4.20.z with the OCPBUGS-61497 backport.

## How reproducible

Always nondeterministic. 3 distinct bundles across 6 identical runs (below).

## Steps to Reproduce

1. Create `isc.yaml` (catalog pinned by digest so the catalog content cannot change between runs):

```yaml
kind: ImageSetConfiguration
apiVersion: mirror.openshift.io/v2alpha1
mirror:
  operators:
  - catalog: registry.redhat.io/redhat/redhat-operator-index@sha256:437b0fee59148a1f7930f1e75f2f666dc23e3954aec5dd323840c3188ee87f3a
    packages:
    - name: web-terminal
      channels:
      - name: fast
        minVersion: 1.12.1
        maxVersion: 1.12.1
```

2. Run it several times, starting clean each time:

```bash
for i in 1 2 3 4 5 6; do
  rm -rf ./run
  oc-mirror -c isc.yaml file://./run --v2 --dry-run >/dev/null 2>&1
  echo "run $i: $(grep -o 'web-terminal-operator-bundle@sha256:[0-9a-f]\{12\}' run/working-dir/dry-run/mapping.txt)"
done
```

## Actual results

```
run 1: web-terminal-operator-bundle@sha256:679eb2b1b47d
run 2: web-terminal-operator-bundle@sha256:f80f5dc17ee6
run 3: web-terminal-operator-bundle@sha256:679eb2b1b47d
run 4: web-terminal-operator-bundle@sha256:a2ff58b97b2a
run 5: web-terminal-operator-bundle@sha256:679eb2b1b47d
run 6: web-terminal-operator-bundle@sha256:a2ff58b97b2a
```

The bundle image, its related images, and the rebuilt catalog's channel content differ
from run to run.

## Expected results

The same ImageSetConfiguration and catalog digest always produce the same mirror content.
Which bundle(s) a version pin selects should be defined and documented. Options:

- Prefer the bundle whose `olm.package` version string exactly equals the requested string
  (here `web-terminal.v1.12.1`), or
- Select every bundle in the requested channel that equals the requested version (here all
  four rebuilds, with the newest as the channel head), or
- At minimum, choose deterministically, e.g. sort candidates and limit them to entries of
  the requested channel.

## Additional info

**Impact**

- Mirrors are not reproducible: a mirror-to-disk run, a later re-run, and a
  mirror-to-mirror run with the same configuration can each carry a different bundle and a
  different set of related images.
- Version pinning is how customers meet change-control requirements (OCPBUGS-61497 came
  from exactly that use case). They get a bundle they did not choose, and it can change on
  the next run.
- The selected bundle may not be the latest rebuild of that version, so a run can silently
  mirror an older rebuild that is missing later fixes.

**Workaround (verified with the same oc-mirror build, deterministic over 2 runs)**

Use a narrow range instead of an exact pin, so the `isSpecificVersion` path is not taken:

```yaml
      - name: fast
        minVersion: 1.12.1
        maxVersion: 1.12.2-0
```

This always mirrors all four 1.12.1 bundles, with `web-terminal.v1.12.1-0.1745393748.p`
as the channel head.

**Suggested fix**

Fix `specificBundle` in catalog-filter (deterministic selection limited to the requested
channel, preferring an exact version-string match), release catalog-filter v0.0.6, bump it
in oc-mirror, and add a unit test with several bundles that differ only in build metadata.
