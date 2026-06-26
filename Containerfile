# Use the official Red Hat UBI 9 Minimal base image
FROM registry.access.redhat.com/ubi9/ubi9-minimal:latest

# Set metadata labels for governance and security auditing
LABEL maintainer="AOS Logging Team" \
      summary="Validated OLM Operator Catalog Cache" \
      description="Contains verified operator catalogs mapping directly to validated container registry mirrors."

# Define the target directory inside the container where files will live
ENV CATALOG_DIR=/opt/olm-catalogs

# Set the working directory
WORKDIR ${CATALOG_DIR}

# ----------------==================================
# 1. Copy the base directory tree layout
# ----------------==================================
# Copy everything from your target path into the container first.
# To prevent the original config.json files from bloating your layer cache, 
# match it with an associated .containerignore file (detailed below).
COPY . .

# ----------------==================================
# 2. Dynamic Renaming Configuration Layer
# ----------------==================================
# Run a secure, minimized execution loop to find all validated sheets 
# and explicitly swap their naming masks to match your cluster lookup requirements.
RUN find ${CATALOG_DIR} -type f -name "config-validated.json" | while read -r validated_file; do \
        dir_path=$(dirname "$validated_file"); \
        mv "$validated_file" "${dir_path}/config.json" && \
        echo "Successfully activated validated configuration ledger for: $(basename $dir_path)"; \
    done

# Drop root privileges and enforce standard non-root container security (User ID 1001)
USER 1001

# Command just to keep the payload browsable or standard for file serving pods
CMD ["/usr/bin/microdnf", "--version"]