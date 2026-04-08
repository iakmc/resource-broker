/*
Copyright The Platform Mesh Authors.
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package broker

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	kcpapisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	kcpapisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	kcpcore "github.com/kcp-dev/sdk/apis/core"
	"golang.org/x/sync/errgroup"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	mctrl "sigs.k8s.io/multicluster-runtime"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	"sigs.k8s.io/multicluster-runtime/providers/clusters"
	"sigs.k8s.io/multicluster-runtime/providers/multi"
	"sigs.k8s.io/multicluster-runtime/providers/single"

	brokerv1alpha1 "github.com/platform-mesh/resource-broker/api/broker/v1alpha1"
	kcpacceptapi "github.com/platform-mesh/resource-broker/contrib/kcp/pkg/acceptapi"
	"github.com/platform-mesh/resource-broker/contrib/kcp/pkg/stagingworkspace"
	"github.com/platform-mesh/resource-broker/pkg/broker"
	genericreconciler "github.com/platform-mesh/resource-broker/pkg/broker/generic"
	"github.com/platform-mesh/resource-broker/pkg/broker/migration"
)

const (
	// stagingConsumerClusterLabel is the label key on StagingWorkspace objects
	// that stores the consumer cluster name for efficient lookup.
	stagingConsumerClusterLabel = "broker.platform-mesh.io/consumer-cluster"

	// stagingProviderClusterLabel is the label key on StagingWorkspace objects
	// that stores the provider cluster name used to look up AcceptAPIs.
	stagingProviderClusterLabel = "broker.platform-mesh.io/provider-cluster"

	// stagingAPIExportLabel is the label key on StagingWorkspace objects that
	// stores the APIExport name bound in that workspace. Together with the
	// consumer and provider labels it forms a unique (consumer, provider,
	// apiexport) tuple, allowing one provider to serve multiple APIExports to
	// the same consumer via separate staging workspaces.
	stagingAPIExportLabel = "broker.platform-mesh.io/api-export"

	// stagingNewClusterAnn is the annotation key on a StagingWorkspace used to
	// record the migration-target staging cluster during a provider migration.
	stagingNewClusterAnn = "broker.platform-mesh.io/new-staging-cluster"

	// stagingResourceFinalizerPrefix is the prefix for per-resource finalizers
	// added to StagingWorkspace CRs. Each active consumer resource adds one
	// finalizer; when all are removed the StagingWorkspace CR is deleted.
	stagingResourceFinalizerPrefix = "broker.platform-mesh.io/resource-"
)

// Options are the options for creating a Broker.
type Options struct {
	Name       string
	Log        logr.Logger
	WatchKinds []string

	LocalConfig           *rest.Config
	KcpConfig             *rest.Config
	MigrationCoordination *rest.Config
	ComputeConfig         *rest.Config

	AcceptAPIName     string
	BrokerAPIName     string
	WorkspaceTreeRoot string
}

func (o Options) validate() error {
	if o.Name == "" {
		return fmt.Errorf("name is required")
	}
	if o.Log.GetSink() == nil {
		return fmt.Errorf("log is required")
	}
	if o.LocalConfig == nil {
		return fmt.Errorf("local config is required")
	}
	if o.KcpConfig == nil {
		return fmt.Errorf("kcp config is required")
	}
	if o.MigrationCoordination == nil {
		return fmt.Errorf("migration coordination config is required")
	}
	if o.ComputeConfig == nil {
		return fmt.Errorf("compute config is required")
	}
	if o.AcceptAPIName == "" {
		return fmt.Errorf("accept api name is required")
	}
	if o.BrokerAPIName == "" {
		return fmt.Errorf("broker api name is required")
	}
	if o.WorkspaceTreeRoot == "" {
		return fmt.Errorf("workspace tree root is required")
	}
	if len(o.WatchKinds) == 0 {
		return fmt.Errorf("at least one watch kinds is required")
	}
	return nil
}

// Broker brokers API resources to clusters that have accepted given APIs.
type Broker struct {
	opts Options

	lock     sync.RWMutex
	managers map[string]mctrl.Manager

	// apiAccepters maps GVRs to provider cluster names to AcceptAPIs.
	// GVR -> providerClusterName -> acceptAPI.Name -> AcceptAPI
	apiAccepters map[metav1.GroupVersionResource]map[string]map[string]brokerv1alpha1.AcceptAPI

	// migrationConfigurations maps source GVKs to target GVKs.
	migrationConfigurations map[metav1.GroupVersionKind]map[metav1.GroupVersionKind]brokerv1alpha1.MigrationConfiguration

	// stagingToProvider maps staging cluster names to provider cluster names.
	stagingToProvider map[string]string

	// localClient is used to read/write StagingWorkspace CRs in the local cluster.
	localClient client.Client

	// multiProvider is the multi-cluster provider that aggregates consumer and
	// staging provider clusters.
	multiProvider *multi.Provider
}

// New creates a new broker that acts on the given manager.
func New(opts Options) (*Broker, error) { //nolint:gocyclo
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("invalid options: %w", err)
	}

	b := new(Broker)
	b.opts = opts
	b.managers = make(map[string]mctrl.Manager)
	b.stagingToProvider = make(map[string]string)
	b.multiProvider = multi.New(multi.Options{})

	/////////////////////////////////////////////////////////////////////////////
	// AcceptAPI Controller

	b.apiAccepters = make(map[metav1.GroupVersionResource]map[string]map[string]brokerv1alpha1.AcceptAPI)
	acceptAPIScheme := runtime.NewScheme()
	if err := brokerv1alpha1.AddToScheme(acceptAPIScheme); err != nil {
		return nil, fmt.Errorf("unable to add broker v1alpha1 to acceptapi scheme: %w", err)
	}
	if err := kcpapisv1alpha1.AddToScheme(acceptAPIScheme); err != nil {
		return nil, fmt.Errorf("unable to add kcp apis to acceptapi scheme: %w", err)
	}
	if err := kcpapisv1alpha2.AddToScheme(acceptAPIScheme); err != nil {
		return nil, fmt.Errorf("unable to add kcp apis to acceptapi scheme: %w", err)
	}
	if err := clientgoscheme.AddToScheme(acceptAPIScheme); err != nil {
		return nil, fmt.Errorf("unable to add client-go scheme to acceptapi scheme: %w", err)
	}

	kcpAcceptAPI, err := kcpacceptapi.New(kcpacceptapi.Options{
		KcpConfig:     opts.KcpConfig,
		APIExportName: opts.AcceptAPIName,
		Scheme:        acceptAPIScheme,
		SetAcceptAPI: func(gvr metav1.GroupVersionResource, clusterName string, acceptAPI brokerv1alpha1.AcceptAPI) {
			clusterName = broker.ProviderPrefix + "#" + clusterName
			b.opts.Log.Info("SetAcceptAPI", "gvr", gvr, "cluster", clusterName, "acceptAPI", acceptAPI.Name)
			b.lock.Lock()
			defer b.lock.Unlock()
			if _, ok := b.apiAccepters[gvr]; !ok {
				b.apiAccepters[gvr] = make(map[string]map[string]brokerv1alpha1.AcceptAPI)
			}
			if _, ok := b.apiAccepters[gvr][clusterName]; !ok {
				b.apiAccepters[gvr][clusterName] = make(map[string]brokerv1alpha1.AcceptAPI)
			}
			b.apiAccepters[gvr][clusterName][acceptAPI.Name] = acceptAPI
		},
		DeleteAcceptAPI: func(gvr metav1.GroupVersionResource, clusterName string, acceptAPIName string) {
			clusterName = broker.ProviderPrefix + "#" + clusterName
			b.opts.Log.Info("DeleteAcceptAPI", "gvr", gvr, "cluster", clusterName, "acceptAPI", acceptAPIName)
			b.lock.Lock()
			defer b.lock.Unlock()
			clusterAcceptedAPIs, ok := b.apiAccepters[gvr][clusterName]
			if ok {
				delete(clusterAcceptedAPIs, acceptAPIName)
				if len(clusterAcceptedAPIs) == 0 {
					delete(b.apiAccepters[gvr], clusterName)
				}
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("unable to create acceptapi provider: %w", err)
	}

	kcpAcceptAPIMgr, err := mcmanager(opts.LocalConfig, acceptAPIScheme, kcpAcceptAPI.Input)
	if err != nil {
		return nil, fmt.Errorf("unable to create acceptapi manager: %w", err)
	}
	if err := mcbuilder.ControllerManagedBy(kcpAcceptAPIMgr).
		Named(b.opts.Name + "-kcp-acceptapi").
		For(&brokerv1alpha1.AcceptAPI{}).
		Complete(kcpAcceptAPI); err != nil {
		return nil, fmt.Errorf("failed to create acceptapi reconciler: %w", err)
	}
	b.managers["kcp-acceptapi"] = kcpAcceptAPIMgr

	/////////////////////////////////////////////////////////////////////////////
	// Migration Controllers

	b.migrationConfigurations = make(map[metav1.GroupVersionKind]map[metav1.GroupVersionKind]brokerv1alpha1.MigrationConfiguration)
	migrationScheme := runtime.NewScheme()
	if err := brokerv1alpha1.AddToScheme(migrationScheme); err != nil {
		return nil, fmt.Errorf("unable to add broker v1alpha1 to migration scheme: %w", err)
	}
	migrationClient, err := client.New(opts.MigrationCoordination, client.Options{
		Scheme: migrationScheme,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating migration coordination client: %w", err)
	}
	migrationCluster, err := cluster.New(b.opts.MigrationCoordination,
		func(o *cluster.Options) {
			o.Scheme = migrationClient.Scheme()
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error creating migration coordination cluster: %w", err)
	}
	migrationProvider := single.New("migration-coordination", migrationCluster)
	migrationMgr, err := mcmanager(opts.MigrationCoordination, migrationClient.Scheme(), migrationProvider)
	if err != nil {
		return nil, fmt.Errorf("unable to create migration manager: %w", err)
	}
	if err := migrationMgr.GetLocalManager().Add(manager.RunnableFunc(migrationCluster.Start)); err != nil {
		return nil, fmt.Errorf("error adding migration coordination cluster to migration manager: %w", err)
	}

	migrationConfigOptions := migration.ConfigurationOptions{
		GetCluster:           migrationMgr.GetCluster,
		ControllerNamePrefix: b.opts.Name,
		SetMigrationConfiguration: func(from metav1.GroupVersionKind, to metav1.GroupVersionKind, config brokerv1alpha1.MigrationConfiguration) {
			b.lock.Lock()
			defer b.lock.Unlock()
			if _, ok := b.migrationConfigurations[from]; !ok {
				b.migrationConfigurations[from] = make(map[metav1.GroupVersionKind]brokerv1alpha1.MigrationConfiguration)
			}
			b.migrationConfigurations[from][to] = config
		},
		DeleteMigrationConfiguration: func(from metav1.GroupVersionKind, to metav1.GroupVersionKind) {
			b.lock.Lock()
			defer b.lock.Unlock()
			delete(b.migrationConfigurations[from], to)
			if len(b.migrationConfigurations[from]) == 0 {
				delete(b.migrationConfigurations, from)
			}
		},
	}
	if err := migration.SetupConfigurationController(migrationMgr, migrationConfigOptions); err != nil {
		return nil, fmt.Errorf("failed to create migration reconciler: %w", err)
	}

	computeClient, err := client.New(b.opts.ComputeConfig, client.Options{
		Scheme: runtime.NewScheme(),
	})
	if err != nil {
		return nil, fmt.Errorf("error creating compute client: %w", err)
	}
	migrationOptions := migration.MigrationOptions{
		Compute:                computeClient,
		ControllerNamePrefix:   b.opts.Name,
		GetCoordinationCluster: migrationMgr.GetCluster,
		GetProviderCluster: func(ctx context.Context, clusterName multicluster.ClusterName) (cluster.Cluster, error) {
			if !strings.HasPrefix(string(clusterName), broker.ProviderPrefix) {
				return nil, fmt.Errorf("cluster %q is not a provider cluster: %w", clusterName, multicluster.ErrClusterNotFound)
			}
			return b.multiProvider.Get(ctx, clusterName)
		},
		GetMigrationConfiguration: func(fromGVK metav1.GroupVersionKind, toGVK metav1.GroupVersionKind) (brokerv1alpha1.MigrationConfiguration, bool) {
			b.lock.RLock()
			defer b.lock.RUnlock()
			toMap, ok := b.migrationConfigurations[fromGVK]
			if !ok {
				return brokerv1alpha1.MigrationConfiguration{}, false
			}
			v, ok := toMap[toGVK]
			return v, ok
		},
	}
	if err := migration.SetupController(migrationMgr, migrationOptions); err != nil {
		return nil, fmt.Errorf("failed to create migration reconciler: %w", err)
	}

	/////////////////////////////////////////////////////////////////////////////
	// Staging Workspace Reconciler + General Manager

	generalScheme := runtime.NewScheme()
	if err := brokerv1alpha1.AddToScheme(generalScheme); err != nil {
		return nil, fmt.Errorf("unable to add broker v1alpha1 to general scheme: %w", err)
	}

	stagingOutput := clusters.New()

	// Consumer clusters come from the broker API VW.
	brokerAPIScheme := runtime.NewScheme()
	if err := kcpapisv1alpha1.AddToScheme(brokerAPIScheme); err != nil {
		return nil, fmt.Errorf("unable to add kcp apis to broker api scheme: %w", err)
	}
	if err := kcpapisv1alpha2.AddToScheme(brokerAPIScheme); err != nil {
		return nil, fmt.Errorf("unable to add kcp apis to broker api scheme: %w", err)
	}
	brokerAPIs, err := apiexport.New(opts.KcpConfig, opts.BrokerAPIName, apiexport.Options{
		Scheme: brokerAPIScheme,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to create brokerapi provider: %w", err)
	}
	if err := b.multiProvider.AddProvider(broker.ConsumerPrefix, brokerAPIs); err != nil {
		return nil, fmt.Errorf("error adding brokerapi provider to multi provider: %w", err)
	}
	if err := b.multiProvider.AddProvider(broker.ProviderPrefix, stagingOutput); err != nil {
		return nil, fmt.Errorf("error adding staging output to multi provider: %w", err)
	}

	generalMgr, err := mcmanager(opts.LocalConfig, generalScheme, b.multiProvider)
	if err != nil {
		return nil, fmt.Errorf("unable to create general broker manager: %w", err)
	}
	b.managers["general"] = generalMgr

	// The staging workspace reconciler uses the local manager so it can CRUD
	// StagingWorkspace objects in the local cluster.
	b.localClient = generalMgr.GetLocalManager().GetClient()

	treeRootCfg, err := treeRootConfig(opts.KcpConfig, opts.WorkspaceTreeRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to build tree-root config: %w", err)
	}

	stagingReconciler, err := stagingworkspace.New(stagingworkspace.Options{
		TreeRootConfig: treeRootCfg,
		Scheme:         generalScheme,
		Output:         stagingOutput,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create staging workspace reconciler: %w", err)
	}
	if err := stagingReconciler.SetupWithManager(generalMgr.GetLocalManager()); err != nil {
		return nil, fmt.Errorf("failed to setup staging workspace reconciler: %w", err)
	}

	// Generic Sync Controllers

	genericOpts := genericreconciler.Options{
		CoordinationClient:   migrationClient,
		ControllerNamePrefix: b.opts.Name,
		GetProviderCluster: func(ctx context.Context, clusterName multicluster.ClusterName) (cluster.Cluster, error) {
			if !strings.HasPrefix(string(clusterName), broker.ProviderPrefix) {
				return nil, fmt.Errorf("cluster %q is not a provider cluster: %w", clusterName, multicluster.ErrClusterNotFound)
			}
			b.opts.Log.Info("GetProviderCluster", "clusterName", clusterName)
			return b.multiProvider.Get(ctx, clusterName)
		},
		GetConsumerCluster: func(ctx context.Context, clusterName multicluster.ClusterName) (cluster.Cluster, error) {
			if !strings.HasPrefix(string(clusterName), broker.ConsumerPrefix) {
				return nil, fmt.Errorf("cluster %q is not a consumer cluster: %w", clusterName, multicluster.ErrClusterNotFound)
			}
			return b.multiProvider.Get(ctx, clusterName)
		},
		GetProviders: func(gvr metav1.GroupVersionResource) map[string]map[string]brokerv1alpha1.AcceptAPI {
			b.lock.RLock()
			defer b.lock.RUnlock()
			ret := make(map[string]map[string]brokerv1alpha1.AcceptAPI, len(b.apiAccepters[gvr]))
			for providerClusterName, acceptors := range b.apiAccepters[gvr] {
				cloned := make(map[string]brokerv1alpha1.AcceptAPI, len(acceptors))
				maps.Copy(cloned, acceptors)
				ret[providerClusterName] = cloned
			}
			return ret
		},
		GetProviderAcceptedAPIs: func(providerOrStagingName string, gvr metav1.GroupVersionResource) ([]brokerv1alpha1.AcceptAPI, error) {
			b.lock.RLock()
			defer b.lock.RUnlock()
			if acceptAPIs, ok := b.apiAccepters[gvr][providerOrStagingName]; ok {
				return slices.Collect(maps.Values(acceptAPIs)), nil
			}
			// Translate staging cluster name → provider cluster name.
			if providerName, ok := b.stagingToProvider[providerOrStagingName]; ok {
				if acceptAPIs, ok := b.apiAccepters[gvr][providerName]; ok {
					return slices.Collect(maps.Values(acceptAPIs)), nil
				}
			}
			return nil, nil
		},
		GetMigrationConfiguration: func(fromGVK metav1.GroupVersionKind, toGVK metav1.GroupVersionKind) (brokerv1alpha1.MigrationConfiguration, bool) {
			b.lock.RLock()
			defer b.lock.RUnlock()
			toMap, ok := b.migrationConfigurations[fromGVK]
			if !ok {
				return brokerv1alpha1.MigrationConfiguration{}, false
			}
			v, ok := toMap[toGVK]
			return v, ok
		},

		// Staging workspace callbacks — these replace annotation-based routing.
		GetStagingCluster: func(ctx context.Context, consumerCluster string, gvr metav1.GroupVersionResource) (string, bool, error) {
			swList := &brokerv1alpha1.StagingWorkspaceList{}
			if err := b.localClient.List(ctx, swList, client.MatchingLabels{
				stagingConsumerClusterLabel: labelSafeClusterName(consumerCluster),
			}); err != nil {
				return "", false, err
			}
			b.lock.RLock()
			defer b.lock.RUnlock()
			for i := range swList.Items {
				sw := &swList.Items[i]
				// Label stores "."-separated form; convert back to "#"-separated for apiAccepters lookup.
				providerCluster := strings.ReplaceAll(sw.Labels[stagingProviderClusterLabel], ".", "#")
				acceptAPIs, ok := b.apiAccepters[gvr][providerCluster]
				if !ok {
					continue
				}
				// Match the staging workspace against the APIExport it was created for.
				swAPIExport := sw.Labels[stagingAPIExportLabel]
				for _, a := range acceptAPIs {
					if a.Annotations[kcpacceptapi.AnnotationAPIExportName] != swAPIExport {
						continue
					}
					// Label stores bare name (no "provider#" prefix); restore it for multi-provider routing.
					if rawName := sw.Labels[stagingworkspace.StagingClusterLabelKey]; rawName != "" {
						return broker.ProviderPrefix + "#" + rawName, true, nil
					}
				}
			}
			return "", false, nil
		},

		EnsureStagingCluster: func(ctx context.Context, consumerCluster, providerClusterName string, gvr metav1.GroupVersionResource) (string, error) {
			b.lock.RLock()
			acceptAPIs := maps.Clone(b.apiAccepters[gvr][providerClusterName])
			b.lock.RUnlock()

			if len(acceptAPIs) == 0 {
				return "", fmt.Errorf("no AcceptAPI found for provider %q and GVR %v", providerClusterName, gvr)
			}

			// Pick the first AcceptAPI to retrieve provider path and export name.
			var acceptAPI brokerv1alpha1.AcceptAPI
			for _, a := range acceptAPIs {
				acceptAPI = a
				break
			}
			providerPath := acceptAPI.Annotations[kcpcore.LogicalClusterPathAnnotationKey]
			if providerPath == "" {
				return "", fmt.Errorf("AcceptAPI for provider %q missing %s annotation", providerClusterName, kcpcore.LogicalClusterPathAnnotationKey)
			}
			apiExportName := acceptAPI.Annotations[kcpacceptapi.AnnotationAPIExportName]
			if apiExportName == "" {
				return "", fmt.Errorf("AcceptAPI for provider %q missing %s annotation", providerClusterName, kcpacceptapi.AnnotationAPIExportName)
			}

			swName := stagingWorkspaceName(consumerCluster, providerClusterName, apiExportName)
			clusterName := stagingClusterName(consumerCluster, providerClusterName, apiExportName)

			sw := &brokerv1alpha1.StagingWorkspace{}
			err := b.localClient.Get(ctx, types.NamespacedName{Name: swName}, sw)
			if apierrors.IsNotFound(err) {
				sw = &brokerv1alpha1.StagingWorkspace{
					ObjectMeta: metav1.ObjectMeta{
						Name: swName,
						Labels: map[string]string{
							stagingConsumerClusterLabel:             labelSafeClusterName(consumerCluster),
							stagingProviderClusterLabel:             labelSafeClusterName(providerClusterName),
							stagingAPIExportLabel:                   apiExportName,
							stagingworkspace.StagingClusterLabelKey: clusterNameToStagingLabel(clusterName),
						},
					},
					Spec: brokerv1alpha1.StagingWorkspaceSpec{
						ConsumerCluster:   consumerCluster,
						ProviderPath:      providerPath,
						APIExportName:     apiExportName,
						WorkspaceTreeRoot: b.opts.WorkspaceTreeRoot,
					},
				}
				if err := b.localClient.Create(ctx, sw); err != nil {
					return "", fmt.Errorf("failed to create StagingWorkspace %q: %w", swName, err)
				}
				return "", fmt.Errorf("staging workspace %q created, waiting for it to be ready: %w", swName, genericreconciler.ErrRequeueAfter)
			}
			if err != nil {
				return "", err
			}

			if sw.Status.Phase != brokerv1alpha1.StagingWorkspacePhaseReady {
				return "", fmt.Errorf("staging workspace %q not yet ready (phase: %s): %w", swName, sw.Status.Phase, genericreconciler.ErrRequeueAfter)
			}

			// Verify the cluster is registered and reachable.
			if _, err := b.multiProvider.Get(ctx, multicluster.ClusterName(clusterName)); err != nil {
				return "", fmt.Errorf("staging workspace %q ready but cluster not yet registered: %w", clusterName, genericreconciler.ErrRequeueAfter)
			}

			b.lock.Lock()
			b.stagingToProvider[clusterName] = providerClusterName
			b.lock.Unlock()

			return clusterName, nil
		},

		GetActiveMigration: func(ctx context.Context, consumerCluster string) (string, string, bool, error) {
			swList := &brokerv1alpha1.StagingWorkspaceList{}
			if err := b.localClient.List(ctx, swList, client.MatchingLabels{
				stagingConsumerClusterLabel: labelSafeClusterName(consumerCluster),
			}); err != nil {
				return "", "", false, err
			}
			for i := range swList.Items {
				sw := &swList.Items[i]
				if newCluster, ok := sw.Annotations[stagingNewClusterAnn]; ok && newCluster != "" {
					oldCluster := broker.ProviderPrefix + "#" + sw.Labels[stagingworkspace.StagingClusterLabelKey]
					return oldCluster, newCluster, true, nil
				}
			}
			return "", "", false, nil
		},

		SetNewStagingCluster: func(ctx context.Context, currentStagingCluster, newStagingCluster string) error {
			swList := &brokerv1alpha1.StagingWorkspaceList{}
			if err := b.localClient.List(ctx, swList, client.MatchingLabels{
				stagingworkspace.StagingClusterLabelKey: clusterNameToStagingLabel(currentStagingCluster),
			}); err != nil {
				return err
			}
			if len(swList.Items) == 0 {
				return fmt.Errorf("staging workspace for cluster %q not found", currentStagingCluster)
			}
			sw := &swList.Items[0]
			if sw.Annotations == nil {
				sw.Annotations = make(map[string]string)
			}
			sw.Annotations[stagingNewClusterAnn] = newStagingCluster
			return b.localClient.Update(ctx, sw)
		},

		ClearNewStagingCluster: func(ctx context.Context, oldStagingCluster string) error {
			swList := &brokerv1alpha1.StagingWorkspaceList{}
			if err := b.localClient.List(ctx, swList, client.MatchingLabels{
				stagingworkspace.StagingClusterLabelKey: clusterNameToStagingLabel(oldStagingCluster),
			}); err != nil {
				return err
			}
			if len(swList.Items) == 0 {
				return nil // already gone
			}
			sw := &swList.Items[0]
			if sw.Annotations == nil {
				return nil
			}
			delete(sw.Annotations, stagingNewClusterAnn)
			return b.localClient.Update(ctx, sw)
		},

		TrackResourceInStagingWorkspace: func(ctx context.Context, stagingCluster, namespace, name string) error {
			swList := &brokerv1alpha1.StagingWorkspaceList{}
			if err := b.localClient.List(ctx, swList, client.MatchingLabels{
				stagingworkspace.StagingClusterLabelKey: clusterNameToStagingLabel(stagingCluster),
			}); err != nil {
				return err
			}
			if len(swList.Items) == 0 {
				return fmt.Errorf("staging workspace for cluster %q not found", stagingCluster)
			}
			sw := &swList.Items[0]
			finalizer := stagingResourceFinalizerPrefix + genericreconciler.SanitizeClusterName(namespace+"/"+name)
			if !containsFinalizer(sw.Finalizers, finalizer) {
				sw.Finalizers = append(sw.Finalizers, finalizer)
				return b.localClient.Update(ctx, sw)
			}
			return nil
		},

		UntrackResourceFromStagingWorkspace: func(ctx context.Context, stagingCluster, namespace, name string) error {
			swList := &brokerv1alpha1.StagingWorkspaceList{}
			if err := b.localClient.List(ctx, swList, client.MatchingLabels{
				stagingworkspace.StagingClusterLabelKey: clusterNameToStagingLabel(stagingCluster),
			}); err != nil {
				return err
			}
			if len(swList.Items) == 0 {
				return nil // already gone
			}
			sw := &swList.Items[0]
			finalizer := stagingResourceFinalizerPrefix + genericreconciler.SanitizeClusterName(namespace+"/"+name)
			sw.Finalizers = removeFinalizer(sw.Finalizers, finalizer)
			if err := b.localClient.Update(ctx, sw); err != nil {
				return err
			}
			// If no more resource finalizers remain, delete the StagingWorkspace.
			for _, f := range sw.Finalizers {
				if strings.HasPrefix(f, stagingResourceFinalizerPrefix) {
					return nil
				}
			}
			if err := b.localClient.Delete(ctx, sw); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete empty staging workspace %q: %w", sw.Name, err)
			}
			return nil
		},
	}

	for _, gvk := range broker.ParseKinds(b.opts.WatchKinds) {
		if err := genericreconciler.SetupController(generalMgr, gvk, genericOpts); err != nil {
			return nil, fmt.Errorf("failed to create generic reconciler for %v: %w", gvk, err)
		}
	}

	return b, nil
}

// Start starts all managers of the broker.
func (b *Broker) Start(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, mgr := range b.managers {
		g.Go(func() error {
			return mgr.Start(ctx)
		})
	}
	return g.Wait()
}

// treeRootConfig derives a REST config pointing at the given kcp workspace
// path by replacing the /clusters/<path> segment in the kcp host URL.
func treeRootConfig(kcpConfig *rest.Config, workspaceTreeRoot string) (*rest.Config, error) {
	cfg := rest.CopyConfig(kcpConfig)
	u, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to parse KCP host URL %q: %w", cfg.Host, err)
	}
	idx := strings.Index(u.Path, "/clusters/")
	if idx < 0 {
		return nil, fmt.Errorf("KCP host URL %q does not contain /clusters/ path segment", cfg.Host)
	}
	u.Path = u.Path[:idx] + "/clusters/" + workspaceTreeRoot
	cfg.Host = u.String()
	return cfg, nil
}

// stagingWorkspaceName returns the deterministic kcp Workspace name for the
// given (consumerCluster, providerCluster, apiExportName) tuple.
func stagingWorkspaceName(consumerCluster, providerCluster, apiExportName string) string {
	consumer := strings.TrimPrefix(consumerCluster, broker.ConsumerPrefix+"#")
	provider := strings.TrimPrefix(providerCluster, broker.ProviderPrefix+"#")
	return "staging-" + genericreconciler.SanitizeClusterName(consumer) + "-" + genericreconciler.SanitizeClusterName(provider) + "-" + genericreconciler.SanitizeClusterName(apiExportName)
}

// stagingClusterName returns the multi-provider key used to register the
// staging cluster for the given (consumerCluster, providerCluster, apiExportName) tuple.
// Uses '.' instead of '#' so the name is also a valid Kubernetes label value.
func stagingClusterName(consumerCluster, providerCluster, apiExportName string) string {
	consumer := strings.TrimPrefix(consumerCluster, broker.ConsumerPrefix+"#")
	provider := strings.TrimPrefix(providerCluster, broker.ProviderPrefix+"#")
	return broker.ProviderPrefix + "#staging-" + genericreconciler.SanitizeClusterName(consumer) + "-" + genericreconciler.SanitizeClusterName(provider) + "-" + genericreconciler.SanitizeClusterName(apiExportName)
}

// labelSafeClusterName converts a cluster name to a Kubernetes-label-safe form
// by replacing '#' with '.'. KCP cluster IDs are lowercase hex (no '.'), so
// the conversion is reversible via strings.ReplaceAll(s, ".", "#").
func labelSafeClusterName(name string) string {
	return strings.ReplaceAll(name, "#", ".")
}

// clusterNameToStagingLabel strips the "provider#" prefix from a staging cluster name,
// yielding the bare "staging-<hash>-<hash>" value stored in StagingClusterLabelKey.
// This bare name is what stagingOutput registers, and the multi-provider prepends
// "provider#" automatically via wrappedAware.Engage.
func clusterNameToStagingLabel(clusterName string) string {
	return strings.TrimPrefix(clusterName, broker.ProviderPrefix+"#")
}

// containsFinalizer reports whether s contains the given finalizer string.
func containsFinalizer(s []string, finalizer string) bool {
	for _, f := range s {
		if f == finalizer {
			return true
		}
	}
	return false
}

// removeFinalizer returns a copy of s with all occurrences of finalizer removed.
func removeFinalizer(s []string, finalizer string) []string {
	out := s[:0:0]
	for _, f := range s {
		if f != finalizer {
			out = append(out, f)
		}
	}
	return out
}
