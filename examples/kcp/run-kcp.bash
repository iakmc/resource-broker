#!/usr/bin/env bash

# This does not actually work at the moment.
# It hinges on rewriting every url to point to the local proxied port
# instead of the in-cluster endpoint, which isn't fully implemented.

example_dir="$(dirname "$0")"
cd "$(dirname "$0")/../.." || die "Failed to change directory"

source "./hack/lib.bash"

kubeconfigs="$PWD/kubeconfigs"
workspace_kubeconfigs="$kubeconfigs/workspaces"

cd ./contrib/kcp || die "error cd'ing into kcp contrib dir"

declare -a args=()

args+=(
    # Leader election etcpp
    "-kubeconfig=$workspace_kubeconfigs/platform.kubeconfig"

    # The kcp workspace is also used as the "coordination" control
    # plane. This is where the MigrationConfiguration and Migrations are
    # managed.
    "-kcp-kubeconfig=$workspace_kubeconfigs/platform.kubeconfig"

    # The control plane to deploy compute resources to
    "-compute-kubeconfig=$kubeconfigs/platform.kubeconfig"

    # The name of the APIExportEndpointSlice to watch for new providers
    "-acceptapi=acceptapis"

    # The APIExportEndpointSlice for the polyglot API and the resource
    # to broker
    "-brokerapi=pgs"
    "-group=example.platform-mesh.io"
    "-version=v1alpha1"
    "-kind=PG"

    "-kcp-host-override=127.0.0.1"
    "-kcp-port-override=8443"
)

go run ./cmd "${args[@]}" "$@"
