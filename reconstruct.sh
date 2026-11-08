#!/bin/bash

OCP_VER="4.20"
#QUAY_HOSTNAME="ec2-52-15-217-213.us-east-2.compute.amazonaws.com:8443"
QUAY_HOSTNAME="ip-10-0-22-0.us-east-2.compute.internal:8443"
USERNAME="init"

# Copy out the metadata files
#podman run -d --rm --name index registry.redhat.io/redhat/redhat-operator-index:v4.20
#podman cp index:/configs .
#podman stop index


REBUILD_DIR=configs-rebuild

mkdir "${REBUILD_DIR}" 2>/dev/null

d="configs/web-terminal/"

#for d in $(find configs -maxdepth 1 -type d)
#do

  op=$(basename "${d}")

  echo "Rebuilding Operator: ${op}"

  echo -n "Package Name: "
  jq -c -r '. | select( .schema == "olm.package" ) | .name' "${d}/catalog.json"

  echo -n "Package Default Channel: "
  jq -c -r '. | select( .schema == "olm.package" ) | .defaultChannel' "${d}/catalog.json"

  # olm.channel of the default channel defined in the olm.bundle
  jq '. | select( .schema == "olm.channel" and .name == "stable" )' "${d}/catalog.json"

exit 0



  # Get all channels for the operator
  # Loop through all versions in each channel
  # If bundle exists in registry, write out the package/channel/bundle information to the op/catalog.json

#done

   jq -c '. | select( .schema == "olm.bundle" )' configs/trustee-operator/catalog.json | while read -r row; do

   bundle_name=$(echo "${row}" | jq -r '.name')
   bundle_image=$(echo "${row}" | jq -r '.image')

   #echo "bundle name: $bundle_name"
   #echo "bundle image: $bundle_image"
   mapped_image=$(echo "${bundle_image}" | sed "s|^[^/]*/|${QUAY_HOSTNAME}/|")

   echo "mapped image: ${mapped_image}"

   if skopeo inspect --tls-verify=false --authfile /home/ec2-user/quay-only.json docker://${mapped_image} 2> /dev/null 1>/dev/null
   then

     echo "bundle image exists: $mapped_image"

     if [ ! -e ${REBUILD_DIR}/${op}/catalog.json ]
     then
       mkdir "${REBUILD_DIR}/${op}"
       # Write out the olm package json to catalog.json
       jq '. | select( .schema == "olm.package" )' > ${REBUILD_DIR}/${op}/catalog.json
       # Write out the olm channel json to catalog.json
     fi

     #Write out olm bundle json to catalog.json

   fi
#done


exit 0

for op in $(find ./configs -type d)
do
  echo "Working on operator: $op"
done


jq '. | select( .schema == "olm.package" )' configs/web-terminal/catalog.json

jq '. | select( .schema == "olm.package" ) | .name' configs/web-terminal/catalog.json
jq '. | select( .schema == "olm.package" ) | .defaultChannel' configs/web-terminal/catalog.json

