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
vw_consumer="$workspace_kubeconfigs/consumer.vw.kubeconfig"

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
    kcp::apiexport "$ws_platform" "./config/crd/bases/example.platform-mesh.io_certificates.yaml" \
        secrets '*'

    log "Setting up internalca kcp workspace"
    kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_internalca" "internalca"
    _provider_setup_new internalca "$kind_internalca" "$ws_internalca" internal.corp

    log "Setting up externalca kcp workspace"
    kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_externalca" "externalca"
    _provider_setup_new externalca "$kind_externalca" "$ws_externalca" corp.com

    # log "Setting up consumer kind cluster"
    # TODO setup with kube-bind
    # kind::cluster consumer "$kind_consumer"

    log "Setting up consumer kcp workspace"
    kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_consumer" "consumer"

    log "Binding the Certificate API from platform"
    kcp::apibinding "$ws_consumer" root:platform certificates \
        secrets "" '*' \
        events "" '*' \
        namespaces "" '*'

    local consumer_cluster_id="$(kubectl --kubeconfig "$ws_consumer" get logicalclusters.core.kcp.io cluster -o jsonpath='{.metadata.annotations.kcp\.io/cluster}')"
    local service_endpoint_url="$(kubectl --kubeconfig "$ws_platform" get apiexportendpointslices certificates -o jsonpath='{.status.endpoints[0].url}')/clusters/$consumer_cluster_id"
    cp "$ws_platform" "$vw_consumer"
    yq -i ".clusters[].cluster.server = \"$service_endpoint_url\"" \
        "$vw_consumer"
    local old_hostname="$(kubeconfig::hostname "$vw_consumer")"
    kubeconfig::hostname::set \
        "$vw_consumer" \
        "$old_hostname" \
        "127.0.0.1:8443"
}

_cluster_id() {
    local kubeconfig="$1"
    local resource="$2"
    kubectl --kubeconfig "$kubeconfig" get "$resource" \
        -o jsonpath='{.metadata.annotations.kcp\.io/cluster}'
}

