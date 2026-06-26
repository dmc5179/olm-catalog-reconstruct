# OLM Catalog Validation & Container Build Pipeline

This repository contains an automation pipeline designed to validate Red Hat OpenShift Operator Lifecycle Manager (OLM) catalog files against container registries, dynamically translate registry targets using an OpenShift `ImageDigestMirrorSet` (IDMS), and bake verified catalog streams into a secure, enterprise-ready container image.

---

## 🛠️ Prerequisites

Ensure your validation environment has the following tools installed and available in your `$PATH`:

* **Bash** (v4.0 or higher)
* **jq** (v1.6 or higher) - For JSON processing
* **yq** (v4.x or higher) - For YAML processing
* **skopeo** - For remote container registry inspections (does not require root privileges or pulling full image layers)
* **podman** or **docker** - For building the secure catalog image

---

## 📄 Repository Anatomy

* `verify_catalog.sh` - The master Bash validation script.
* `Containerfile` - Dockerfile mapping the validated files into a secure UBI9-minimal ecosystem.
* `.containerignore` - Excludes unverified files from the container context boundary.
* `README.md` - This user documentation file.

---

## 🚀 Step 1: Running the Validation Script

The validation script scans a root directory recursively looking for files named `catalog.json`. For each catalog found, it uses the provided OpenShift `ImageDigestMirrorSet` configuration to intercept public repository calls, mirror them to your local registry domain, and verify their existence using `skopeo inspect`.

### Usage Syntax
```bash
./verify_catalog.sh <target_directory_path> <path_to_idms.yaml>