# OLM Operator Dependency Checker

`check-operator-deps.sh` validates that an `imageset-config.yaml` for `oc-mirror` includes all of the operator dependencies required by the operators it lists. It catches missing dependencies **before** you mirror, so you don't end up with a disconnected cluster that can't install an operator because a required dependency was left out of the image set.

## Problem

When using `oc-mirror` to mirror a subset of operators from the `redhat-operator-index` into a disconnected environment, you must explicitly list every operator package you want. OLM operators can declare dependencies on other operators, but `oc-mirror` does not resolve or warn about those dependencies for you. If you mirror `web-terminal` without also mirroring `devworkspace-operator`, the install will fail at runtime.

## How it works

1. **Parses the imageset-config.yaml** to extract each catalog image reference and the list of operator packages requested under it.
2. **Extracts the file-based catalog (FBC)** from the operator-index container image by running `podman create` + `podman cp /configs`. Alternatively, you can point it at an already-extracted configs directory with `--local-configs`.
3. **Scans every requested operator's catalog entries** for three types of dependency declarations:
   - **`olm.package.required`** — a direct dependency on another operator package by name and version range.
   - **`olm.gvk.required`** — a dependency on a Kubernetes API (group/version/kind) that must be provided by some other operator.
   - **`olm.constraint`** — a general OLM constraint that can contain arbitrarily nested `package` references (e.g. `all.constraints[].package.packageName`).
4. **Builds a GVK provider index** across the entire catalog so it can resolve `olm.gvk.required` dependencies to specific operator package names.
5. **Reports the results**: either all dependencies are satisfied ("ALL OK", exit code 0) or it prints a detailed report of what is missing along with copy-paste suggestions (exit code 1).

The script handles both FBC directory layouts found in the Red Hat operator index:
- **Single-file**: `<operator>/catalog.json` (concatenated JSON objects, one per line-delimited blob)
- **Split**: `<operator>/bundles/*.json` + `channels/` + `package.json` (one JSON file per bundle version)

## Prerequisites

| Tool | Required when | Install |
|------|--------------|---------|
| `jq` | Always | `dnf install jq` or `brew install jq` |
| `yq` | Always | `pip install yq` (python-yq) or `brew install yq` (mikefarah/yq) |
| `podman` | Default mode (extracting from index image) | `dnf install podman` |

You also need pull access to the operator-index image (e.g. `registry.redhat.io/redhat/redhat-operator-index:v4.21`). Run `podman login registry.redhat.io` first if you haven't already.

## Usage

```bash
# Basic — pull the index image and check dependencies
./check-operator-deps.sh imageset-config.yaml

# Keep the extracted configs directory for re-use
./check-operator-deps.sh imageset-config.yaml --keep-configs

# Use a previously extracted configs directory (no podman needed)
./check-operator-deps.sh imageset-config.yaml --local-configs /path/to/configs

# Show help
./check-operator-deps.sh --help
```

### Options

| Flag | Description |
|------|-------------|
| `--keep-configs` | Do not delete the extracted configs directory on exit. The path is printed at the start of the run. Useful when iterating on the same index version. |
| `--local-configs <path>` | Skip the podman extraction entirely and use an existing configs directory. `<path>` should be the directory containing operator subdirectories (e.g. `web-terminal/`, `mtc-operator/`). |
| `-h`, `--help` | Print usage information and exit. |

### Extracting configs manually

If you prefer to extract the configs directory yourself (or already have one from a prior run), you can do so with:

```bash
podman create --name index registry.redhat.io/redhat/redhat-operator-index:v4.21
podman cp index:/configs ./configs
podman rm index
```

Then pass it to the script:

```bash
./check-operator-deps.sh imageset-config.yaml --local-configs ./configs
```

## Input format

The script expects a standard `oc-mirror` `ImageSetConfiguration`:

```yaml
kind: ImageSetConfiguration
apiVersion: mirror.openshift.io/v1alpha2
mirror:
  operators:
  - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.21
    packages:
    - name: web-terminal
    - name: odf-operator
    - name: mtc-operator
    - name: service-telemetry-operator
```

Multiple catalog entries under `.mirror.operators[]` are supported — each is processed independently.

## Output

### When dependencies are missing (exit code 1)

