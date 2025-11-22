#!/usr/bin/env bash

# cd into repo root
example_dir="$(dirname "$0")"
cd "$(dirname "$0")/../.."
source "./hack/lib.bash"

if [[ -n "$CI" ]]; then
    # In CI, install kcp plugins
    kcp::setup::plugins
fi

kubeconfigs="$PWD/kubeconfigs"
log "Using directory for kubeconfigs: $kubeconfigs"

workspace_kubeconfigs="$kubeconfigs/workspaces"
mkdir -p "$workspace_kubeconfigs"

kind_platform="$kubeconfigs/platform.kubeconfig"
ws_platform="$workspace_kubeconfigs/platform.kubeconfig"

kind_internalca="$kubeconfigs/internalca.kubeconfig"
ws_internalca="$workspace_kubeconfigs/internalca.kubeconfig"

kind_externalca="$kubeconfigs/externalca.kubeconfig"
ws_externalca="$workspace_kubeconfigs/externalca.kubeconfig"

kind_consumer="$kubeconfigs/consumer.kubeconfig"
ws_consumer="$workspace_kubeconfigs/consumer.kubeconfig"

_setup() {
    log "Setting up platform cluster"
    kind::cluster platform "$kind_platform"
    helm::install::certmanager "$kind_platform"
    helm::install::etcddruid "$kind_platform"
    helm::install::kcp "$kind_platform"
    kubectl::kustomize "$kind_platform" "./examples/kcp-certs/platform"

    log "Setting up kcp"
    kcp::setup::kubeconfigs \
        "$kind_platform" \
        "$kubeconfigs/kcp-admin.kubeconfig" \
        "$kubeconfigs/kcp-from-host.kubeconfig"

    log "Setting up platform kcp workspace"
    kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_platform" "platform"

    log "Installing migration CRDs into platform workspace"
    kubectl::apply \
        "$ws_platform" \
        ./config/crd/bases/broker.platform-mesh.io_migrationconfigurations.yaml \
        ./config/crd/bases/broker.platform-mesh.io_migrations.yaml

    log "Setting up AcceptAPI APIExport for providers"
    kcp::apiexport "$ws_platform" "./config/crd/bases/broker.platform-mesh.io_acceptapis.yaml" \
        secrets get,list,watch

    log "Setting up Certificate APIExport for consumers"
    kcp::apiexport "$ws_platform" "./config/crd/bases/example.platform-mesh.io_certificates.yaml"

    # log "Setting up internalca kind cluster"
    # TODO: Setup with api-syncagent
    # kind::cluster internalca "$kind_internalca"

    log "Setting up internalca kcp workspace"
    kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_internalca" "internalca"
    _provider_setup "$ws_internalca" internalca internal.corp

    # log "Setting up externalca kind cluster"
    # TODO: Setup with api-syncagent
    # kind::cluster externalca "$kind_externalca"

    log "Setting up externalca kcp workspace"
    kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_externalca" "externalca"
    _provider_setup "$ws_externalca" externalca corp.com

    # log "Setting up consumer kind cluster"
    # TODO setup with kube-bind
    # kind::cluster consumer "$kind_consumer"

    log "Setting up consumer kcp workspace"
    kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_consumer" "consumer"

    log "Binding the Certificate API from platform"
    kcp::apibinding "$ws_consumer" root:platform certificates
}

_cluster_id() {
    local kubeconfig="$1"
    local resource="$2"
    kubectl --kubeconfig "$kubeconfig" get "$resource" \
        -o jsonpath='{.metadata.annotations.kcp\.io/cluster}'
}

_provider_setup() {
    local ws_kubeconfig="$1"
    local name="$2"
    local suffix="$3"

    # Creating an APIExport and binding it in the same workspace to get a VW
    # TODO replace the use of the generic VM - this should be an APIExport
    # from the exported resources from api-syncagent
    kcp::apiexport "$ws_kubeconfig" "./config/crd/bases/example.platform-mesh.io_certificates.yaml"
    kcp::apibinding "$ws_kubeconfig" "root:$name" certificates

    # Grab the VW endpoint URL for later use
    cluster_id="$(_cluster_id "$ws_kubeconfig" apiexportendpointslices/certificates)"
    endpoint_url="$(
        KUBECONFIG="$ws_kubeconfig" \
            kubectl get apiexportendpointslices certificates -o jsonpath='{.status.endpoints[0].url}'
    )/clusters/$cluster_id"
    [[ -n "$endpoint_url" ]] || die "Failed to get $name certificates VW endpoint URL"

    # Create a service account for the broker to use; this should get proper
    # RBAC in a prod setup
    sa_token="$(kcp::serviceaccount::admin "$ws_kubeconfig" broker default)"
    # Create a secret with a kubeconfig for the VW, this will be used by the
    # broker to write resources into the VW.
    ws_vw="${ws_kubeconfig%%.kubeconfig}.vw.kubeconfig"
    kubeconfig::create::token "$ws_vw" "$endpoint_url" "$sa_token"
    # Push the kubeconfig into a secret in the cloud workspace, making it
    # accessible to the broker through the APIBindinga that is created next.
    kubectl create secret generic platform-kubeconfig --dry-run=client -o yaml \
        --from-file=kubeconfig="$ws_vw" \
        | kubectl::apply "$ws_kubeconfig" "-"

    # Bind the AcceptAPI from the platform workspace and instantiate it with
    # the certificates resources.
    kcp::apibinding "$ws_kubeconfig" root:platform acceptapis \
        secrets test get,list,watch

    {
        echo "apiVersion: broker.platform-mesh.io/v1alpha1"
        echo "kind: AcceptAPI"
        echo "metadata:"
        echo "  name: acceptapis.broker.platform-mesh.io"
        echo "  annotations:"
        echo "    broker.platform-mesh.io/secret-name: platform-kubeconfig"
        echo "spec:"
        echo "  gvr:"
        echo "    group: example.platform-mesh.io"
        echo "    version: v1alpha1"
        echo "    resource: certificates"
        echo "  filters:"
        echo "    - key: fqdn"
        echo "      suffix: $suffix"
    } | kubectl::apply "$ws_kubeconfig" "-"
}

