#!/usr/bin/env bash
#
# check-operator-deps.sh
#
# Given an imageset-config.yaml for oc-mirror, extract the file-based catalog
# from the redhat-operator-index image and check whether every operator listed
# in the config has its dependencies satisfied by other operators in the same
# config.  Reports missing dependencies or prints an all-clear.
#
# Usage:
#   ./check-operator-deps.sh <imageset-config.yaml> [--keep-configs]
#   ./check-operator-deps.sh <imageset-config.yaml> --local-configs /path/to/configs
#
# Requirements: jq, yq (python-yq or mikefarah/yq), podman (unless --local-configs)
#

set -euo pipefail

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
die()  { echo "ERROR: $*" >&2; exit 1; }
info() { echo "INFO: $*"; }
warn() { echo "WARN: $*" >&2; }

usage() {
    cat <<EOF
Usage: $(basename "$0") <imageset-config.yaml> [--keep-configs] [--local-configs <path>]

Options:
  --keep-configs          Do not delete the extracted configs directory on exit.
                          Useful for debugging or re-running against the same index.
  --local-configs <path>  Use an already-extracted configs directory instead of
                          pulling the operator-index image via podman.  <path>
                          should contain operator subdirectories (e.g. web-terminal/).

The imageset-config.yaml should follow the oc-mirror format, e.g.:

  kind: ImageSetConfiguration
  apiVersion: mirror.openshift.io/v1alpha2
  mirror:
    operators:
    - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.21
      packages:
      - name: web-terminal
      - name: odf-operator
EOF
    exit 1
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
KEEP_CONFIGS=false
LOCAL_CONFIGS=""
IMAGESET_CONFIG=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --keep-configs)  KEEP_CONFIGS=true; shift ;;
        --local-configs) LOCAL_CONFIGS="$2"; shift 2 ;;
        -h|--help)       usage ;;
        -*)              die "Unknown option: $1" ;;
        *)
            if [[ -z "$IMAGESET_CONFIG" ]]; then
                IMAGESET_CONFIG="$1"; shift
            else
                die "Unexpected argument: $1"
            fi
            ;;
    esac
done

[[ -n "$IMAGESET_CONFIG" ]] || usage
[[ -f "$IMAGESET_CONFIG" ]] || die "File not found: $IMAGESET_CONFIG"

for cmd in jq yq; do
    command -v "$cmd" >/dev/null 2>&1 || die "'$cmd' is required but not found in PATH."
done
if [[ -z "$LOCAL_CONFIGS" ]]; then
    command -v podman >/dev/null 2>&1 || die "'podman' is required (unless --local-configs is used)."
fi

# ---------------------------------------------------------------------------
# Parse the imageset-config.yaml
#
# Supports multiple operator catalog entries.  For each catalog image we
# collect the list of requested operator package names.
# ---------------------------------------------------------------------------
CATALOG_COUNT=$(yq -r '.mirror.operators | length' "$IMAGESET_CONFIG")
[[ "$CATALOG_COUNT" -gt 0 ]] || die "No operators found in $IMAGESET_CONFIG under .mirror.operators[]"

# ---------------------------------------------------------------------------
# Process each catalog entry
# ---------------------------------------------------------------------------
OVERALL_RC=0

