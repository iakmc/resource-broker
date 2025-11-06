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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// MigrationConfigurationSpec defines the desired state of MigrationConfiguration.
type MigrationConfigurationSpec struct {
	// From indicates the source GVK of the resource to be migrated.
	// +required
	From metav1.GroupVersionKind `json:"from"`
	// To indicates the target GVK of the resource to be migrated.
	// +required
	To metav1.GroupVersionKind `json:"to"`

	// Stages defines the ordered list of migration stages to be
	// applied.
	// +optional
	Stages []MigrationStage `json:"stages,omitempty"`
}

// MigrationStage defines a single stage in a migration process.
type MigrationStage struct {
	// ID is a unique identifier for the migration stage.
	// +required
	ID string `json:"id"`

	// Templates is a list of raw Kubernetes resource templates of
	// resources to be deployed as part of this migration stage.
	// +optional
	Templates []runtime.RawExtension `json:"templates,omitempty"`
}

// +kubebuilder:object:root=true

// MigrationConfiguration is the Schema for the migrationconfigurations API.
type MigrationConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of MigrationConfiguration
	// +required
	Spec MigrationConfigurationSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// MigrationConfigurationList contains a list of MigrationConfiguration.
type MigrationConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MigrationConfiguration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MigrationConfiguration{}, &MigrationConfigurationList{})
}
