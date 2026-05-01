package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Represents a Lobby CRD.
// +kubebuilder:object:root=true
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
type Lobby struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec LobbySpec `json:"spec"`

	//+kubebuilder:validation:Optional
	//+optional
	Status LobbyStatus `json:"status"`
}

type LobbySpec struct {
	// Reference to the App being run
	//+kubebuilder:validation:Required
	AppReference corev1.LocalObjectReference `json:"appReference"`

	// Reference to the User who owns the lobby
	//+kubebuilder:validation:Required
	ProfileReference corev1.LocalObjectReference `json:"profileReference"`

	//+kubebuilder:validation:Required
	VideoSettings LobbyVideoSettings `json:"videoSettings"`

	//+kubebuilder:validation:Required
	AudioSettings LobbyAudioSettings `json:"audioSettings"`

	//+kubebuilder:validation:Required
	MultiUser bool `json:"multiUser"`

	//+kubebuilder:validation:Required
	StopWhenEveryoneLeaves bool `json:"stopWhenEveryoneLeaves"`

	// If provided, overrides the default runner command (which is "sleep infinity")
	//+kubebuilder:validation:Optional
	RunnerOverride string `json:"runnerOverride,omitempty"`
}

type LobbyVideoSettings struct {
	Width       int `json:"width"`
	Height      int `json:"height"`
	RefreshRate int `json:"refreshRate"`
}

type LobbyAudioSettings struct {
	ChannelCount int `json:"channelCount"`
}

type LobbyStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// The ID returned by the Wolf API when the lobby is created
	LobbyID string `json:"lobbyID,omitempty"`

	// WaylandSocketName is the name of the Wayland socket file generated for this Lobby
	WaylandSocketName string `json:"waylandSocketName,omitempty"`

	DeploymentName string `json:"deploymentName,omitempty"`
	ServiceName    string `json:"serviceName,omitempty"`

	// The ports allocated to the lobby on the shared gateway.
	Ports SessionPorts `json:"ports,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type LobbyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Lobby `json:"items"`
}
