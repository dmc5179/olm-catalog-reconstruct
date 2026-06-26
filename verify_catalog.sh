#!/usr/bin/env bash

# ==============================================================================
# Strict Error Handling & Best Practices
# ==============================================================================
set -euo pipefail

# ==============================================================================
# Helper Functions
# ==============================================================================
error_exit() {
    local message="$1"
    echo "ERROR: ${message}" >&2
    exit 1
}

log_info() {
    local message="$1"
    echo "INFO: ${message}"
}

# Translates an image URI if a source match is found in the IDMS mirror table
apply_idms_mirroring() {
    local original_image="$1"
    
    # Read the mapping table line-by-line: "source|mirror"
    while IFS="|" read -r source mirror; do
        if [[ -z "${source}" ]]; then continue; fi
        
        # Check if the image string starts with the IDMS source prefix
        if [[ "${original_image}" == "${source}"* ]]; then
            # Replace the source prefix match with the corresponding mirror target string
            local mirrored_image="${original_image/${source}/${mirror}}"
            echo "${mirrored_image}"
            return 0
        fi
    done < "${IDMS_MAP_FILE}"
    
    # If no IDMS rule intercepted the repository registry domain, return the original
    echo "${original_image}"
}

verify_image_exists() {
    local raw_image="$1"
    
    # Dynamically compute the registry layout destination based on IDMS translation tables
    local target_image
    target_image=$(apply_idms_mirroring "${raw_image}")
    
    if [[ "${raw_image}" != "${target_image}" ]]; then
        echo -n "🔄 [IDMS Redirect: ${target_image}] ... "
    fi

    # Skopeo inspect queries remote metadata architectures cleanly without downloading image layers
    if skopeo inspect --raw "docker://${target_image}" > /dev/null 2>&1; then
        return 0
    else
        return 1
    fi
}

