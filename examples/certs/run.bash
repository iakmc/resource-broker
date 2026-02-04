#!/usr/bin/env bash

# cd into repo root
example_dir="$(dirname "$0")"
cd "$(dirname "$0")/../.."
source "./hack/lib.bash"

kubeconfigs="$PWD/kubeconfigs"
log "Using directory for kubeconfigs: $kubeconfigs"

kind_platform="$kubeconfigs/platform.kubeconfig"
kind_consumer="$kubeconfigs/consumer.kubeconfig"
kind_internalca="$kubeconfigs/internalca.kubeconfig"
kind_externalca="$kubeconfigs/externalca.kubeconfig"

_setup() {
    log "Setting up platform cluster"
    kind::cluster platform "$kind_platform"
    # Platform only needs the Migration and MigrationConfiguration CRDs but
    # mcmanager currently requries all to be installed.
    kubectl::kustomize "$kind_platform" ./config/crd/

    log "Setting up provider internalca"
    kind::cluster internalca "$kind_internalca"
    helm::install::kro "$kind_internalca"
    helm::install::certmanager "$kind_internalca"
    kubectl::kustomize "$kind_internalca" "$example_dir/internalca"

    log "Setting up provider externalca"
    kind::cluster externalca "$kind_externalca"
    helm::install::kro "$kind_externalca"
    helm::install::certmanager "$kind_externalca"
    kubectl::kustomize "$kind_externalca" "$example_dir/externalca"

    log "Setting up consumer consumer"
    kind::cluster consumer "$kind_consumer"
    kubectl::kustomize "$kind_consumer" "$example_dir/consumer"
}

_start_broker() {
    log "Starting broker"

    make docker-build || die "Failed to build resource-broker image"
    local image_id="$(docker inspect resource-broker:latest --format '{{.ID}}')"
    image_id="${image_id//sha256:/}"
    docker tag "resource-broker:latest" "resource-broker:${image_id}"
    kind load docker-image "resource-broker:${image_id}" --name broker-platform \
        || die "Failed to load resource-broker image into kind cluster"

    kubectl::kubeconfig::secret "$kind_platform" "$kind_platform" platform broker-platform-control-plane:6443
    kubectl::kubeconfig::secret "$kind_platform" "$kind_consumer" consumer broker-consumer-control-plane:6443
    kubectl::kubeconfig::secret "$kind_platform" "$kind_internalca" internalca broker-internalca-control-plane:6443
    kubectl::kubeconfig::secret "$kind_platform" "$kind_externalca" externalca broker-externalca-control-plane:6443

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
        echo '        - "-kubeconfig=/kubeconfigs/platform/kubeconfig"'
        echo '        - "-coordination-kubeconfig=/kubeconfigs/platform/kubeconfig"'
        echo '        - "-compute-kubeconfig=/kubeconfigs/platform/kubeconfig"'
        echo '        - "-consumer-kubeconfig=/kubeconfigs/consumer/kubeconfig"'
        echo '        - "-provider-kubeconfig=/kubeconfigs/internalca/kubeconfig,/kubeconfigs/externalca/kubeconfig"'
        echo '        - "-zap-devel=true"'
        echo '        - "-watch-kind=Certificate.v1alpha1.example.platform-mesh.io"'
        echo '        volumeMounts:'
        echo '        - name: kubeconfig-platform'
        echo '          mountPath: /kubeconfigs/platform'
        echo '          readOnly: true'
        echo '        - name: kubeconfig-consumer'
        echo '          mountPath: /kubeconfigs/consumer'
        echo '          readOnly: true'
        echo '        - name: kubeconfig-internalca'
        echo '          mountPath: /kubeconfigs/internalca'
        echo '          readOnly: true'
        echo '        - name: kubeconfig-externalca'
        echo '          mountPath: /kubeconfigs/externalca'
        echo '          readOnly: true'
        echo '      volumes:'
        echo '      - name: kubeconfig-platform'
        echo '        secret:'
        echo '          secretName: kubeconfig-platform'
        echo '      - name: kubeconfig-consumer'
        echo '        secret:'
        echo '          secretName: kubeconfig-consumer'
        echo '      - name: kubeconfig-internalca'
        echo '        secret:'
        echo '          secretName: kubeconfig-internalca'
        echo '      - name: kubeconfig-externalca'
        echo '        secret:'
        echo '          secretName: kubeconfig-externalca'
    } | kubectl::apply "$kind_platform" "-"
    KUBECONFIG="$kubeconfigs/platform.kubeconfig" \
        kubectl rollout status deployment/resource-broker -n default --timeout="$timeout" \
        || die "Resource broker deployment failed to roll out"
}

_stop_broker() {
    kubectl::delete "$kind_platform" deployment/resource-broker
}

_cleanup() {
    kubectl::delete "$kind_consumer" \
        certificates.example.platform-mesh.io/cert-from-consumer \
        secret/cert-from-consumer

    kubectl::delete "$kind_internalca" \
        certificates.example.platform-mesh.io/cert-from-consumer \
        certificates.cert-manager.io/cert-from-consumer \
        secret/cert-from-consumer

    kubectl::delete "$kind_externalca" \
        certificates.example.platform-mesh.io/cert-from-consumer \
        certificates.cert-manager.io/cert-from-consumer \
        secret/cert-from-consumer
    return 0
}

_ci() {
    kubectl --kubeconfig "$kind_platform" logs deployment/resource-broker > resource-broker.log
    kubectl --kubeconfig "$kind_consumer" get certificates.example.platform-mesh.io -A -o yaml > consumer-certificates.yaml

    kubectl --kubeconfig "$kind_internalca" logs deployment/cert-manager > internalca-cert-manager.log
    kubectl --kubeconfig "$kind_internalca" get certificates.example.platform-mesh.io -A -o yaml > internalca-certificates.yaml

    kubectl --kubeconfig "$kind_externalca" logs deployment/cert-manager > externalca-cert-manager.log
    kubectl --kubeconfig "$kind_externalca" get certificates.example.platform-mesh.io -A -o yaml > externalca-certificates.yaml
}

case "$1" in
    (setup) _setup ;;
    (cleanup) _cleanup ;;
    (start-broker) _start_broker ;;
    (stop-broker) _stop_broker ;;
    (ci) _ci;;
    ("")
        _setup || die "Setup failed"
        _start_broker || die "Starting broker failed"
        _cleanup || die "Cleanup failed"
        _run_example || die "Running example failed"
        ;;
    (*) die "Unknown command: $1" ;;
esac