_provider_setup_new() {
    local name="$1"
    local kind_kubeconfig="$2"
    local ws_kubeconfig="$3"
    local suffix="$4"

    kcp::create_workspace "$kubeconfigs/kcp-admin.kubeconfig" "$ws_kubeconfig" "$name"

    log "Creating APIExport certificates in $name workspace"
    {
        echo "apiVersion: apis.kcp.io/v1alpha1"
        echo "kind: APIExport"
        echo "metadata:"
        echo "  name: certificates"
    } | kubectl::apply "$ws_kubeconfig" "-"

    log "Setting up $name kind cluster"
    kind::cluster "$name" "$kind_kubeconfig"
    helm::install::kro "$kind_kubeconfig"
    helm::install::certmanager "$kind_kubeconfig"
    # Installing the same resources as in the non-kcp example
    kubectl::kustomize "$kind_kubeconfig" "$example_dir/$name"
    kubectl::wait "$kind_kubeconfig" rgd/certificates.example.platform-mesh.io condition=Ready

    log "Setting up api-syncagent in $name kind cluster"

    # kubectl::kubeconfig::secret "$kind_kubeconfig" "$ws_kubeconfig" "$name" "broker-platform-control-plane:32443"

    local api_syncagent_token="$(kcp::serviceaccount::admin "$ws_kubeconfig" api-syncagent default)"
    local api_syncagent_kubeconfig="$kubeconfigs/api-syncagent-$name.kubeconfig"
    kubeconfig::create::token "$api_syncagent_kubeconfig" \
        "$(kubectl::kubeconfig::current_server_url "$ws_kubeconfig")" \
        "$api_syncagent_token"
    kubectl::kubeconfig::secret "$kind_kubeconfig" "$api_syncagent_kubeconfig" "$name" "broker-platform-control-plane:32111"

    helm::install::api_syncagent "$kind_kubeconfig" "certificates" "$name" "kubeconfig-$name" \
        --set replicas=1
    apisyncagent::publish "$kind_kubeconfig" \
        "certificates" "Certificate" "example.platform-mesh.io" "v1alpha1" \
        "certificate" "service" "Secret" "status.relatedResources.secret.name"

    log "Bind APIExport $name locally in $name workspace"
    kcp::apibinding "$ws_kubeconfig" "root:$name" certificates \
        secrets "" '*' \
        events "" '*' \
        namespaces "" '*'

    # Grab the VW endpoint URL for later use
    local cluster_id="$(_cluster_id "$ws_kubeconfig" apiexportendpointslices/certificates)"
    local endpoint_url="https://127.0.0.1:8443/services/apiexport/$cluster_id/certificates/clusters/$cluster_id"

    # Create a service account for the broker to use; this should get proper
    # RBAC in a prod setup
    local sa_token="$(kcp::serviceaccount::admin "$ws_kubeconfig" broker default)"
    # Create a kubeconfig for the VW
    local ws_vw="${ws_kubeconfig%%.kubeconfig}.vw.kubeconfig"
    kubeconfig::create::token "$ws_vw" "$endpoint_url" "$sa_token"

    # Create a secret with the kubeconfig, this will be pulled by the
    # broker.
    kubectl::kubeconfig::secret "$ws_kubeconfig" "$ws_vw" "$name" "broker-platform-control-plane:32443"

    # Bind the AcceptAPI from the platform workspace and instantiate it with
    # the certificates resources.
    kcp::apibinding "$ws_kubeconfig" root:platform acceptapis \
        secrets "" get,list,watch \
        events "" '*' \
        namespaces "" '*'

    {
        echo "apiVersion: broker.platform-mesh.io/v1alpha1"
        echo "kind: AcceptAPI"
        echo "metadata:"
        echo "  name: acceptapis.broker.platform-mesh.io"
        echo "  annotations:"
        echo "    broker.platform-mesh.io/secret-name: kubeconfig-$name"
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

_cn_from_cert_secret() {
    local kubeconfig="$1"
    local secret_name="$2"
    kubectl --kubeconfig "$kubeconfig" get secret "$secret_name" -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -subject
}

_wait_for_provider_annotation() {
    local kubeconfig="$1"
    local resource="$2"
    local expected_cluster_id="$3"
    local expected_fqdn="$4"

    log "Wait for provider annotation on $resource to be set to $expected_cluster_id"
    local try_count=0
    local max_retries=120
    local current_cluster_id="$(_provider_ann "$kubeconfig" "$resource")"
    until [[ "$current_cluster_id" == *"$expected_cluster_id" ]]; do
        log "$resource not yet annotated with provider cluster, current value: $current_cluster_id"
        try_count=$((try_count + 1))
        if [[ "$try_count" -ge "$max_retries" ]]; then
            die "Timed out waiting for $resource to be annotated with provider cluster '$expected_cluster_id': $current_cluster_id"
        fi
        sleep 1
        current_cluster_id="$(_provider_ann "$kubeconfig" "$resource")"
    done
    log "$resource annotated with provider cluster: $current_cluster_id"

    local name_try_count=0
    local secret_try_count=0
    until [[ "$secret_try_count" -ge "$max_retries" ]]; do
        local secret_name="$(kubectl --kubeconfig "$kubeconfig" get certificates cert-from-consumer -o jsonpath='{.status.relatedResources.secret.name}')"
        if [[ -z "$secret_name" ]]; then
            name_try_count=$((name_try_count + 1))
            log "Certificate secret name not yet set, waiting..."
            kubectl --kubeconfig "$kubeconfig" get certificates cert-from-consumer -o yaml
            if [[ "$name_try_count" -ge "$max_retries" ]]; then
                die "Timed out waiting for certificate secret name to be set"
            fi
            sleep 1
            continue
        fi

        log "Secret with issued certificate: $secret_name"
        local cn="$(_cn_from_cert_secret "$kubeconfig" "$secret_name")"
        if [[ "$cn" == *"$expected_fqdn"* ]]; then
            log "Certificate CN has expected fqdn '$expected_fqdn': $cn"
            return 0
        fi
        log "Certificate not yet issued, waiting..."
        secret_try_count=$((secret_try_count + 1))
        sleep 1
    done
    die "Timed out waiting for certificate to be issued with expected fqdn '$expected_fqdn'"
}

_run_example() {
    log "Order Certificate from internalca in consumer workspace"
    kubectl::apply "$ws_consumer" "$example_dir/cert.yaml"

    internalca_cluster_id="$(_cluster_id "$ws_internalca" apiexportendpointslices/certificates)"
    _wait_for_provider_annotation "$ws_consumer" certificates.example.platform-mesh.io/cert-from-consumer "$internalca_cluster_id" "app.internal.corp"

    log "Update Certificate to use externalca in consumer workspace"
    kubectl --kubeconfig "$ws_consumer" patch certificates cert-from-consumer --type merge -p '{"spec":{"fqdn":"app.corp.com"}}'

    externalca_cluster_id="$(_cluster_id "$ws_externalca" apiexportendpointslices/certificates)"
    _wait_for_provider_annotation "$ws_consumer" certificates.example.platform-mesh.io/cert-from-consumer "$externalca_cluster_id" "app.corp.com"
}

_cleanup() {
    log "Cleaning up example resources in consumer ws"
    kubectl::delete "$ws_consumer" certificates.example.platform-mesh.io/cert-from-consumer
    log "Cleaning up example resources in consumer vw"
    kubectl::delete "$vw_consumer" certificates.example.platform-mesh.io/cert-from-consumer

    log "Cleaning up example resources in internalca vw"
    kubectl::delete "$kubeconfigs/workspaces/internalca.vw.kubeconfig" certificates.example.platform-mesh.io/cert-from-consumer
    kubectl --kubeconfig "$kubeconfigs/workspaces/internalca.vw.kubeconfig" delete secrets -A --selector kro.run/owned=true
    log "Cleaning up example resources in internalca provider"
    kubectl --kubeconfig "$kind_internalca" delete certificates.example.platform-mesh.io -A --all
    kubectl --kubeconfig "$kind_internalca" delete secrets -A --selector kro.run/owned=true

    log "Cleaning up example resources in externalca vw"
    kubectl::delete "$kubeconfigs/workspaces/externalca.vw.kubeconfig" certificates.example.platform-mesh.io/cert-from-consumer
    kubectl --kubeconfig "$kubeconfigs/workspaces/externalca.vw.kubeconfig" delete secrets -A --selector kro.run/owned=true
    log "Cleaning up example resources in externalca provider"
    kubectl --kubeconfig "$kind_externalca" delete certificates.example.platform-mesh.io -A --all
    kubectl --kubeconfig "$kind_externalca" delete secrets -A --selector kro.run/owned=true

    return 0
}

_ci() {
    kubectl --kubeconfig "$kind_platform" logs deployment/resource-broker > resource-broker.log
    kubectl --kubeconfig "$ws_consumer" get certificates.example.platform-mesh.io cert-from-consumer -o yaml > consumer-certificate.yaml

    kubectl --kubeconfig "$kind_internalca" logs deployment/api-syncagent > internalca-api-syncagent.log
    kubectl --kubeconfig "$kind_internalca" logs deployment/cert-manager > internalca-cert-manager.log
    kubectl --kubeconfig "$kind_internalca" get certificates.example.platform-mesh.io -A -o yaml > internalca-certificates.yaml
    kubectl --kubeconfig "$kubeconfigs/workspaces/internalca.vw.kubeconfig" get certificates.example.platform-mesh.io -A -o yaml > internalca-vw-certificates.yaml

    kubectl --kubeconfig "$kind_externalca" logs deployment/api-syncagent > externalca-api-syncagent.log
    kubectl --kubeconfig "$kind_externalca" logs deployment/cert-manager > externalca-cert-manager.log
    kubectl --kubeconfig "$kind_externalca" get certificates.example.platform-mesh.io -A -o yaml > externalca-certificates.yaml
    kubectl --kubeconfig "$kubeconfigs/workspaces/externalca.vw.kubeconfig" get certificates.example.platform-mesh.io -A -o yaml > externalca-vw-certificates.yaml
}

if [[ -n "$CI" ]]; then
    trap _ci EXIT
fi

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
