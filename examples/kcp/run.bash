#!/usr/bin/env bash

# cd into repo root
example_dir="$(dirname "$0")"
cd "$(dirname "$0")/../.."
source "./hack/lib.bash"

kubeconfigs="$PWD/kubeconfigs"
log "Using directory for kubeconfigs: $kubeconfigs"
workspace_kubeconfigs="$kubeconfigs/workspaces"
mkdir -p "$workspace_kubeconfigs"

kind_platform="$kubeconfigs/platform.kubeconfig"
kind::cluster platform "$kind_platform"
helm::install::certmanager "$kind_platform"
helm::install::etcddruid "$kind_platform"
helm::install::kcp "$kind_platform"
kubectl::kustomize "$kind_platform" "./examples/kcp/platform"

kcp::setup::kubeconfigs \
    "$kind_platform" \
    "$kubeconfigs/kcp-admin.kubeconfig" \
    "$kubeconfigs/kcp-from-host.kubeconfig"

## platform
log "Setting up platform kcp workspace"
ws_platform="$workspace_kubeconfigs/platform.kubeconfig"
kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_platform" "platform"

log "Installing migration CRDs into platform workspace"
kubectl::apply \
    "$ws_platform" \
    ./config/crd/bases/broker.platform-mesh.io_migrationconfigurations.yaml \
    ./config/crd/bases/broker.platform-mesh.io_migrations.yaml

log "Setting up AcceptAPI for providers"
kcp::apiexport "$ws_platform" "./config/crd/bases/broker.platform-mesh.io_acceptapis.yaml" \
    secrets get,list,watch

log "Setting up PG for consumers"
kcp::apiexport "$ws_platform" "./config/crd/bases/example.platform-mesh.io_pgs.yaml"

## TODO cleanup
docker build -t resource-broker:kcp -f contrib/kcp/Dockerfile . \
    || die "Failed to build resource-broker image"
image_id="$(docker inspect resource-broker:kcp --format '{{.ID}}')"
image_id="${image_id//sha256:/}"
docker tag "resource-broker:kcp" "resource-broker:${image_id}"
kind load docker-image "resource-broker:$image_id" --name broker-platform \
    || die "Failed to load resource-broker image into kind cluster"

# Grab the new kubeconfig for the operator, targeting the platform
# workspace. This will be mounted into the resource-broker pod.
KUBECONFIG="$kubeconfigs/platform.kubeconfig" \
    kubectl get secret operator-kubeconfig -o jsonpath='{.data.kubeconfig}' \
        | base64 -d \
        > "$kubeconfigs/operator.kubeconfig" \
        || die "Failed to get operator kubeconfig from kind cluster"
yq -i '(.clusters[] | select(.name=="default") | .cluster.server) += ":platform"' \
    "$kubeconfigs/operator.kubeconfig" \
    || die "Failed to modify operator kubeconfig server URL"

kubectl create secret generic kcp-kubeconfig --dry-run=client -o yaml \
    --from-file=kubeconfig="$kubeconfigs/operator.kubeconfig" \
    | kubectl::apply "$kubeconfigs/platform.kubeconfig" "-"

{
    echo 'apiVersion: apps/v1'
    echo 'kind: Deployment'
    echo 'metadata:'
    echo '  name: resource-broker'
    echo '  namespace: default'
    echo 'spec:'
    echo '  replicas: 1'
    echo '  selector:'
    echo '    matchLabels:'
    echo '      app: resource-broker'
    echo '  template:'
    echo '    metadata:'
    echo '      labels:'
    echo '        app: resource-broker'
    echo '    spec:'
    echo '      containers:'
    echo '      - name: resource-broker'
    echo "        image: resource-broker:${image_id}"
    echo '        args:'
    echo '        - "-kubeconfig=/kubeconfig/kubeconfig"'
    echo '        - "-kcp-kubeconfig=/kubeconfig/kubeconfig"'
    echo '        - "-acceptapi=acceptapis"'
    echo '        - "-brokerapi=pgs"'
    echo '        - "-group=example.platform-mesh.io"'
    echo '        - "-version=v1alpha1"'
    echo '        - "-kind=PG"'
    echo '        - "-zap-devel=true"'
    # echo '        - "-zap-log-level=1"'
    # echo '        - "-zap-stacktrace-level=info"'
    echo '        volumeMounts:'
    echo '        - name: kubeconfig-volume'
    echo '          mountPath: /kubeconfig'
    echo '          readOnly: true'
    echo '      volumes:'
    echo '      - name: kubeconfig-volume'
    echo '        secret:'
    echo '          secretName: kcp-kubeconfig'
} | kubectl::apply "$kubeconfigs/platform.kubeconfig" "-"
KUBECONFIG="$kubeconfigs/platform.kubeconfig" \
    kubectl rollout status deployment/resource-broker -n default --timeout="$timeout" \
    || die "Resource broker deployment failed to roll out"
## END TODO cleanup

