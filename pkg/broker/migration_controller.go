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
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/google/cel-go/cel"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	mctrl "sigs.k8s.io/multicluster-runtime"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	brokerv1alpha1 "github.com/platform-mesh/resource-broker/api/broker/v1alpha1"
)

const (
	migrationStageLabel = "broker.platform-mesh.io/migration-stage"
	migrationIDLabel    = "broker.platform-mesh.io/migration-id"
)

func (b *Broker) migrationReconciler(name string, mgr mctrl.Manager) error {
	mr := migrationReconciler{
		log:           ctrllog.Log.WithName("migration-reconciler"),
		computeClient: b.compute,
		getCluster:    b.mgr.GetCluster,
		getMigrationConfiguration: func(fromGVK metav1.GroupVersionKind, toGVK metav1.GroupVersionKind) (brokerv1alpha1.MigrationConfiguration, bool) {
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

	return mcbuilder.ControllerManagedBy(mgr).
		Named(name + "-migration").
		For(&brokerv1alpha1.Migration{}).
		Complete(&mr)
}

type migrationReconciler struct {
	log logr.Logger

	computeClient             client.Client
	getCluster                func(context.Context, string) (cluster.Cluster, error)
	getMigrationConfiguration func(metav1.GroupVersionKind, metav1.GroupVersionKind) (brokerv1alpha1.MigrationConfiguration, bool)
}

// +kubebuilder:rbac:groups=broker.platform-mesh.io,resources=migrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=broker.platform-mesh.io,resources=migrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=broker.platform-mesh.io,resources=migrations/finalizers,verbs=update

//nolint:gocyclo
func (mr *migrationReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (mctrl.Result, error) {
	log := ctrllog.FromContext(ctx).WithValues(
		"clusterName", req.ClusterName,
		"name", req.Name,
		"namespace", req.Namespace,
	)
	log.Info("Reconciling migration")

	clusterName := req.ClusterName
	if !strings.HasPrefix(clusterName, CoordinationPrefix) {
		log.Info("Skipping migration reconciliation for non-coordination cluster")
		return ctrl.Result{}, nil
	}

	cl, err := mr.getCluster(ctx, clusterName)
	if err != nil {
		log.Error(err, "Failed to get cluster")
		return ctrl.Result{}, err
	}

	migration := &brokerv1alpha1.Migration{}
	if err := cl.GetClient().Get(ctx, req.NamespacedName, migration); err != nil {
		log.Error(err, "Failed to get migration")
		return ctrl.Result{}, err
	}
	log.Info("Migration found")

	switch migration.Status.State {
	case brokerv1alpha1.MigrationStateUnknown:
		log.Info("Setting migration state to Pending")
		migration.Status.ID = strings.ToLower(rand.Text())
		migration.Status.State = brokerv1alpha1.MigrationStatePending
		if err := cl.GetClient().Status().Update(ctx, migration); err != nil {
			log.Error(err, "Failed to update migration status")
			return ctrl.Result{}, err
		}
	case brokerv1alpha1.MigrationStateCutoverCompleted:
		log.Info("Migration already completed, skipping")
		return ctrl.Result{}, nil
	}

	log.Info("Copying related resources from source and target clusters")
	if err := mr.copyRelatedResources(ctx, migration.Spec.From, "from-"); err != nil {
		log.Error(err, "Failed to copy related resources from source cluster")
		return ctrl.Result{}, err
	}

	if err := mr.copyRelatedResources(ctx, migration.Spec.To, "to-"); err != nil {
		log.Error(err, "Failed to copy related resources from target cluster")
		return ctrl.Result{}, err
	}

	log.Info("Fetching migration configuration")
	migrationConfig, ok := mr.getMigrationConfiguration(migration.Spec.From.GVK, migration.Spec.To.GVK)
	if !ok {
		log.Info("No migration configuration found for migration", "fromGVK", migration.Spec.From.GVK, "toGVK", migration.Spec.To.GVK)
		return ctrl.Result{}, nil
	}

	if len(migrationConfig.Spec.Stages) == 0 {
		log.Info("Migration configuration has no stages defined, marking migration as completed")
		if err := mr.setState(ctx, cl, req.NamespacedName, brokerv1alpha1.MigrationStateCutoverCompleted); err != nil {
			log.Error(err, "Failed to set migration state to CutoverCompleted")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	stageIndex := 0
	for i, stage := range migrationConfig.Spec.Stages {
		if migration.Status.Stage == stage.Name {
			stageIndex = i
			break
		}
	}

	if stageIndex < 0 || stageIndex >= len(migrationConfig.Spec.Stages) {
		return ctrl.Result{}, fmt.Errorf("current migration stage %s not found in migration configuration", migration.Status.Stage)
	}

	log.Info("Processing migration stage", "stage", migrationConfig.Spec.Stages[stageIndex].Name)
	curStage := migrationConfig.Spec.Stages[stageIndex]
	stageResources, err := mr.deployStage(ctx, migration.Status.ID, curStage)
	if err != nil {
		log.Error(err, "Failed to deploy resources for migration stage", "stage", curStage.Name)
		return ctrl.Result{}, fmt.Errorf("failed to deploy resources for migration stage %s: %w", curStage.Name, err)
	}

	// Set next state if pending, otherwise there is not transition out
	// of the initial state.
	if migration.Status.State == brokerv1alpha1.MigrationStatePending {
		if err := mr.setState(ctx, cl, req.NamespacedName, brokerv1alpha1.MigrationStateInitialInProgress); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set migration state to %s for stage %s: %w", brokerv1alpha1.MigrationStateInitialInProgress, curStage.Name, err)
		}
	}

	log.Info("Deployed resources for migration stage", "stage", curStage.Name, "resources", stageResources)

	log.Info("Evaluating success conditions for migration stage", "stage", curStage.Name)
	celEnv, celArgs, err := mr.prepCEL(stageResources)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to prepare CEL environment for migration stage %s: %w", curStage.Name, err)
	}

	for _, condExpr := range curStage.SuccessConditions {
		ast, issues := celEnv.Compile(condExpr)
		if issues.Err() != nil {
			return ctrl.Result{}, fmt.Errorf("failed to compile CEL expression %s for migration stage %s: %w", condExpr, curStage.Name, issues.Err())
		}

		prg, err := celEnv.Program(ast)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create CEL program for expression %s in migration stage %s: %w", condExpr, curStage.Name, err)
		}

		val, _, err := prg.ContextEval(ctx, celArgs)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to evaluate CEL expression %s for migration stage %s: %w", condExpr, curStage.Name, err)
		}

		if val.Type().TypeName() != "bool" {
			return ctrl.Result{}, fmt.Errorf("CEL expression %s for migration stage %s did not evaluate to a boolean", condExpr, curStage.Name)
		}

		if !val.Value().(bool) {
			log.Info("Success condition not yet met for migration stage", "stage", curStage.Name, "condition", condExpr)
			return ctrl.Result{}, nil
		}

		log.Info("Success condition met for migration stage", "stage", curStage.Name, "condition", condExpr)
	}

	log.Info("All success conditions met for migration stage", "stage", curStage.Name)
	for _, res := range stageResources {
		if err := mr.computeClient.Delete(ctx, res); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to clean up resource %s/%s from migration stage %s: %w", res.GetNamespace(), res.GetName(), curStage.Name, err)
		}
	}

	if stageIndex+1 >= len(migrationConfig.Spec.Stages) {
		log.Info("All migration stages completed, marking migration as completed")
		migration.Status.State = brokerv1alpha1.MigrationStateCutoverCompleted
		return ctrl.Result{}, cl.GetClient().Status().Update(ctx, migration)
	}

	if curStage.Progress {
		log.Info("Progressing migration state to next phase")
		switch migration.Status.State {
		case brokerv1alpha1.MigrationStateInitialInProgress:
			migration.Status.State = brokerv1alpha1.MigrationStateInitialCompleted
		case brokerv1alpha1.MigrationStateCutoverInProgress:
			migration.Status.State = brokerv1alpha1.MigrationStateCutoverCompleted
		default:
			return ctrl.Result{}, fmt.Errorf("cannot progress migration state from %s", migration.Status.State)
		}
		return ctrl.Result{}, cl.GetClient().Status().Update(ctx, migration)
	}

	nextStage := migrationConfig.Spec.Stages[stageIndex+1]
	migration.Status.Stage = nextStage.Name
	if err := cl.GetClient().Status().Update(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	// requeue to process the next stage
	return ctrl.Result{Requeue: true}, nil
}

func (mr *migrationReconciler) copyRelatedResources(ctx context.Context, from brokerv1alpha1.MigrationRef, prefix string) error {
	cl, err := mr.getCluster(ctx, from.ClusterName)
	if err != nil {
		return fmt.Errorf("failed to get cluster %s: %w", from.ClusterName, err)
	}

	relatedResources, err := collectRelatedResources(
		ctx,
		cl.GetClient(),
		schema.GroupVersionKind{
			Group:   from.GVK.Group,
			Version: from.GVK.Version,
			Kind:    from.GVK.Kind,
		},
		types.NamespacedName{
			Name:      from.Name,
			Namespace: from.Namespace,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to collect related resources: %w", err)
	}

	var errs error
	for _, rr := range relatedResources {
		errs = errors.Join(errs, mr.copyRelatedResource(ctx, cl, prefix, rr))
	}

	return errs
}

func (mr *migrationReconciler) copyRelatedResource(ctx context.Context, source cluster.Cluster, prefix string, relatedResource brokerv1alpha1.RelatedResource) error {
	sourceObj := &unstructured.Unstructured{}
	sourceObj.SetGroupVersionKind(
		schema.GroupVersionKind{
			Group:   relatedResource.GVK.Group,
			Version: relatedResource.GVK.Version,
			Kind:    relatedResource.GVK.Kind,
		},
	)
	sourceNamespacedName := types.NamespacedName{
		Name:      relatedResource.Name,
		Namespace: relatedResource.Namespace,
	}
	if err := source.GetClient().Get(ctx, sourceNamespacedName, sourceObj); err != nil {
		return fmt.Errorf("failed to get related resource %s/%s: %w", relatedResource.Namespace, relatedResource.Name, err)
	}

	targetObj := StripClusterMetadata(sourceObj)
	targetObj.SetName(prefix + relatedResource.Name)
	if relatedResource.Namespace != "" {
		targetObj.SetNamespace(relatedResource.Namespace) // TODO handle namespacing. all resources should probably be in the same namespace
	}

	existingObj := &unstructured.Unstructured{}
	existingObj.SetGroupVersionKind(targetObj.GroupVersionKind())
	err := mr.computeClient.Get(
		ctx,
		types.NamespacedName{
			Name:      targetObj.GetName(),
			Namespace: targetObj.GetNamespace(),
		},
		existingObj,
	)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return mr.computeClient.Create(ctx, targetObj)
		}
		return fmt.Errorf("failed to get existing related resource %s/%s: %w", targetObj.GetNamespace(), targetObj.GetName(), err)
	}

	targetObj.SetResourceVersion(existingObj.GetResourceVersion())
	return mr.computeClient.Update(ctx, targetObj)
}

