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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	mctrl "sigs.k8s.io/multicluster-runtime"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	brokerv1alpha1 "github.com/platform-mesh/resource-broker/api/broker/v1alpha1"
)

func (b *Broker) migrationConfigurationReconciler(name string, mgr mctrl.Manager) error {
	return mcbuilder.ControllerManagedBy(mgr).
		Named(name + "-migrationconfiguration").
		For(&brokerv1alpha1.MigrationConfiguration{}).
		Complete(mcreconcile.Func(b.migrationConfigurationReconcile))
}

// +kubebuilder:rbac:groups=broker.platform-mesh.io,resources=migrationconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=broker.platform-mesh.io,resources=migrationconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=broker.platform-mesh.io,resources=migrationconfigurations/finalizers,verbs=update

const migrationConfigurationFinalizer = "broker.platform-mesh.io/migrationconfiguration-finalizer"

func (b *Broker) migrationConfigurationReconcile(ctx context.Context, req mctrl.Request) (mctrl.Result, error) {
	log := ctrllog.FromContext(ctx).WithValues(
		"cluster", req.ClusterName,
		"name", req.Name,
		"namespace", req.Namespace,
	)
	log.Info("Reconciling MigrationConfiguration")

	cl, err := b.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return mctrl.Result{}, err
	}

	migrationConfiguration := &brokerv1alpha1.MigrationConfiguration{}
	if err := cl.GetClient().Get(ctx, req.NamespacedName, migrationConfiguration); err != nil {
		if apierrors.IsNotFound(err) {
			return mctrl.Result{}, nil
		}
		return mctrl.Result{}, err
	}

	if !migrationConfiguration.DeletionTimestamp.IsZero() {
		b.lock.Lock()
		delete(b.migrationConfigurations[migrationConfiguration.Spec.From], migrationConfiguration.Spec.To)
		if len(b.migrationConfigurations[migrationConfiguration.Spec.From]) == 0 {
			delete(b.migrationConfigurations, migrationConfiguration.Spec.From)
		}
		b.lock.Unlock()
		if ctrlutil.ContainsFinalizer(migrationConfiguration, migrationConfigurationFinalizer) {
			ctrlutil.RemoveFinalizer(migrationConfiguration, migrationConfigurationFinalizer)
			if err := cl.GetClient().Update(ctx, migrationConfiguration); err != nil {
				return mctrl.Result{}, err
			}
		}
		return mctrl.Result{}, nil
	}

	b.lock.Lock()
	if _, ok := b.migrationConfigurations[migrationConfiguration.Spec.From]; !ok {
		b.migrationConfigurations[migrationConfiguration.Spec.From] = make(map[metav1.GroupVersionKind]brokerv1alpha1.MigrationConfiguration)
	}
	b.migrationConfigurations[migrationConfiguration.Spec.From][migrationConfiguration.Spec.To] = *migrationConfiguration
	b.lock.Unlock()

	if !ctrlutil.ContainsFinalizer(migrationConfiguration, migrationConfigurationFinalizer) {
		ctrlutil.AddFinalizer(migrationConfiguration, migrationConfigurationFinalizer)
		if err := cl.GetClient().Update(ctx, migrationConfiguration); err != nil {
			return mctrl.Result{}, err
		}
	}

	return mctrl.Result{}, nil
}