## provider cloud
log "Setting up cloud kind cluster"
kind_cloud="$kubeconfigs/cloud.kubeconfig"
kind::cluster cloud "$kind_cloud"

log "Setting up cloud kcp workspace"
ws_cloud="$workspace_kubeconfigs/cloud.kubeconfig"
kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_cloud" "cloud"

# Creating an APIExport and binding it in the same workspace to get a VW
# TODO replace the use of the generic VM - this should be an APIExport
# from the exported resources from api-syncagent
kcp::apiexport "$ws_cloud" "./config/crd/bases/example.platform-mesh.io_pgs.yaml"
kcp::apibinding "$ws_cloud" root:cloud pgs

# Grab the VW URL
cluster_id="$(
    KUBECONFIG="$ws_cloud" \
        kubectl get apiexportendpointslices pgs -o jsonpath='{.metadata.annotations.kcp\.io/cluster}'
)"
pgs_endpoint="$(
    KUBECONFIG="$ws_cloud" \
        kubectl get apiexportendpointslices pgs -o jsonpath='{.status.endpoints[0].url}'
)/clusters/$cluster_id"
[[ -n "$pgs_endpoint" ]] || die "Failed to get pgs VW endpoint URL"

# Create a service account for the broker to use; this should get proper
# RBAC in a prod setup
sa_token="$(kcp::serviceaccount::admin "$ws_cloud" "broker" "default")"
# Create a secret with a kubeconfig for the VW, this will be used by the
# broker to write resources into the VW.
ws_vw_cloud="${ws_cloud%%.kubeconfig}.vw.kubeconfig"
kubeconfig::create::token "$ws_vw_cloud" "$pgs_endpoint" "$sa_token"
# Push the kubeconfig into a secret in the cloud workspace, making it
# accessible to the broker through the APIBindinga that is created next.
kubectl create secret generic platform-kubeconfig --dry-run=client -o yaml \
    --from-file=kubeconfig="$ws_vw_cloud" \
    | kubectl::apply "$ws_cloud" "-"

# Bind the AcceptAPI from the platform workspace and instantiate it with
# the pgs resources. The endpoint URL is annotated so the broker knows
# where to write the resources.
kcp::apibinding "$ws_cloud" root:platform acceptapis \
    secrets test get,list,watch
kubectl::apply "$ws_cloud" "-" <<EOF
apiVersion: broker.platform-mesh.io/v1alpha1
kind: AcceptAPI
metadata:
  name: acceptapis.broker.platform-mesh.io
  annotations:
    broker.platform-mesh.io/secret-name: platform-kubeconfig
spec:
  gvr:
    group: example.platform-mesh.io
    version: v1alpha1
    resource: pgs
  filters:
    # Theseus only handles small postgres instances, so they set
    # a maximum of 5GiB storage
    - key: storage.size
      boundary:
        max: 5
EOF

## consumer
log "Setting up consumer kind cluster"
kind_consumer="$kubeconfigs/consumer.kubeconfig"
kind::cluster consumer "$kind_consumer"

log "Setting up consumer kcp workspace"
ws_consumer="$workspace_kubeconfigs/consumer.kubeconfig"
kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_consumer" "consumer"

log "Binding the PG API from platform"
kcp::apibinding "$ws_consumer" root:platform pgs

# This PG resources should be picked up by the broker and written into
# the cloud VW
kubectl::apply "$ws_consumer" "-" <<EOF
apiVersion: example.platform-mesh.io/v1alpha1
kind: PG
metadata:
  name: pg-from-consumer
spec:
  storage:
    size: 1
EOF

# TODO Workaround. For some reason the resource-broker gets stuck when
# a resource it should watch is created. Might be a timing issue? Not
# sure. For the workflow test kill the pod so the next instance sync
# correctly.
KUBECONFIG="$kind_platform" kubectl delete pods -l app=resource-broker

provider_ann() {
    KUBECONFIG="$ws_consumer" \
        kubectl get \
            -o jsonpath='{.metadata.annotations.broker\.platform-mesh\.io/provider-cluster}' \
            pg/pg-from-consumer
}

log "Waiting for PG to be annotated with provider cluster $cluster_id"
try_count=0
max_retries=120
until [[ "$(provider_ann)" == *"$cluster_id" ]]; do
    log "PG not yet annotated with provider cluster, current value: $(provider_ann)"
    try_count=$((try_count + 1))
    if [[ "$try_count" -ge "$max_retries" ]]; then
        log "Timed out waiting for PG to be annotated with provider cluster '$cluster_id': $(provider_ann)"
        log "Resource broker logs:"
        KUBECONFIG="$kind_platform" kubectl logs deployment/resource-broker
        die "PG was not annotated with provider cluster within timeout"
    fi
    sleep 1
done
log "PG annotated with provider cluster: $(provider_ann)"

exit $?

kind_storage="$kubeconfigs/storage.kubeconfig"
kind::cluster storage "$kind_storage"
ws_storage="$workspace_kubeconfigs/storage.kubeconfig"
kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_storage" "storage"
