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

    log "Delete example-app in consumer consumer"
    kubectl --kubeconfig "$consumers/consumer.kubeconfig" delete deployment/example-app --ignore-not-found=true --wait=false

    log "Delete PG in consumer consumer"
    kubectl --kubeconfig "$consumers/consumer.kubeconfig" delete --wait=false pgs/pg-from-consumer
    kubectl --kubeconfig "$consumers/consumer.kubeconfig" patch pgs/pg-from-consumer \
        --type merge -p '{"metadata":{"finalizers":[]}}'

    log "Delete PG in provider db"
    kubectl --kubeconfig "$providers/db.kubeconfig" delete --wait=false pgs/pg-from-consumer
    kubectl --kubeconfig "$providers/db.kubeconfig" patch pgs/pg-from-consumer \
        --type merge -p '{"metadata":{"finalizers":[]}}'

    log "Delete PG in provider cloud"
    kubectl --kubeconfig "$providers/cloud.kubeconfig" delete --wait=false pgs/pg-from-consumer
    kubectl --kubeconfig "$providers/cloud.kubeconfig" patch pgs/pg-from-consumer \
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

log "Setting up provider db"
_kind_cluster broker-db "$providers/db.kubeconfig"
KUBECONFIG="$providers/db.kubeconfig" \
    helm upgrade --install kro oci://registry.k8s.io/kro/charts/kro \
        --namespace kro \
        --create-namespace \
        --version=0.5.1 \
        || die "Failed to install kro in db"
# TODO: instead of applying the PG CRD configure AcceptAPI with
# a template to something else (e.g. just plain configmap or secret,
# whatever works and isn't too complex to setup)
_kustomize "$providers/db.kubeconfig" "$example_dir/db" || die "Failed to setup db"

log "Waiting for cnpg-controller-manager to be ready in db"
kubectl --kubeconfig "$providers/db.kubeconfig" rollout status deployment \
    -n cnpg-system cnpg-controller-manager

log "Setting up provider cloud"
_kind_cluster broker-cloud "$providers/cloud.kubeconfig"
KUBECONFIG="$providers/cloud.kubeconfig" \
    helm upgrade --install kro oci://registry.k8s.io/kro/charts/kro \
        --namespace kro \
        --create-namespace \
        --version=0.5.1 \
        || die "Failed to install kro in cloud"
_kustomize "$providers/cloud.kubeconfig" "$example_dir/cloud" || die "Failed to setup cloud"

log "Setting up consumer consumer"
_kind_cluster broker-consumer "$consumers/consumer.kubeconfig" || die "Failed to create consumer consumer"
_kapply "$consumers/consumer.kubeconfig" ./config/crd/bases/example.platform-mesh.io_pgs.yaml

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

log "Deploying PG in consumer, should land in db"
make build-examples
kubectl --kubeconfig "$consumers/consumer.kubeconfig" delete deployment/example-app --ignore-not-found=true
kind load docker-image --name broker-consumer broker-example-app:dev || die "Failed to load example app image into kind cluster consumer"
_kustomize "$consumers/consumer.kubeconfig" "$example_dir/consumer" || die "Failed to setup consumer"

log "Waiting for PG to appear in db"
kubectl --kubeconfig "$providers/db.kubeconfig" wait --for=create pgs/pg-from-consumer || die "PG did not become ready in db"

log "Waiting for PG to become ready in db"
kubectl --kubeconfig "$providers/db.kubeconfig" wait --for=condition=Ready --timeout=120s cluster/pg-from-consumer || die "cnpg Cluster did not become ready in db"

log "Verifying PG is not in cloud"
if kubectl --kubeconfig "$providers/cloud.kubeconfig" get pgs/pg-from-consumer &>/dev/null; then
    die "PG should not be present in cloud"
fi

log "Wait for secrets to appear in consumer"
if ! kubectl --kubeconfig "$consumers/consumer.kubeconfig" get secrets/pg-from-consumer-app &>/dev/null; then
    die "App secret did not appear in consumer"
fi

log "Wait for example-app to become ready in consumer"
kubectl --kubeconfig "$consumers/consumer.kubeconfig" wait --for=condition=Available deployment/example-app || die "example-app did not become ready in consumer"

log "Wait for application to start writing to DB"
retry_count=0
max_retries=360
while true; do
    kubectl --kubeconfig "$consumers/consumer.kubeconfig" logs deployment/example-app | grep -q "added new row with value" && break
    retry_count=$((retry_count + 1))
    if [[ $retry_count -ge $max_retries ]]; then
        die "Application did not notice DB connection details change"
    fi
    sleep 1
done

log "Change PG in consumer consumer to land on cloud"
kubectl --kubeconfig "$consumers/consumer.kubeconfig" patch pgs pg-from-consumer \
    --type merge -p '{"spec":{"storage":{"size":5120}}}' || die "Failed to patch PG in consumer"

# TODO The PG should be first created in cloud and when it is ready and
# data was migrated deleted from db. But as the initial poc
# deleting and then creating is fine.

log "Wait for PG to appear in cloud"
kubectl --kubeconfig "$providers/cloud.kubeconfig" wait --for=create pgs/pg-from-consumer || die "PG did not become ready in cloud"

log "Wait for PG to vanish from db"
kubectl --kubeconfig "$providers/db.kubeconfig" wait --for=delete pgs/pg-from-consumer || die "PG did not get deleted from db"

log "Wait for application to notice the change"
retry_count=0
max_retries=360
while true; do
    kubectl --kubeconfig "$consumers/consumer.kubeconfig" logs deployment/example-app | grep -q "DB connection details change" && break
    retry_count=$((retry_count + 1))
    if [[ $retry_count -ge $max_retries ]]; then
        die "Application did not notice DB connection details change"
    fi
    sleep 1
done