_start_broker() {
    log "Starting broker"

    docker build -t resource-broker:kcp -f contrib/kcp/Dockerfile . \
        || die "Failed to build resource-broker image"
    image_id="$(docker inspect resource-broker:kcp --format '{{.ID}}')"
    image_id="${image_id//sha256:/}"
    docker tag "resource-broker:kcp" "resource-broker:${image_id}"
    kind load docker-image "resource-broker:$image_id" --name broker-platform \
        || die "Failed to load resource-broker image into kind cluster"

    # Grab the new kubeconfig for the operator, targeting the platform
    # workspace. This will be mounted into the resource-broker pod.
    KUBECONFIG="$kind_platform" \
        kubectl get secret operator-kubeconfig -o jsonpath='{.data.kubeconfig}' \
            | base64 -d \
            > "$kubeconfigs/operator.kubeconfig" \
            || die "Failed to get operator kubeconfig from kind cluster"
    yq -i '(.clusters[] | select(.name=="default") | .cluster.server) += ":platform"' \
        "$kubeconfigs/operator.kubeconfig" \
        || die "Failed to modify operator kubeconfig server URL"

    kubectl create secret generic kcp-kubeconfig --dry-run=client -o yaml \
        --from-file=kubeconfig="$kubeconfigs/operator.kubeconfig" \
        | kubectl::apply "$kind_platform" "-"

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
        echo '        - "-brokerapi=certificates"'
        echo '        - "-group=example.platform-mesh.io"'
        echo '        - "-version=v1alpha1"'
        echo '        - "-kind=Certificate"'
        echo '        - "-zap-devel=true"'
        echo '        volumeMounts:'
        echo '        - name: kubeconfig-volume'
        echo '          mountPath: /kubeconfig'
        echo '          readOnly: true'
        echo '      volumes:'
        echo '      - name: kubeconfig-volume'
        echo '        secret:'
        echo '          secretName: kcp-kubeconfig'
    } | kubectl::apply "$kind_platform" "-"
    KUBECONFIG="$kind_platform" \
        kubectl rollout status deployment/resource-broker -n default --timeout="$timeout" \
        || die "Resource broker deployment failed to roll out"
}

_stop_broker() {
    kubectl::delete "$kind_platform" deployment/resource-broker
}

_provider_ann() {
    local kubeconfig="$1"
    local resource="$2"
    kubectl --kubeconfig "$kubeconfig" get "$resource" \
        -o jsonpath='{.metadata.annotations.broker\.platform-mesh\.io/provider-cluster}'
}

_run_example() {
    log "Order Certificate in consumer workspace"
    kubectl::apply "$ws_consumer" ./examples/certs/consumer/cert.yaml

    internalca_cluster_id="$(_cluster_id "$ws_internalca" apiexportendpointslices/certificates)"
    log "Wait for provider annotation on certificate to be set to internalca cluster $internalca_cluster_id"
    try_count=0
    max_retries=120
    current_cluster_id="$(_provider_ann "$ws_consumer" certificates.example.platform-mesh.io/cert-from-consumer)"
    until [[ "$current_cluster_id" == *"$internalca_cluster_id" ]]; do
        log "Certificate not yet annotated with internalca provider cluster, current value: $current_cluster_id"
        try_count=$((try_count + 1))
        if [[ "$try_count" -ge "$max_retries" ]]; then
            die "Timed out waiting for certificate to be annotated with internalca provider cluster '$internalca_cluster_id': $current_cluster_id"
        fi
        sleep 1
        current_cluster_id="$(_provider_ann "$ws_consumer" certificates.example.platform-mesh.io/cert-from-consumer)"
    done
    log "Certificate annotated with internalca provider cluster: $current_cluster_id"
}

_cleanup() {
    kubectl::delete "$ws_consumer" \
        certificates.example.platform-mesh.io/cert-from-consumer \
        secret/cert-from-consumer
    return 0
}

case "$1" in
    (setup) _setup ;;
    (cleanup) _cleanup ;;
    (start-broker) _start_broker ;;
    (stop-broker) _stop_broker ;;
    (run-example) _run_example ;;
    ("")
        _setup || die "Setup failed"
        _start_broker || die "Starting broker failed"
        _cleanup || die "Cleanup failed"
        _run_example || die "Running example failed"
        ;;
    (*) die "Unknown command: $1" ;;
esac