```
==========================================
 MISSING OPERATOR PACKAGE DEPENDENCIES
==========================================
  - amq7-interconnect-operator
    needed by: service-telemetry-operator
  - devworkspace-operator
    needed by: web-terminal
  - redhat-oadp-operator
    needed by: mtc-operator
  - smart-gateway-operator
    needed by: service-telemetry-operator

==========================================
 MISSING GVK (API) DEPENDENCIES
==========================================
 These GVKs are required but the providing
 operator is not in your imageset-config.
==========================================
  - controller.devfile.io/v1alpha1/DevWorkspaceRouting
    web-terminal (provided by: devworkspace-operator)
  - workspace.devfile.io/v1alpha1/DevWorkspace
    web-terminal (provided by: devworkspace-operator)

==========================================
 SUGGESTED ADDITIONS to imageset-config
==========================================
 Add these packages under .mirror.operators[].packages:

    - name: amq7-interconnect-operator
    - name: devworkspace-operator
    - name: redhat-oadp-operator
    - name: smart-gateway-operator
```

The output has up to four sections:

| Section | Meaning |
|---------|---------|
| **MISSING OPERATOR PACKAGE DEPENDENCIES** | An operator in your config declares `olm.package.required` or `olm.constraint` on another operator package that is not in your config. |
| **MISSING GVK (API) DEPENDENCIES** | An operator requires a Kubernetes API (group/version/kind) via `olm.gvk.required`, and the script found which operator in the index provides it — but that operator is not in your config. |
| **UNRESOLVABLE GVK DEPENDENCIES** | An operator requires a GVK that no operator in the entire index provides. This usually means the CRD is installed outside of OLM (e.g. by the platform itself or a Helm chart). These are informational and may not require action. |
| **SUGGESTED ADDITIONS** | A consolidated list of `- name:` entries you can copy directly into your imageset-config to resolve the missing dependencies. |

### When all dependencies are satisfied (exit code 0)

```
ALL OK — all operator dependencies are satisfied.
```

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | All operator dependencies are satisfied by other operators in the config. |
| `1` | One or more dependencies are missing. See the output report for details. |

## Dependency types explained

### olm.package.required

The most common dependency type. The operator explicitly declares that it needs another operator package to be installed. Found in the bundle's `properties` array:

```json
{
  "type": "olm.package.required",
  "value": {
    "packageName": "devworkspace-operator",
    "versionRange": ">=0.6.0"
  }
}
```

### olm.gvk.required

The operator requires a specific Kubernetes API (Custom Resource) to exist on the cluster. The script builds an index of which operators provide which GVKs (via `olm.gvk` properties) across the entire catalog and uses that to resolve GVK requirements back to operator package names.

```json
{
  "type": "olm.gvk.required",
  "value": {
    "group": "workspace.devfile.io",
    "version": "v1alpha1",
    "kind": "DevWorkspace"
  }
}
```

### olm.constraint

A general-purpose constraint mechanism that can contain arbitrarily nested structures. The script recursively walks the constraint tree to extract any `packageName` references. These often include human-readable `failureMessage` fields:

```json
{
  "type": "olm.constraint",
  "value": {
    "all": {
      "constraints": [
        {
          "failureMessage": "Package amq7-interconnect-operator is needed for data transport with STF",
          "package": {
            "packageName": "amq7-interconnect-operator",
            "versionRange": ">=1.10.0"
          }
        }
      ]
    },
    "failureMessage": "Require data transport for Service Telemetry Framework"
  }
}
```

## Tips

- **Run early**: check your imageset-config before starting the mirror. A missing dependency discovered after hours of mirroring is costly.
- **Iterate with `--local-configs`**: extract the configs once with `--keep-configs`, then re-run with `--local-configs` as you add operators to your imageset-config. This avoids re-pulling the index image each time.
- **Transitive dependencies**: the script checks the operators you listed, not their dependencies' dependencies. If the script suggests adding `devworkspace-operator` and that operator also has dependencies, run the script again after adding it to catch the next level.
- **Informational GVK warnings**: some `olm.gvk.required` entries refer to APIs provided by the OpenShift platform itself (not by an operator). These appear in the "UNRESOLVABLE GVK DEPENDENCIES" section and can usually be ignored.
