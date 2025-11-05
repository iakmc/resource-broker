/*
Copyright 2025.
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
	"slices"
	"strings"

	"github.com/go-logr/logr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	mctrl "sigs.k8s.io/multicluster-runtime"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	brokerv1alpha1 "github.com/platform-mesh/resource-broker/api/broker/v1alpha1"
)

func (b *Broker) genericReconciler(name string, mgr mctrl.Manager, gvk schema.GroupVersionKind) error {
	gr := genericReconciler{
		log:        ctrllog.Log.WithName("generic-reconciler").WithValues("gvk", gvk),
		gvk:        gvk,
		getCluster: b.mgr.GetCluster,
		getAcceptedAPIs: func(providerName string, gvr metav1.GroupVersionResource) ([]*brokerv1alpha1.AcceptAPI, error) {
			b.lock.RLock()
			defer b.lock.RUnlock()
			acceptAPIs, ok := b.apiAccepters[gvr][providerName]
			if !ok {
				return nil, fmt.Errorf("provider %q does not accept %q", providerName, gvr)
			}
			return slices.Collect(maps.Values(acceptAPIs)), nil
		},
		getPossibleProvider: func(gvr metav1.GroupVersionResource, obj *unstructured.Unstructured) (string, error) {
			b.lock.RLock()
			defer b.lock.RUnlock()
			possibleProviders, ok := b.apiAccepters[gvr]

			if !ok || len(possibleProviders) == 0 {
				return "", fmt.Errorf("no clusters accept GVR %v", gvr)
			}

			for possibleProvider, acceptedAPIs := range possibleProviders {
				for _, acceptAPI := range acceptedAPIs {
					if acceptAPI.AppliesTo(gvr, obj) {
						return possibleProvider, nil
					}
				}
			}

			return "", fmt.Errorf("no accepting cluster found for GVR %v", gvr)
		},
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	return mcbuilder.ControllerManagedBy(mgr).
		Named(name + "-generic-" + gvk.String()).
		For(obj).
		Complete(&gr)
}

const (
	genericFinalizer   = "broker.platform-mesh.io/generic-finalizer"
	consumerClusterAnn = "broker.platform-mesh.io/consumer-cluster"
	providerClusterAnn = "broker.platform-mesh.io/provider-cluster"
)

type genericReconciler struct {
	log logr.Logger
	gvk schema.GroupVersionKind

	getCluster          func(context.Context, string) (cluster.Cluster, error)
	getPossibleProvider func(metav1.GroupVersionResource, *unstructured.Unstructured) (string, error)
	getAcceptedAPIs     func(string, metav1.GroupVersionResource) ([]*brokerv1alpha1.AcceptAPI, error)
}

// TODO this needs some more refactoring. not a fan of the
// genericReconcilerEvent. but it makes passing the data around and
// following the flow easier.

func (gr *genericReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (mctrl.Result, error) {
	ev := genericReconcilerEvent{
		log:                 ctrllog.FromContext(ctx).WithValues("clusterName", req.ClusterName),
		gvk:                 gr.gvk,
		req:                 req,
		getCluster:          gr.getCluster,
		getPossibleProvider: gr.getPossibleProvider,
		getAcceptedAPIs:     gr.getAcceptedAPIs,
	}

	ev.log.Info("Reconciling generic resource")

	cont, err := ev.determineClusters(ctx)
	if err != nil {
		return mctrl.Result{}, err
	}
	if !cont {
		// Not continuing
		return mctrl.Result{}, nil
	}

	if ev.consumerCluster == nil {
		ev.log.Info("Consumer cluster is not set, skipping")
		return mctrl.Result{}, ev.deleteObjs(ctx)
	}

	ev.log = ev.log.WithValues("consumer", ev.consumerName, "provider", ev.providerName)

	consumerObj, err := ev.getConsumerObj(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return mctrl.Result{}, ev.deleteObjs(ctx)
		}
		return mctrl.Result{}, fmt.Errorf("failed to get resource from consumer cluster %q: %w", ev.consumerName, err)
	}

	if !consumerObj.GetDeletionTimestamp().IsZero() {
		ev.log.Info("Resource in consumer cluster is being deleted, finalizing")
		return mctrl.Result{}, ev.deleteObjs(ctx)
	}

	if err := ev.decorateInConsumer(ctx); err != nil {
		return mctrl.Result{}, err
	}

	providerAccepts, err := ev.providerAcceptsObj(ctx)
	if err != nil {
		return mctrl.Result{}, err
	}

	if !providerAccepts {
		// TODO

		// choose new provider

		// create in new provider

		// delete from old provider when new ready

		// update annotations

		// OLD:

		if err := ev.deleteObj(ctx, ev.providerCluster); err != nil {
			return mctrl.Result{}, fmt.Errorf("failed to delete resource from provider cluster %q: %w", ev.providerName, err)
		}
		ev.providerName = ""
		return mctrl.Result{Requeue: true}, ev.decorateInConsumer(ctx)
	}

	if err := ev.decorateInProvider(ctx); err != nil {
		return mctrl.Result{}, err
	}

	if err := ev.syncResource(ctx); err != nil {
		return mctrl.Result{}, err
	}

	status, found, err := ev.getProviderStatus(ctx)
	if err != nil {
		return mctrl.Result{}, err
	}
	if found {
		switch status {
		case brokerv1alpha1.StatusEmpty, brokerv1alpha1.StatusUnknown, brokerv1alpha1.StatusProvisioning:
			ev.log.Info("Synced resource status is Empty, Unknown or Provisioning, not syncing related resources")
			return mctrl.Result{}, nil
		default:
			ev.log.Info("Synced resource status is not Unknown or Provisioning, syncing related resources", "status", status)
		}
	}

	if err := ev.syncRelatedResources(ctx); err != nil {
		return mctrl.Result{}, err
	}

	return mctrl.Result{}, nil
}

type genericReconcilerEvent struct {
	log logr.Logger
	gvk schema.GroupVersionKind
	req mcreconcile.Request

	getCluster          func(context.Context, string) (cluster.Cluster, error)
	getPossibleProvider func(metav1.GroupVersionResource, *unstructured.Unstructured) (string, error)
	getAcceptedAPIs     func(string, metav1.GroupVersionResource) ([]*brokerv1alpha1.AcceptAPI, error)

	consumerName    string
	consumerCluster cluster.Cluster
	providerName    string
	providerCluster cluster.Cluster

	gvr metav1.GroupVersionResource
}

func (ev *genericReconcilerEvent) getProviderName(obj *unstructured.Unstructured) (string, error) {
	providerName, ok := obj.GetAnnotations()[providerClusterAnn]
	if ok && providerName != "" {
		return providerName, nil
	}

	return ev.getPossibleProvider(ev.gvr, obj)
}

func (ev *genericReconcilerEvent) determineClusters(ctx context.Context) (bool, error) {
	switch {
	case strings.HasPrefix(ev.req.ClusterName, ConsumerPrefix):
		// Request comes from consumer cluster
		if err := ev.setConsumerCluster(ctx, ev.req.ClusterName); err != nil {
			return false, err
		}
	case strings.HasPrefix(ev.req.ClusterName, ProviderPrefix):
		// Request comes from provider cluster
		if err := ev.setProviderCluster(ctx, ev.req.ClusterName); err != nil {
			return false, err
		}
	default:
		ev.log.Info("Request cluster name does not have consumer or provider prefix, skipping")
		return false, nil
	}

	if ev.consumerName == "" {
		if err := ev.setConsumerClusterFromProvider(ctx); err != nil {
			return false, err
		}
	}

	// GVR must be set here because it is needed to find possible
	// providers based on accepted APIs
	var err error
	ev.gvr, err = ev.getGVR()
	if err != nil {
		ev.log.Error(err, "Failed to determine GVR for resource")
		return false, err
	}

	if ev.providerName == "" {
		if err := ev.setProviderClusterFromConsumer(ctx); err != nil {
			return false, err
		}
	}

	return true, nil
}

func (ev *genericReconcilerEvent) setConsumerCluster(ctx context.Context, name string) error {
	ev.consumerName = name
	cl, err := ev.getCluster(ctx, ev.consumerName)
	if err != nil {
		return fmt.Errorf("failed to get consumer cluster %q: %w", ev.consumerName, err)
	}
	ev.consumerCluster = cl
	return nil
}

func (ev *genericReconcilerEvent) setConsumerClusterFromProvider(ctx context.Context) error {
	providerObj, err := ev.getProviderObj(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the provider object is not found, the consumer cluster
			// cannot be set based on its annotation.
			return nil
		}
		return fmt.Errorf("failed to get resource from provider cluster %q: %w", ev.providerName, err)
	}

	consumerNameAnn, ok := providerObj.GetAnnotations()[consumerClusterAnn]
	if !ok || consumerNameAnn == "" {
		ev.log.Info("Resource in provider cluster missing consumer cluster annotation, skipping")
		return nil
	}

	return ev.setConsumerCluster(ctx, consumerNameAnn)
}

func (ev *genericReconcilerEvent) setProviderCluster(ctx context.Context, name string) error {
	ev.providerName = name
	cl, err := ev.getCluster(ctx, ev.providerName)
	if err != nil {
		return fmt.Errorf("failed to get provider cluster %q: %w", ev.providerName, err)
	}
	ev.providerCluster = cl
	return nil
}

func (ev *genericReconcilerEvent) setProviderClusterFromConsumer(ctx context.Context) error {
	consumerObj, err := ev.getConsumerObj(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the consumer object is not found, the provider cluster
			// cannot be set based on its annotation.
			return nil
		}
		return fmt.Errorf("failed to get resource from consumer cluster %q: %w", ev.consumerName, err)
	}

	possibleProviderName, err := ev.getProviderName(consumerObj)
	if err != nil {
		return fmt.Errorf("failed to determine provider cluster: %w", err)
	}
	if possibleProviderName == "" {
		return fmt.Errorf("no present or possible provider cluster found %q: %w", ev.consumerName, err)
	}

	return ev.setProviderCluster(ctx, possibleProviderName)
}

func (ev *genericReconcilerEvent) getGVR() (metav1.GroupVersionResource, error) {
	if ev.consumerCluster == nil {
		return metav1.GroupVersionResource{}, fmt.Errorf("consumer cluster is not set")
	}
	mapper := ev.consumerCluster.GetRESTMapper()
	mapping, err := mapper.RESTMapping(ev.gvk.GroupKind(), ev.gvk.Version)
	if err != nil {
		return metav1.GroupVersionResource{}, err
	}

	// mapper returns schema.GroupVersionResource, broker works
	// with metav1.GroupVersionResource
	return metav1.GroupVersionResource{
		Group:    mapping.Resource.Group,
		Version:  mapping.Resource.Version,
		Resource: mapping.Resource.Resource,
	}, nil
}

func (ev *genericReconcilerEvent) getConsumerObj(ctx context.Context) (*unstructured.Unstructured, error) {
	consumerObj := &unstructured.Unstructured{}
	consumerObj.SetGroupVersionKind(ev.gvk)
	if err := ev.consumerCluster.GetClient().Get(ctx, ev.req.NamespacedName, consumerObj); err != nil {
		return nil, err
	}
	return consumerObj, nil
}

func (ev *genericReconcilerEvent) getProviderObj(ctx context.Context) (*unstructured.Unstructured, error) {
	providerObj := &unstructured.Unstructured{}
	providerObj.SetGroupVersionKind(ev.gvk)
	if err := ev.providerCluster.GetClient().Get(ctx, ev.req.NamespacedName, providerObj); err != nil {
		return nil, err
	}
	return providerObj, nil
}

func (ev *genericReconcilerEvent) deleteObjs(ctx context.Context) error {
	if ev.providerCluster != nil {
		if err := ev.deleteObj(ctx, ev.providerCluster); err != nil {
			return fmt.Errorf("failed to delete resource from provider cluster %q: %w", ev.providerName, err)
		}
	}

	if ev.consumerCluster != nil {
		if err := ev.deleteObj(ctx, ev.consumerCluster); err != nil {
			return fmt.Errorf("failed to delete resource from consumer cluster %q: %w", ev.consumerName, err)
		}
	}

	return nil
}

func (ev *genericReconcilerEvent) deleteObj(ctx context.Context, cl cluster.Cluster) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ev.gvk)
	if err := cl.GetClient().Get(ctx, ev.req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if controllerutil.RemoveFinalizer(obj, genericFinalizer) {
		if err := cl.GetClient().Update(ctx, obj); err != nil {
			return fmt.Errorf("failed to remove finalizer during finalization: %w", err)
		}
	}

	if err := cl.GetClient().Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete resource during finalization: %w", err)
	}

	return nil
}

func (ev *genericReconcilerEvent) decorateInConsumer(ctx context.Context) error {
	consumerObj, err := ev.getConsumerObj(ctx)
	if err != nil {
		return fmt.Errorf("failed to get resource from consumer cluster %q: %w", ev.consumerName, err)
	}

	if controllerutil.AddFinalizer(consumerObj, genericFinalizer) {
		if err := ev.consumerCluster.GetClient().Update(ctx, consumerObj); err != nil {
			return fmt.Errorf("failed to add finalizer in consumer: %w", err)
		}
	}

	return setAnnotation(ctx, ev.consumerCluster, consumerObj, providerClusterAnn, ev.providerName)
}

func (ev *genericReconcilerEvent) decorateInProvider(ctx context.Context) error {
	providerObj, err := ev.getProviderObj(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Object does not exist in provider yet
			return nil
		}
		return fmt.Errorf("failed to get resource from provider cluster %q: %w", ev.providerName, err)
	}

	if controllerutil.AddFinalizer(providerObj, genericFinalizer) {
		if err := ev.providerCluster.GetClient().Update(ctx, providerObj); err != nil {
			return fmt.Errorf("failed to add finalizer in provider: %w", err)
		}
	}

	return setAnnotation(ctx, ev.providerCluster, providerObj, consumerClusterAnn, ev.consumerName)
}

func (ev *genericReconcilerEvent) providerAcceptsObj(ctx context.Context) (bool, error) {
	obj, err := ev.getConsumerObj(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get resource from consumer cluster %q: %w", ev.consumerName, err)
	}

	acceptAPIs, err := ev.getAcceptedAPIs(ev.providerName, ev.gvr)
	if err != nil {
		return false, err
	}
	for _, acceptAPI := range acceptAPIs {
		if acceptAPI.AppliesTo(ev.gvr, obj) {
			return true, nil
		}
	}
	return false, nil
}

func (ev *genericReconcilerEvent) syncResource(ctx context.Context) error {
	ev.log.Info("Syncing resource between consumer and provider cluster")
	// TODO send conditions back to consumer cluster
	// TODO there should be two informers triggering this - one
	// for consumer and one for provider
	if _, err := CopyResource(
		ctx,
		ev.gvk,
		ev.req.NamespacedName,
		ev.consumerCluster.GetClient(),
		ev.providerCluster.GetClient(),
	); err != nil {
		ev.log.Error(err, "Failed to copy resource to provider cluster")
		return err
	}

	return ev.decorateInProvider(ctx)
}

func (ev *genericReconcilerEvent) getProviderStatus(ctx context.Context) (brokerv1alpha1.Status, bool, error) {
	providerObj, err := ev.getProviderObj(ctx)
	if err != nil {
		return brokerv1alpha1.StatusUnknown, false, fmt.Errorf("failed to get resource from provider cluster %q: %w", ev.providerName, err)
	}

	statusI, found, err := unstructured.NestedString(providerObj.Object, "status", "status")
	return brokerv1alpha1.Status(statusI), found, err
}

func (ev *genericReconcilerEvent) syncRelatedResources(ctx context.Context) error {
	providerObj, err := ev.getProviderObj(ctx)
	if err != nil {
		return fmt.Errorf("failed to get resource from provider cluster %q: %w", ev.providerName, err)
	}

	// TODO handle resource drift when a related resource is removed in
	// the provider it needs to be removed in the consumer
	// maybe just a finalizer on the resources in the provider?
	relatedResourcesI, found, err := unstructured.NestedMap(providerObj.Object, "status", "relatedResources")
	if err != nil {
		ev.log.Error(err, "Failed to get related resources from synced resource status")
		return err
	}
	if !found {
		ev.log.Info("No related resources found in synced resource status")
		return nil
	}

	relatedResources := make(map[string]brokerv1alpha1.RelatedResource, len(relatedResourcesI))
	for key, rrI := range relatedResourcesI {
		rrMap, ok := rrI.(map[string]interface{})
		if !ok {
			return fmt.Errorf("failed to cast related resource from synced resource status")
		}

		var rr brokerv1alpha1.RelatedResource
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rrMap, &rr); err != nil {
			return fmt.Errorf("failed to convert related resource from synced resource status: %w", err)
		}

		relatedResources[key] = rr
	}

	ev.log.Info("Syncing related resources from provider to consumer", "count", len(relatedResources))

	consumerObj, err := ev.getConsumerObj(ctx)
	if err != nil {
		return fmt.Errorf("failed to get resource from consumer cluster %q: %w", ev.consumerName, err)
	}

	for key, relatedResource := range relatedResources {
		ev.syncRelatedResource(ctx, key, relatedResource, consumerObj)
	}

	return nil
}

func (ev *genericReconcilerEvent) syncRelatedResource(ctx context.Context, key string, relatedResource brokerv1alpha1.RelatedResource, consumerObj *unstructured.Unstructured) {
	log := ev.log.WithValues("relatedResourceKey", key)
	log.Info("Syncing related resource", "relatedResource", relatedResource)
	providerRRObj := &unstructured.Unstructured{}
	providerRRObj.SetGroupVersionKind(relatedResource.SchemaGVK())
	if err := ev.providerCluster.GetClient().Get(
		ctx,
		client.ObjectKey{
			Namespace: relatedResource.Namespace,
			Name:      relatedResource.Name,
		},
		providerRRObj,
	); err != nil {
		log.Error(err, "Failed to get related resource from provider cluster", "relatedResource", relatedResource)
		return
	}

	// TODO conditions
	_, err := CopyResource(
		ctx,
		relatedResource.SchemaGVK(),
		types.NamespacedName{
			Namespace: relatedResource.Namespace, // TODO namespace from consumer?
			Name:      relatedResource.Name,
		},
		ev.providerCluster.GetClient(),
		ev.consumerCluster.GetClient(),
	)
	if err != nil {
		log.Error(err, "Failed to copy related resource to consumer cluster")
		return
	}

	log.Info("Getting synced resource from consumer cluster")
	consumerRRObj := &unstructured.Unstructured{}
	consumerRRObj.SetGroupVersionKind(relatedResource.SchemaGVK())
	if err := ev.consumerCluster.GetClient().Get(
		ctx,
		client.ObjectKey{
			Namespace: relatedResource.Namespace,
			Name:      relatedResource.Name,
		},
		consumerRRObj,
	); err != nil {
		log.Error(err, "Failed to get synced related resource from consumer cluster")
		return
	}

	log.Info("Setting owner reference on related resource in consumer cluster")
	if err := controllerutil.SetOwnerReference(consumerObj, consumerRRObj, ev.consumerCluster.GetScheme()); err != nil {
		log.Error(err, "Failed to set owner reference on related resource in consumer cluster")
		return
	}

	if err := ev.consumerCluster.GetClient().Update(ctx, consumerRRObj); err != nil {
		log.Error(err, "Failed to set owner reference on related resource in consumer cluster")
	}
}
