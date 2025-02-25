package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Represents a Pairing CRD.
// A pairing CRD is created when a client pairs with the server. It represents
// an association between a Moonlight client and a user.
//
// +kubebuilder:object:root=true
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Pairing struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec PairingSpec `json:"spec,omitempty"`
}

type PairingSpec struct {
	//+kubebuilder:validation:Required
	ClientCertPEM string `json:"clientCertPEM,omitempty"`

	//+kubebuilder:validation:Required
	UserReference UserReference `json:"userReference,omitempty"`
}

type UserReference struct {
	//+kubebuilder:validation:Required
	Name string `json:"name,omitempty"`
}

type GameReference struct {
	//+kubebuilder:validation:Required
	Name string `json:"name,omitempty"`
}

type PairingReference struct {
	//+kubebuilder:validation:Required
	Name string `json:"name,omitempty"`
}

type GatewayReference struct {
	//+kubebuilder:validation:Required
	Name string `json:"name,omitempty"`

	Namespace string `json:"namespace,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type PairingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Pairing `json:"items"`
}
