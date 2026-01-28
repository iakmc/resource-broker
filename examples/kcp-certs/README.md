# Certificate Brokering with kcp Example

This example extends the [certs example](../certs/README.md) by using [kcp](https://kcp.io/) (Kubernetes-like Control Plane) for communication between providers and consumers instead of direct multi-cluster access.

> **Prerequisites:** This guide assumes familiarity with the [certs example](../certs/README.md). Please read that documentation first to understand the core concepts of certificate brokering, AcceptAPIs, and resource synchronization.

## Overview

While the basic certs example uses direct kubeconfig access to multiple clusters, this example introduces kcp as a control plane layer that:

1. Uses kcp workspaces to isolate consumers and providers
2. Shares APIs between workspaces using APIExports and APIBindings
   instead of installing CRDs manually
3. Synchronizes resources between kcp workspaces and compute clusters using [api-syncagent](https://docs.kcp.io/api-syncagent/main/)

## Prerequisites

### Required Tools

- docker
- kind
- kubectl
- helm
- yq
- go
- [kcp kubectl plugins](https://docs.kcp.io/kcp/main/setup/kubectl-plugin/)

<!--
TODO: Install kubectl plugins locally via e.g. uget
-->

## Components

### Platform Cluster

The platform cluster hosts kcp and the resource-broker.

#### Platform kcp Workspace

The platform kcp workspace export the AcceptAPI for providers and the generic
Certificate API for consumers.

### Consumer kcp Workspace

The consumer workspace binds the Certificate generic API from the
platform workspace. When creating an instance the resource-broker will
be able to see and interact with it through the [Virtual Workspace](https://docs.kcp.io/kcp/main/concepts/workspaces/virtual-workspaces/) of the APIExport.

### Provider Clusters (InternalCA & ExternalCA)

The provider compute clusters run kro and cert-manager to issue certificates.
They publish their certificate API to their respective kcp workspaces using api-syncagent.

#### Provider kcp Workspaces

The provider workspaces bind the AcceptAPI from the platform workspace
and create an AcceptAPI resource to declare under which constraints they
will be able to serve Certificate resources from consumers.
The resource-broker sees these AcceptAPIs through the Virtual Workspace of the APIExport.

Additionally they are creating APIExports of their own published
Certificate API (synced from the compute cluster with api-syncagent) and
bind them in their own workspace to get a Virtual Workspace for the
platform to use.

<!-- TODO: This hackery could be too complex for new users. -->

<!--
# Adding krew to PATH early for all subsequent commands
export PATH="${KREW_ROOT:-$HOME/.krew}/bin:$PATH"
-->

## Running the Example

1. Setup the kind clusters and install components

```bash
./examples/kcp-certs/run.bash setup
```

2. Build and start the resource-broker in the platform cluster

```bash
./examples/kcp-certs/run.bash start-broker
```

3. Run the example scenario

```bash
./examples/kcp-certs/run.bash run-example
```

<!--
TODO: Split up run-example into smaller steps to show them here with explanations.
-->

Similar to the certs example, this creates a Certificate in the consumer
workspace, waits until it is provisioned by the resource-broker through
one of the providers, and then modifies it to trigger a migration to
another provider.

4. (Optional) Clean up resources created during the example

```bash noci
./examples/certs/run.bash cleanup
./examples/certs/run.bash stop-broker
```

Or delete the clusters:

```bash
kind delete cluster --name broker-platform
kind delete cluster --name broker-internalca
kind delete cluster --name broker-externalca
```
