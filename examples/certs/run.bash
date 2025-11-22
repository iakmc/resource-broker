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

    kubectl::kubeconfig::secret "$kind_platform" "$kind_platform" platform
    kubectl::kubeconfig::secret "$kind_platform" "$kind_consumer" consumer
    kubectl::kubeconfig::secret "$kind_platform" "$kind_internalca" internalca
    kubectl::kubeconfig::secret "$kind_platform" "$kind_externalca" externalca

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
        echo '        - "-group=example.platform-mesh.io"'
        echo '        - "-version=v1alpha1"'
        echo '        - "-kind=Certificate"'
        echo '        - "-zap-devel=true"'
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

_cn_from_secret() {
    local kubeconfig="$1"
    local resource="$2"

    local subject="$(kubectl --kubeconfig "$kubeconfig" get secret "$resource" -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -subject)"
    # Subject is in the format "subject=CN=example.com" or "subject = CN = example.com"
    # so strip everything before the '=' and echo without quotes to strip whitespace
    echo ${subject##*=}
}

_wait_for_subject() {
    local kubeconfig="$1"
    local resource="$2"
    local expected_subject="$3"

    local subject="$(_cn_from_secret "$kubeconfig" "$resource")"
    local retry_count=0
    local max_retries=360
    while [[ "$subject" != "$expected_subject" ]]; do
        log "Current subject is '$subject', waiting for '$expected_subject'"
        retry_count=$((retry_count + 1))
        if [[ $retry_count -ge $max_retries ]]; then
            die "Timed out waiting for correct certificate in $kubeconfig"
        fi
        sleep 1
        subject="$(_cn_from_secret "$kubeconfig" "$resource")"
    done
    log "Found expected subject '$expected_subject'"
}

_run_example() {
    log "Requesting certificate in consumer"
    kubectl::apply "$kind_consumer" "$example_dir/consumer/cert.yaml"

    log "Waiting for certificate to appear in internalca"
    kubectl::wait "$kind_internalca" certificates.example.platform-mesh.io/cert-from-consumer create
    kubectl::wait "$kind_internalca" certificates.cert-manager.io/cert-from-consumer create

    log "Waiting for certificate to become ready in internal"
    kubectl::wait "$kind_internalca" certificates.cert-manager.io/cert-from-consumer condition=Ready

    log "Verifying certificate is not in externalca"
    if kubectl --kubeconfig "$kind_externalca" get certificates.example.platform-mesh.io/cert-from-consumer &>/dev/null; then
        die "Certificate should not be present in externalca"
    fi

    log "Wait for secrets to appear in consumer"
    kubectl::wait "$kind_consumer" secrets/cert-from-consumer create

    log "Verify FQDN in secret"
    _wait_for_subject "$kind_consumer" "cert-from-consumer" "app.internal.corp"

    log "Change Certificate in consumer to request external certificate"
    kubectl --kubeconfig "$kind_consumer" patch certificates.example.platform-mesh.io/cert-from-consumer \
        --type merge -p '{"spec":{"fqdn":"app.corp.com"}}' \
        || die "Failed to patch Certificate in consumer"

    log "Waiting for Certificate to appear in externalca"
    kubectl::wait "$kind_externalca" certificates.example.platform-mesh.io/cert-from-consumer create
    kubectl::wait "$kind_externalca" certificates.cert-manager.io/cert-from-consumer create

    log "Waiting for Certificate to become ready in external"
    kubectl::wait "$kind_externalca" certificates.cert-manager.io/cert-from-consumer condition=Ready

    log "Wait for secret in consumer to be updated"
    _wait_for_subject "$kind_consumer" "cert-from-consumer" "app.corp.com"

    log "Wait for Certificate to vanish from db"
    kubectl::wait "$kind_internalca" certificates.example.platform-mesh.io/cert-from-consumer delete
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
