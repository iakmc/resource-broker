package v1alpha1

import (
	brokerv1alpha1 "github.com/platform-mesh/resource-broker/api/broker/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CertificateSpec defines the desired state of Certificate
type CertificateSpec struct {
	// fqdn is the fully qualified domain name for the certificate.
	// +required
	FQDN *string `json:"fqdn,omitempty"`
}

// CertificateStatus defines the observed state of Certificate.
type CertificateStatus struct {
	// conditions represent the current state of the Certificate resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// RelatedResources lists resources related to this VM.
	// +optional
	RelatedResources brokerv1alpha1.RelatedResources `json:"relatedResources,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Certificate is the Schema for the certificates API
type Certificate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Certificate
	// +required
	Spec CertificateSpec `json:"spec"`

	// status defines the observed state of Certificate
	// +optional
	Status CertificateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CertificateList contains a list of Certificate
type CertificateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Certificate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Certificate{}, &CertificateList{})
}
