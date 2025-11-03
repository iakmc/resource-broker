#!/usr/bin/env bash

log() { echo ">>> $@"; }
die() { echo "!!! $@" >&2; exit 1; }

# cd into repo root
cd "$(dirname "$0")/.." || die "Failed to change directory"

# set -x

command -v kind &>/dev/null || die "kind is not installed. Please install kind to proceed."

# kind get clusters | grep broker- | xargs -r -n1 kind delete cluster --name

example_dir="$PWD/example"
kubeconfigs="$PWD/kubeconfigs"
log "Using directory for kubeconfigs: $kubeconfigs"

providers="$kubeconfigs/providers"
mkdir -p "$providers"

consumers="$kubeconfigs/consumers"
mkdir -p "$consumers"

if [[ "$1" == "clean" ]]; then
    log "Cleaning up"

    log "Delete example-app in consumer homer"
    kubectl --kubeconfig "$consumers/homer.kubeconfig" delete deployment/example-app --ignore-not-found=true --wait=false

    log "Delete PG in consumer homer"
    kubectl --kubeconfig "$consumers/homer.kubeconfig" delete --wait=false pgs/pg-from-homer
    kubectl --kubeconfig "$consumers/homer.kubeconfig" patch pgs/pg-from-homer \
        --type merge -p '{"metadata":{"finalizers":[]}}'

    log "Delete PG in provider theseus"
    kubectl --kubeconfig "$providers/theseus.kubeconfig" delete --wait=false pgs/pg-from-homer
    kubectl --kubeconfig "$providers/theseus.kubeconfig" patch pgs/pg-from-homer \
        --type merge -p '{"metadata":{"finalizers":[]}}'

    log "Delete PG in provider nestor"
    kubectl --kubeconfig "$providers/nestor.kubeconfig" delete --wait=false pgs/pg-from-homer
    kubectl --kubeconfig "$providers/nestor.kubeconfig" patch pgs/pg-from-homer \
        --type merge -p '{"metadata":{"finalizers":[]}}'

    exit 0
fi

_kind_cluster() {
    rm -f "$2"
    if ! kind get clusters | grep -q "^$1$"; then
        kind create cluster --name "$1" --kubeconfig "$2" || die "Failed to create cluster $1"
    else
        kind export kubeconfig --name "$1" --kubeconfig "$2" || die "Failed to export kubeconfig for cluster $1"
    fi
}

_kapply() {
    kubectl --kubeconfig "$1" apply -f "$2" || die "Failed to apply $2 to cluster with kubeconfig $1"
}

_kustomize() {
    local try_count=0
    local max_retries=5

    while [ $try_count -lt $max_retries ]; do
        # --server-side is for cnpg operator
        if kubectl --kubeconfig "$1" kustomize --load-restrictor=LoadRestrictionsNone "$2" \
                | kubectl --kubeconfig "$1" apply -f- --server-side
        then
            return
        else
            log "Kustomize apply failed, retrying... ($((try_count + 1))/$max_retries)"
            try_count=$((try_count + 1))
            sleep 1
        fi
    done

    die "Failed to kustomize apply $2 to cluster with kubeconfig $1 after $max_retries attempts"
}

log "Setting up platform cluster"
_kind_cluster broker-platform "$kubeconfigs/platform.kubeconfig"
# TODO: The platform cluster doesn't do anything _yet_. But it will be
# used to run workloads for migrations when that is implemented.
# Might also run the broker controller from there instead of locally.

log "Setting up provider theseus"
_kind_cluster broker-theseus "$providers/theseus.kubeconfig"
KUBECONFIG="$providers/theseus.kubeconfig" \
    helm upgrade --install kro oci://registry.k8s.io/kro/charts/kro \
        --namespace kro \
        --create-namespace \
        --version=0.5.1 \
        || die "Failed to install kro in theseus"
_kustomize "$providers/theseus.kubeconfig" "$example_dir/theseus" || die "Failed to setup theseus"

log "Waiting for cnpg-controller-manager to be ready in theseus"
kubectl --kubeconfig "$providers/theseus.kubeconfig" rollout status deployment \
    -n cnpg-system cnpg-controller-manager