func (mr *migrationReconciler) deployStage(ctx context.Context, migrationID string, stage brokerv1alpha1.MigrationStage) (map[string]*unstructured.Unstructured, error) {
	var ret = make(map[string]*unstructured.Unstructured)
	var errs error

	for name, rawExt := range stage.Templates {
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(rawExt.Raw); err != nil {
			errs = errors.Join(errs, fmt.Errorf("failed to unmarshal template %s: %w", name, err))
			continue
		}
		obj.SetName(fmt.Sprintf("%s-%s", migrationID, name))
		obj.SetNamespace("default") // TODO: make namespace configurable
		labels := obj.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[migrationStageLabel] = stage.Name
		labels[migrationIDLabel] = migrationID
		obj.SetLabels(labels)

		existingObj := &unstructured.Unstructured{}
		existingObj.SetGroupVersionKind(obj.GroupVersionKind())
		err := mr.computeClient.Get(
			ctx,
			types.NamespacedName{
				Name:      obj.GetName(),
				Namespace: obj.GetNamespace(),
			},
			existingObj,
		)
		if err == nil {
			ret[name] = existingObj
			continue
		}

		if !apierrors.IsNotFound(err) {
			errs = errors.Join(errs, fmt.Errorf("failed to get existing resource from template %s: %w", name, err))
			continue
		}

		if err := mr.computeClient.Create(ctx, obj); err != nil {
			errs = errors.Join(errs, fmt.Errorf("failed to create resource from template %s: %w", name, err))
			continue
		}
		ret[name] = obj
	}

	return ret, errs
}

func (mr *migrationReconciler) setState(ctx context.Context, cl cluster.Cluster, migrationName types.NamespacedName, state brokerv1alpha1.MigrationState) error {
	migration := &brokerv1alpha1.Migration{}
	if err := cl.GetClient().Get(ctx, migrationName, migration); err != nil {
		return fmt.Errorf("failed to get migration %s/%s: %w", migrationName.Namespace, migrationName.Name, err)
	}

	migration.Status.State = state
	if err := cl.GetClient().Status().Update(ctx, migration); err != nil {
		return fmt.Errorf("failed to update migration %s/%s status: %w", migrationName.Namespace, migrationName.Name, err)
	}
	return nil
}

func (mr *migrationReconciler) prepCEL(stageResources map[string]*unstructured.Unstructured) (*cel.Env, map[string]any, error) {
	envArgs := []cel.EnvOption{}
	evalArgs := make(map[string]any)
	for name, obj := range stageResources {
		envArgs = append(envArgs, cel.Variable(name, cel.DynType))
		evalArgs[name] = obj
	}

	env, err := cel.NewEnv(envArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	return env, evalArgs, nil
}
