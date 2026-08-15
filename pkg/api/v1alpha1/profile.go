package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Profile contains the user's devices and allowed applications
// +genclient
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Profile struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec ProfileSpec `json:"spec,omitempty"`

	// +kubebuilder:subresource:status
	Status ProfileStatus `json:"status,omitempty"`
}

type ProfileSpec struct {
	// Apps defines the list of apps available to this profile.
	// +optional
	Apps []GameReference `json:"apps,omitempty"`

	// Pairings defines the list of paired moonlight clients that can access this profile.
	// Admins manually add Pairing IDs here to grant access.
	// +optional
	Pairings []PairingReference `json:"pairings,omitempty"`
}

type ProfileReference struct {
	//+kubebuilder:validation:Required
	Name string `json:"name,omitempty"`
}

type ProfileStatus struct {
}

// ProfileList is as the name implies a list of profiles
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Profile `json:"items"`
}
