// Copyright The Platform Mesh Authors.
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import (
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	operatorv1alpha1 "github.com/platform-mesh/resource-broker/api/operator/v1alpha1"
)

// updateDeployment updates a deployment based on the values in the broker spec.
func updateDeployment(scheme *runtime.Scheme, broker *operatorv1alpha1.Broker, deployment *appsv1.Deployment) error {
	// labels
	deployment.Labels = maps.Clone(broker.Spec.Labels)
	deployment.Spec.Template.Labels = maps.Clone(broker.Spec.Labels)
	if deployment.Spec.Template.Labels == nil {
		deployment.Spec.Template.Labels = map[string]string{}
	}

	// set selector labels
	if deployment.Spec.Selector == nil {
		deployment.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app": broker.Name,
			},
		}
	}
	deployment.Spec.Template.Labels["app"] = broker.Name

	// annotations
	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	for k, v := range broker.Spec.Annotations {
		deployment.Annotations[k] = v
		deployment.Spec.Template.Annotations[k] = v
	}

	// pod / container
	replicas := int32(1)
	if broker.Spec.Replicas != nil {
		replicas = *broker.Spec.Replicas
	}
	deployment.Spec.Replicas = &replicas

	deployment.Spec.Template.Spec.Volumes = broker.Spec.Volumes
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{
		Name:            broker.Name,
		Image:           buildImageRef(broker.Spec.Image),
		ImagePullPolicy: broker.Spec.Image.PullPolicy,
		Args:            broker.Spec.ExtraArgs,
		Resources:       broker.Spec.Resources,
		Env:             broker.Spec.Env,
		VolumeMounts:    broker.Spec.VolumeMounts,
	}}

	saName := broker.Spec.ServiceAccountName
	if saName == "" {
		saName = "resource-broker"
	}
	deployment.Spec.Template.Spec.ServiceAccountName = saName

	deployment.Spec.Template.Spec.SecurityContext = broker.Spec.SecurityContext

	deployment.Spec.Template.Spec.ImagePullSecrets = broker.Spec.Image.ImagePullSecrets

	return controllerutil.SetControllerReference(broker, deployment, scheme)
}

func buildImageRef(imageSpec operatorv1alpha1.ImageSpec) string {
	repository := imageSpec.Repository
	if repository == "" {
		repository = "ghcr.io/platform-mesh/resource-broker"
	}

	tag := imageSpec.Tag
	if tag == "" {
		tag = "latest" // TODO pin version on release
	}

	return fmt.Sprintf("%s:%s", repository, tag)
}
