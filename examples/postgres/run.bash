#!/usr/bin/env bash

# cd into repo root
example_dir="$(dirname "$0")"
cd "$(dirname "$0")/../.."
source "./hack/lib.bash"

kubeconfigs="$PWD/kubeconfigs"
log "Using directory for kubeconfigs: $kubeconfigs"

kind_platform="$kubeconfigs/platform.kubeconfig"
kind_consumer="$kubeconfigs/consumer.kubeconfig"
kind_db="$kubeconfigs/db.kubeconfig"
kind_cloud="$kubeconfigs/cloud.kubeconfig"

if [[ "$1" == "clean" ]]; then
    log "Cleaning up"

    log "Delete example-app in consumer consumer"
    kubectl --kubeconfig "$kind_consumer" delete deployment/example-app --ignore-not-found=true --wait=false

    log "Delete PG in consumer consumer"
    kubectl --kubeconfig "$kind_consumer" delete --wait=false pgs/pg-from-consumer
    kubectl --kubeconfig "$kind_consumer" patch pgs/pg-from-consumer \
        --type merge -p '{"metadata":{"finalizers":[]}}'

    log "Delete PG in provider db"
    kubectl --kubeconfig "$kind_db" delete --wait=false pgs/pg-from-consumer
    kubectl --kubeconfig "$kind_db" patch pgs/pg-from-consumer \
        --type merge -p '{"metadata":{"finalizers":[]}}'
    kubectl --kubeconfig "$kind_db" delete --wait=false clusters/pg-from-consumer

    log "Delete PG in provider cloud"
    kubectl --kubeconfig "$kind_cloud" delete --wait=false pgs/pg-from-consumer
    kubectl --kubeconfig "$kind_cloud" patch pgs/pg-from-consumer \
        --type merge -p '{"metadata":{"finalizers":[]}}'
    kubectl --kubeconfig "$kind_cloud" delete --wait=false clusters/pg-from-consumer

    exit 0
fi

log "Setting up platform cluster"
kind::cluster platform "$kind_platform"
# Platform only needs the Migration and MigrationConfiguration CRDs but
# mcmanager currently requries all to be installed.
kubectl::kustomize "$kind_platform" ./config/crd/

log "Setting up provider db"
kind::cluster db "$kind_db"
helm::install::kro "$kind_db"
kubectl::kustomize "$kind_db" "$example_dir/db"

log "Waiting for cnpg-controller-manager to be ready in db"
kubectl --kubeconfig "$kind_db" rollout status deployment \
    -n cnpg-system cnpg-controller-manager

log "Setting up provider cloud"
kind::cluster cloud "$kind_cloud"
helm::install::kro "$kind_cloud"
kubectl::kustomize "$kind_cloud" "$example_dir/cloud"

log "Setting up consumer consumer"
kind::cluster consumer "$kind_consumer"
kubectl::kustomize "$kind_consumer" "$example_dir/consumer"

log "Starting broker"
# go run isn't being killed properly, instead the binary is built and
# run
make build || die "Failed to build manager binary"
./bin/manager \
    -kubeconfig "$kind_platform" \
    -coordination-kubeconfig "$kind_platform" \
    -compute-kubeconfig "$kind_platform" \
    -consumer-kubeconfig "$kind_consumer" \
    -provider-kubeconfig "$kind_db,$kind_cloud" \
    -group example.platform-mesh.io \
    -version v1alpha1 \
    -kind PG \
    &>manager.log &
broker_pid=$!
trap "kill $broker_pid; wait $broker_pid" EXIT

log "Deploying PG in consumer, should land in db"
make build-examples
kubectl --kubeconfig "$kind_consumer" delete deployment/example-app --ignore-not-found=true
kind load docker-image --name broker-consumer broker-example-app:dev \
    || die "Failed to load example app image into kind cluster consumer"
kubectl::kustomize "$kind_consumer" "$example_dir/consumer"

log "Waiting for PG to appear in db"
kubectl::wait "$kind_db" pgs/pg-from-consumer create
kubectl::wait "$kind_db" cluster/pg-from-consumer create

log "Waiting for PG to become ready in db"
kubectl::wait "$kind_db" cluster/pg-from-consumer condition=Ready

log "Verifying PG is not in cloud"
if kubectl --kubeconfig "$kind_cloud" get pgs/pg-from-consumer &>/dev/null; then
    die "PG should not be present in cloud"
fi

log "Wait for secrets to appear in consumer"
if ! kubectl --kubeconfig "$kind_consumer" get secrets/pg-from-consumer-app &>/dev/null; then
    die "App secret did not appear in consumer"
fi

log "Wait for example-app to become ready in consumer"
kubectl::wait "$kind_consumer" deployment/example-app condition=Available

log "Wait for application to start writing to DB"
retry_count=0
max_retries=360
while true; do
    kubectl --kubeconfig "$kind_consumer" logs deployment/example-app | grep -q "added new row with value" && break
    retry_count=$((retry_count + 1))
    if [[ $retry_count -ge $max_retries ]]; then
        die "Application did not notice DB connection details change"
    fi
    sleep 1
done

log "Change PG in consumer consumer to land on cloud"
kubectl --kubeconfig "$kind_consumer" patch pgs pg-from-consumer \
    --type merge -p '{"spec":{"storage":{"size":5120}}}' \
    || die "Failed to patch PG in consumer"

log "Waiting for PG to appear in cloud"
kubectl::wait "$kind_cloud" pgs/pg-from-consumer create
kubectl::wait "$kind_cloud" cluster/pg-from-consumer create

log "Waiting for PG to become ready in cloud"
kubectl::wait "$kind_cloud" cluster/pg-from-consumer condition=Ready

log "Wait for application to notice the change"
retry_count=0
max_retries=360
while true; do
    kubectl --kubeconfig "$kind_consumer" logs deployment/example-app | grep -q "DB connection details change" && break
    retry_count=$((retry_count + 1))
    if [[ $retry_count -ge $max_retries ]]; then
        die "Application did not notice DB connection details change"
    fi
    sleep 1
done

log "Wait for PG to vanish from db"
kubectl::wait "$kind_db" pgs/pg-from-consumer delete