log "Setting up provider nestor"
_kind_cluster broker-nestor "$providers/nestor.kubeconfig"
KUBECONFIG="$providers/nestor.kubeconfig" \
    helm upgrade --install kro oci://registry.k8s.io/kro/charts/kro \
        --namespace kro \
        --create-namespace \
        --version=0.5.1 \
        || die "Failed to install kro in theseus"
# TODO: instead of applying the PG CRD configure AcceptAPI with
# a template to something else (e.g. just plain configmap or secret,
# whatever works and isn't too complex to setup)
_kustomize "$providers/nestor.kubeconfig" "$example_dir/nestor" || die "Failed to setup nestor"

log "Setting up consumer homer"
_kind_cluster broker-homer "$consumers/homer.kubeconfig" || die "Failed to create consumer homer"
_kapply "$consumers/homer.kubeconfig" ./config/crd/bases/example.platform-mesh.io_pgs.yaml

log "Starting broker"
# go run isn't being killed properly, instead the binary is built and
# run
make build || die "Failed to build manager binary"
./bin/manager \
    -kubeconfig "$kubeconfigs/platform.kubeconfig" \
    -consumer-kubeconfig-dir "$consumers" \
    -provider-kubeconfig-dir "$providers" \
    -group example.platform-mesh.io \
    -version v1alpha1 \
    -kind PG \
    &>manager.log &
broker_pid=$!
trap "kill $broker_pid; wait $broker_pid" EXIT

log "Deploying PG in homer, should land in theseus"
make build-examples
kubectl --kubeconfig "$consumers/homer.kubeconfig" delete deployment/example-app --ignore-not-found=true
kind load docker-image --name broker-homer broker-example-app:dev || die "Failed to load example app image into kind cluster homer"
_kustomize "$consumers/homer.kubeconfig" "$example_dir/homer" || die "Failed to setup homer"

log "Waiting for PG to appear in theseus"
kubectl --kubeconfig "$providers/theseus.kubeconfig" wait --for=create pgs/pg-from-homer || die "PG did not become ready in theseus"

log "Waiting for PG to become ready in theseus"
kubectl --kubeconfig "$providers/theseus.kubeconfig" wait --for=condition=Ready --timeout=120s cluster/pg-from-homer || die "cnpg Cluster did not become ready in theseus"

log "Verifying PG is not in nestor"
if kubectl --kubeconfig "$providers/nestor.kubeconfig" get pgs/pg-from-homer &>/dev/null; then
    die "PG should not be present in nestor"
fi

log "Wait for secrets to appear in homer"
if ! kubectl --kubeconfig "$consumers/homer.kubeconfig" get secrets/pg-from-homer-app &>/dev/null; then
    die "App secret did not appear in homer"
fi

log "Wait for example-app to become ready in homer"
kubectl --kubeconfig "$consumers/homer.kubeconfig" wait --for=condition=Available deployment/example-app || die "example-app did not become ready in homer"

log "Wait for application to start writing to DB"
retry_count=0
max_retries=360
while true; do
    kubectl --kubeconfig "$consumers/homer.kubeconfig" logs deployment/example-app | grep -q "added new row with value" && break
    retry_count=$((retry_count + 1))
    if [[ $retry_count -ge $max_retries ]]; then
        die "Application did not notice DB connection details change"
    fi
    sleep 1
done

log "Change PG in consumer homer to land on nestor"
kubectl --kubeconfig "$consumers/homer.kubeconfig" patch pgs pg-from-homer \
    --type merge -p '{"spec":{"storage":{"size":5120}}}' || die "Failed to patch PG in homer"

# TODO The PG should be first created in nestor and when it is ready and
# data was migrated deleted from theseus. But as the initial poc
# deleting and then creating is fine.

log "Wait for PG to vanish from theseus"
kubectl --kubeconfig "$providers/theseus.kubeconfig" wait --for=delete pgs/pg-from-homer || die "PG did not get deleted from theseus"

log "Wait for PG to appear in nestor"
kubectl --kubeconfig "$providers/nestor.kubeconfig" wait --for=create pgs/pg-from-homer || die "PG did not become ready in nestor"

log "Wait for application to notice the change"
retry_count=0
max_retries=360
while true; do
    kubectl --kubeconfig "$consumers/homer.kubeconfig" logs deployment/example-app | grep -q "DB connection details change" && break
    retry_count=$((retry_count + 1))
    if [[ $retry_count -ge $max_retries ]]; then
        die "Application did not notice DB connection details change"
    fi
    sleep 1
done