for (( i=0; i<CATALOG_COUNT; i++ )); do
    CATALOG_IMAGE=$(yq -r ".mirror.operators[$i].catalog" "$IMAGESET_CONFIG")
    [[ -n "$CATALOG_IMAGE" && "$CATALOG_IMAGE" != "null" ]] || die "Could not read catalog image for .mirror.operators[$i]"

    # Collect requested package names into a bash array
    mapfile -t REQUESTED_PKGS < <(
        yq -r ".mirror.operators[$i].packages[].name" "$IMAGESET_CONFIG" 2>/dev/null
    )
    [[ ${#REQUESTED_PKGS[@]} -gt 0 ]] || { warn "No packages listed for catalog $CATALOG_IMAGE — skipping."; continue; }

    info "============================================================"
    info "Catalog : $CATALOG_IMAGE"
    info "Packages: ${REQUESTED_PKGS[*]}"
    info "============================================================"

    # ------------------------------------------------------------------
    # Extract /configs from the operator-index image (or use local copy)
    # ------------------------------------------------------------------
    if [[ -n "$LOCAL_CONFIGS" ]]; then
        CONFIGS_ROOT="$LOCAL_CONFIGS"
        [[ -d "$CONFIGS_ROOT" ]] || die "Local configs directory not found: $CONFIGS_ROOT"
        info "Using local configs at $CONFIGS_ROOT"
    else
        CONFIGS_DIR=$(mktemp -d --suffix=.olm-configs)
        if [[ "$KEEP_CONFIGS" == "true" ]]; then
            info "Extracted configs will be kept at: $CONFIGS_DIR"
        else
            trap 'rm -rf "$CONFIGS_DIR"' EXIT
        fi

        info "Pulling and extracting /configs from $CATALOG_IMAGE ..."
        CONTAINER_ID=$(podman create --quiet "$CATALOG_IMAGE" 2>/dev/null) \
            || die "Failed to pull / create container from $CATALOG_IMAGE"

        podman cp "$CONTAINER_ID":/configs "$CONFIGS_DIR/" 2>/dev/null \
            || { podman rm -f "$CONTAINER_ID" >/dev/null 2>&1; die "Failed to copy /configs from $CATALOG_IMAGE"; }

        podman rm -f "$CONTAINER_ID" >/dev/null 2>&1

        CONFIGS_ROOT="$CONFIGS_DIR/configs"
        [[ -d "$CONFIGS_ROOT" ]] || die "Expected directory $CONFIGS_ROOT not found after extraction."

        info "Catalog extracted to $CONFIGS_ROOT"
    fi

    # ------------------------------------------------------------------
    # Build a lookup set of requested package names for fast membership
    # tests.  We use an associative array.
    # ------------------------------------------------------------------
    declare -A REQUESTED_SET=()
    for pkg in "${REQUESTED_PKGS[@]}"; do
        REQUESTED_SET["$pkg"]=1
    done

    # ------------------------------------------------------------------
    # For each requested operator, scan its catalog for dependencies.
    #
    # The FBC layout has two shapes:
    #   1. Single-file:  <operator>/catalog.json  (concatenated JSON objects)
    #   2. Split:        <operator>/bundles/*.json (one JSON object per file)
    #                    + channels/ and package.json
    #
    # Dependencies appear inside olm.bundle objects in the "properties"
    # array as one of:
    #   - olm.package.required  → .value.packageName
    #   - olm.gvk.required      → .value.{group,version,kind}
    #   - olm.constraint         → recursively nested .package.packageName
    # ------------------------------------------------------------------

    # Collect ALL operators that provide a given GVK so we can resolve
    # olm.gvk.required dependencies.  Build: GVK_KEY → space-separated
    # list of operator package names.
    declare -A GVK_PROVIDERS=()
    info "Building GVK provider index (this may take a moment) ..."

    for op_dir in "$CONFIGS_ROOT"/*/; do
        op_name=$(basename "$op_dir")

        # Extract olm.gvk entries (APIs this operator provides)
        gvks=""
        if [[ -f "$op_dir/catalog.json" ]]; then
            gvks=$(jq -r '
                select(.schema == "olm.bundle") |
                .properties[]? |
                select(.type == "olm.gvk") |
                "\(.value.group)/\(.value.version)/\(.value.kind)"
            ' "$op_dir/catalog.json" 2>/dev/null || true)
        elif [[ -d "$op_dir/bundles" ]]; then
            gvks=$(find "$op_dir/bundles" -name '*.json' -exec \
                jq -r '
                    .properties[]? |
                    select(.type == "olm.gvk") |
                    "\(.value.group)/\(.value.version)/\(.value.kind)"
                ' {} + 2>/dev/null || true)
        fi

        while IFS= read -r gvk_key; do
            [[ -n "$gvk_key" ]] || continue
            existing="${GVK_PROVIDERS["$gvk_key"]:-}"
            # Avoid duplicate provider entries
            if [[ " $existing " != *" $op_name "* ]]; then
                GVK_PROVIDERS["$gvk_key"]="$existing $op_name"
            fi
        done <<< "$gvks"
    done

    # ------------------------------------------------------------------
    # Scan each requested operator for dependencies
    # ------------------------------------------------------------------
    declare -A ALL_MISSING_PKGS=()       # packageName → list of "needed by" operators
    declare -A ALL_MISSING_GVKS=()       # GVK key     → list of "needed by" operators
    declare -A ALL_RESOLVABLE_GVKS=()    # GVK key     → provider(s) not in requested set

    for pkg in "${REQUESTED_PKGS[@]}"; do
        OP_DIR="$CONFIGS_ROOT/$pkg"
        if [[ ! -d "$OP_DIR" ]]; then
            warn "Operator directory not found in index: $pkg — skipping."
            continue
        fi

        # Gather all dependency properties from every bundle version
        DEP_PROPS=""
        if [[ -f "$OP_DIR/catalog.json" ]]; then
            DEP_PROPS=$(jq -c '
                select(.schema == "olm.bundle") |
                .properties[]? |
                select(.type == "olm.package.required"
                    or .type == "olm.gvk.required"
                    or .type == "olm.constraint")
            ' "$OP_DIR/catalog.json" 2>/dev/null || true)
        elif [[ -d "$OP_DIR/bundles" ]]; then
            DEP_PROPS=$(find "$OP_DIR/bundles" -name '*.json' -exec \
                jq -c '
                    .properties[]? |
                    select(.type == "olm.package.required"
                        or .type == "olm.gvk.required"
                        or .type == "olm.constraint")
                ' {} + 2>/dev/null || true)
        fi

        [[ -n "$DEP_PROPS" ]] || continue

        # --- olm.package.required -------------------------------------------
        pkg_deps=$(echo "$DEP_PROPS" | jq -r '
            select(.type == "olm.package.required") | .value.packageName
        ' 2>/dev/null | sort -u)

        while IFS= read -r dep; do
            [[ -n "$dep" ]] || continue
            if [[ -z "${REQUESTED_SET[$dep]:-}" ]]; then
                ALL_MISSING_PKGS["$dep"]="${ALL_MISSING_PKGS["$dep"]:-}${ALL_MISSING_PKGS["$dep"]:+, }$pkg"
            fi
        done <<< "$pkg_deps"

        # --- olm.constraint (nested packageName) ----------------------------
        constraint_deps=$(echo "$DEP_PROPS" | jq -r '
            select(.type == "olm.constraint") |
            .value | .. | .packageName? // empty
        ' 2>/dev/null | sort -u)

        while IFS= read -r dep; do
            [[ -n "$dep" ]] || continue
            if [[ -z "${REQUESTED_SET[$dep]:-}" ]]; then
                ALL_MISSING_PKGS["$dep"]="${ALL_MISSING_PKGS["$dep"]:-}${ALL_MISSING_PKGS["$dep"]:+, }$pkg"
            fi
        done <<< "$constraint_deps"

        # --- olm.gvk.required -----------------------------------------------
        gvk_deps=$(echo "$DEP_PROPS" | jq -r '
            select(.type == "olm.gvk.required") |
            "\(.value.group)/\(.value.version)/\(.value.kind)"
        ' 2>/dev/null | sort -u)

        while IFS= read -r gvk_key; do
            [[ -n "$gvk_key" ]] || continue
            providers="${GVK_PROVIDERS["$gvk_key"]:-}"

            if [[ -z "$providers" ]]; then
                ALL_MISSING_GVKS["$gvk_key"]="${ALL_MISSING_GVKS["$gvk_key"]:-}${ALL_MISSING_GVKS["$gvk_key"]:+, }$pkg"
                continue
            fi

            # Check if ANY provider is in the requested set
            resolved=false
            for prov in $providers; do
                if [[ -n "${REQUESTED_SET[$prov]:-}" ]]; then
                    resolved=true
                    break
                fi
            done

            if [[ "$resolved" == "false" ]]; then
                trimmed=$(echo "$providers" | xargs)
                ALL_RESOLVABLE_GVKS["$gvk_key"]="${ALL_RESOLVABLE_GVKS["$gvk_key"]:-}${ALL_RESOLVABLE_GVKS["$gvk_key"]:+, }$pkg (provided by: $trimmed)"
            fi
        done <<< "$gvk_deps"
    done

    # ------------------------------------------------------------------
    # Report results
    # ------------------------------------------------------------------
    echo ""
    HAS_ISSUES=false

    if [[ ${#ALL_MISSING_PKGS[@]} -gt 0 ]]; then
        HAS_ISSUES=true
        echo "=========================================="
        echo " MISSING OPERATOR PACKAGE DEPENDENCIES"
        echo "=========================================="
        for dep in $(echo "${!ALL_MISSING_PKGS[@]}" | tr ' ' '\n' | sort); do
            needed_by="${ALL_MISSING_PKGS[$dep]}"
            echo "  - $dep"
            echo "    needed by: $needed_by"
        done
        echo ""
    fi

    if [[ ${#ALL_RESOLVABLE_GVKS[@]} -gt 0 ]]; then
        HAS_ISSUES=true
        echo "=========================================="
        echo " MISSING GVK (API) DEPENDENCIES"
        echo "=========================================="
        echo " These GVKs are required but the providing"
        echo " operator is not in your imageset-config."
        echo "=========================================="
        for gvk in $(echo "${!ALL_RESOLVABLE_GVKS[@]}" | tr ' ' '\n' | sort); do
            info_str="${ALL_RESOLVABLE_GVKS[$gvk]}"
            echo "  - $gvk"
            echo "    $info_str"
        done
        echo ""
    fi

    if [[ ${#ALL_MISSING_GVKS[@]} -gt 0 ]]; then
        HAS_ISSUES=true
        echo "=========================================="
        echo " UNRESOLVABLE GVK DEPENDENCIES"
        echo "=========================================="
        echo " These GVKs are required but no operator in"
        echo " the index provides them. They may be provided"
        echo " by CRDs installed outside OLM."
        echo "=========================================="
        for gvk in $(echo "${!ALL_MISSING_GVKS[@]}" | tr ' ' '\n' | sort); do
            needed_by="${ALL_MISSING_GVKS[$gvk]}"
            echo "  - $gvk"
            echo "    needed by: $needed_by"
        done
        echo ""
    fi

    if [[ "$HAS_ISSUES" == "true" ]]; then
        echo "=========================================="
        echo " SUGGESTED ADDITIONS to imageset-config"
        echo "=========================================="
        echo " Add these packages under .mirror.operators[].packages:"
        echo ""
        for dep in $(echo "${!ALL_MISSING_PKGS[@]}" | tr ' ' '\n' | sort); do
            echo "    - name: $dep"
        done

        # Suggest GVK providers too
        declare -A SUGGESTED_GVK_PROVIDERS=()
        for gvk in "${!ALL_RESOLVABLE_GVKS[@]}"; do
            info_str="${ALL_RESOLVABLE_GVKS[$gvk]}"
            # Extract provider names from the "(provided by: x y)" suffix
            provs=$(echo "$info_str" | grep -oP 'provided by: \K[^)]+' | tr ' ' '\n' | sort -u)
            for p in $provs; do
                [[ -z "${REQUESTED_SET[$p]:-}" ]] && SUGGESTED_GVK_PROVIDERS["$p"]=1
            done
        done
        for dep in $(echo "${!SUGGESTED_GVK_PROVIDERS[@]}" | tr ' ' '\n' | sort); do
            # Don't double-suggest if already in the package list
            [[ -n "${ALL_MISSING_PKGS[$dep]:-}" ]] && continue
            echo "    - name: $dep    # provides required GVK"
        done
        echo ""
        OVERALL_RC=1
    else
        echo "ALL OK — all operator dependencies are satisfied."
        echo ""
    fi

    # Clean up associative arrays for next catalog iteration
    unset REQUESTED_SET ALL_MISSING_PKGS ALL_MISSING_GVKS ALL_RESOLVABLE_GVKS GVK_PROVIDERS
done

exit $OVERALL_RC
