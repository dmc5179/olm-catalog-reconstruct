# olm-reconstruct

Go tool for working out what oc-mirror v2 (4.18+) mirrored for operator catalogs, and
whether the operators in an ImageSetConfiguration have their dependencies mirrored too.

It uses the same filtering library as oc-mirror v2
(`github.com/sherine-k/catalog-filter`) and the same ImageSetConfiguration → filter
conversion, so for the same catalog digest it selects the same bundles oc-mirror does.
oc-mirror v1 and oc-mirror 4.17 and older are out of scope.

## Build

```bash
go build -o bin/olm-reconstruct ./cmd/olm-reconstruct
go test ./...
```

## Catalog sources

Catalog references in the ImageSetConfiguration are pulled (linux/amd64 by default) and
their `/configs` directory is cached under `~/.cache/olm-reconstruct/catalogs/<digest>`.
Credentials come from `--authfile`, `$REGISTRY_AUTH_FILE`, or the standard docker/podman
locations. To use an already extracted catalog instead:

```bash
--catalog-dir registry.redhat.io/redhat/redhat-operator-index:v4.20=./configs
```

Pin catalogs by digest (`...@sha256:...`) when you need results to match a specific
oc-mirror run: tags like `:v4.20` move as Red Hat publishes updates.

## replay: rebuild the catalog for one or more ImageSetConfigurations

```bash
bin/olm-reconstruct replay isc-2025.yaml isc-2026-q1.yaml -o replay-out
```

For each target catalog it writes `replay-out/<target>/configs/` (a file-based catalog
that passes `opm validate`), `replay-out/<target>/images.txt`, and `replay-out/report.json`.

When several configs select non-contiguous parts of the same channel, merging leaves a
gap in the upgrade graph. By default the gap is filled from the source catalog and
reported as `FILLED`; `--no-fill-gaps` makes it an error instead.

Warnings to know about:

- **Exact versions are ambiguous.** `minVersion == maxVersion` makes catalog-filter
  v0.0.5 pick one bundle at random among all bundles in the package with that semver
  (build metadata ignored). Replay flags this; only a registry scan can tell which one
  was mirrored.
- **Index drift.** Channels and bundles are removed from Red Hat indexes over time, so an
  old config replayed against today's index can fail or select different heads.

## deps: check operator dependencies

```bash
bin/olm-reconstruct deps imageset-config.yaml -o additions.yaml
bin/olm-reconstruct deps imageset-config.yaml additions.yaml   # confirm: should be clean
```

Checks every bundle the config selects for `olm.package.required`,
`olm.gvk.required`, `olm.constraint` (all/any/not; CEL flagged for manual review), and
undeclared runtime dependencies from `internal/deps/known.yaml` (extend with
`--known-deps file.yaml`). Missing dependencies are resolved from the loaded catalogs,
recursively, and written as a supplementary ImageSetConfiguration. `--strict` exits 3
when anything is missing, unresolved or conflicting; `--json` writes the full result.

oc-mirror v2 never adds dependencies itself (by design), so anything reported as missing
would be absent from the mirror.

Things that commonly surprise people:

- A package listed with no channels mirrors the head of **every** channel, including old
  ones, and their dependencies (for example old mta-operator heads need rhsso-operator).
- A range can select old bundles whose dependencies no longer exist in the index (for
  example mtc-operator 1.8.0–1.8.4 need OADP < 1.5, but the v4.20 index only has OADP
  1.5.x). When a newer mirrored bundle in the same channel skips them (`skips` or
  `skipRange`), they are upgrade sources rather than install targets: they are listed under
  SKIPPED BUNDLES for information, add nothing to the suggested additions, and do not fail
  `--strict`. If nothing mirrored skips them, they are reported as unresolved.
- Exact pins (`minVersion == maxVersion`) can mirror a random bundle (OCPBUGS-138615); use
  a narrow range such as `minVersion: 1.12.1` / `maxVersion: 1.12.2-0` instead.

## Test fixtures

`testdata/` holds ImageSetConfigurations pinned to
`redhat-operator-index@sha256:437b0fee…` that were compared against `oc-mirror --v2
--dry-run` output.