process_catalog_file() {
    local catalog_path="$1"
    local output_dir
    output_dir=$(dirname "${catalog_path}")
    local final_output="${output_dir}/config-validated.json"

    log_info "=================================================="
    log_info "Processing file: ${catalog_path}"
    log_info "=================================================="

    # Create a clean, isolated scratch workspace for the current file processing cycle
    local current_scratch="${TEMP_DIR}/current_output.json"
    echo '{}' > "${current_scratch}"

    # Extract all unique channel names from the active catalog loop instance
    local channels
    mapfile -t CHANNELS < <(jq -r 'select(.schema == "olm.channel") | .name' "${catalog_path}")

    if [[ ${#CHANNELS[@]} -eq 0 ]]; then
        echo "  ⚠️  No channels found in ${catalog_path}. Skipping file."
        return 0
    fi

    # Iterate over each found channel track
    for channel in "${CHANNELS[@]}"; do
        log_info "  ↳ Channel Track: ${channel}"
        local versions_added_for_channel=0

        # Get all entries (versions) associated with the current loop channel
        local bundle_versions
        mapfile -t BUNDLE_VERSIONS < <(jq -r --arg chan "${channel}" 'select(.schema == "olm.channel" and .name == $chan) | .entries[].name' "${catalog_path}")
        
        for version in "${BUNDLE_VERSIONS[@]}"; do
            log_info "    Validating Version: ${version}"
            
            # Extract related images for this specific bundle version
            local related_images
            mapfile -t RELATED_IMAGES < <(jq -r --arg ver "${version}" 'select(.schema == "olm.bundle" and .name == $ver) | .relatedImages[].image' "${catalog_path}")
            
            # Include the base bundle image path if specified in the schema top-level
            local base_image
            BASE_IMAGE="$(jq -r --arg ver "${version}" 'select(.schema == "olm.bundle" and .name == $ver) | .image' "${catalog_path}")"
            if [[ -n "${BASE_IMAGE}" && "${BASE_IMAGE}" != "null" ]]; then
                RELATED_IMAGES+=("${BASE_IMAGE}")
            fi

            if [[ ${#RELATED_IMAGES[@]} -eq 0 ]]; then
                echo "      ⚠️  No container images found. Skipping version."
                continue
            fi

            local version_valid=true
            
            # Check all collected images against mapped container registries
            for img in "${RELATED_IMAGES[@]}"; do
                echo -n "      🔎 Evaluating: ${img} ... "
                if verify_image_exists "${img}"; then
                    echo "✅ EXISTS"
                else
                    echo "❌ NOT FOUND"
                    version_valid=false
                    break # Fail fast if any single image registry dependencies are broken
                fi
            done
            
            # If all associated images are validated, append to our tracking ledger object
            if [[ "${version_valid}" == "true" ]]; then
                jq --arg chan "${channel}" --arg ver "${version}" \
                   '.[$chan] += [{"name": $ver}]' "${current_scratch}" > "${current_scratch}.tmp"
                mv "${current_scratch}.tmp" "${current_scratch}"
                ((versions_added_for_channel++))
            fi
        done

        # Reconstruct original structural metadata contexts for successfully retained channels
        if [[ ${versions_added_for_channel} -gt 0 ]]; then
            local package_name schema_type
            PACKAGE_NAME="$(jq -r --arg chan "${channel}" 'select(.schema == "olm.channel" and .name == $chan) | .package' "${catalog_path}")"
            SCHEMA_TYPE="$(jq -r --arg chan "${channel}" 'select(.schema == "olm.channel" and .name == $chan) | .schema' "${catalog_path}")"
            
            jq --arg chan "${channel}" --arg pkg "${PACKAGE_NAME}" --arg sch "${SCHEMA_TYPE}" \
               '.[$chan] = {"entries": .[$chan], "name": $chan, "package": $pkg, "schema": $sch}' "${current_scratch}" > "${current_scratch}.tmp"
            mv "${current_scratch}.tmp" "${current_scratch}"
        fi
    done

    # Output and finalize file structures
    if [[ "$(jq 'length' "${current_scratch}")" -gt 0 ]]; then
        jq -c '.[]' "${current_scratch}" > "${final_output}"
        log_info "  💾 Verified configurations written straight to: ${final_output}"
    else
        log_info "  ∅ No valid assets discovered for this instance. Output artifact omitted."
    fi
}

# ==============================================================================
# Input Validation & Argument Parsing
# ==============================================================================
if [[ $# -ne 2 ]]; then
    error_exit "Usage: $0 <path_to_directory> <path_to_idms.yaml>"
fi

TARGET_DIR="$1"
IDMS_FILE="$2"

if [[ ! -d "${TARGET_DIR}" ]]; then
    error_exit "Target location is not a valid directory: ${TARGET_DIR}"
fi

if [[ ! -f "${IDMS_FILE}" ]]; then
    error_exit "IDMS mirror configuration file not found: ${IDMS_FILE}"
fi

# Create a secure isolated workspace for temporary storage tables
TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TEMP_DIR}"' EXIT

IDMS_MAP_FILE="${TEMP_DIR}/idms_mappings.txt"

# ==============================================================================
# Process and Ingest IDMS Layout Mappings
# ==============================================================================
log_info "Ingesting IDMS file rules from ${IDMS_FILE}..."
yq e '.spec.imageDigestMirrors[] | .source + "|" + .mirrors[0]' "${IDMS_FILE}" > "${IDMS_MAP_FILE}"

# ==============================================================================
# Recursive Discovery Loop
# ==============================================================================
log_info "Scanning directory tree path: ${TARGET_DIR} for target catalogs..."

# Find all catalog files safely using null-terminated characters to prevent spacing breakages
while IFS= read -r -d '' found_catalog; do
    process_catalog_file "${found_catalog}"
done < <(find "${TARGET_DIR}" -type f -name "catalog.json" -print0)

log_info "Processing run finalized across all found matches."