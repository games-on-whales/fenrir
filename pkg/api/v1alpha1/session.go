package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Represents a Session CRD.
// +kubebuilder:object:root=true
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
type Session struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SessionSpec `json:"spec"`

	//+kubebuilder:validation:Optional
	//+optional
	Status SessionStatus `json:"status"`
}

// A session refers to a certain user playing a specific game with a specific
// client. This object is meant to live for the duration of the user's session.
//
// A session is created in response to a user's /launch request.
//
// It is meant to be deleted by the session controller once the agent has reported
// that it is ended.
//
// A user can have multiple sessions active at a time, and even multiple copies
// of the same "game" if the underlying persistentVolumeClass supports it multiple
// binding.
//
// A session is created when moonlight calls /launch to launch a game.
type SessionSpec struct {
	//+kubebuilder:validation:Required
	UserReference UserReference `json:"userReference"`

	//+kubebuilder:validation:Required
	GameReference GameReference `json:"gameReference"`

	//+kubebuilder:validation:Required
	PairingReference PairingReference `json:"pairingReference"`

	// The name of the Gateway used to access the moonlight server.
	// The gateway IP used for the stream session must be the same as the IP of
	// the moonlight server used to initiate the connection due to moonlight
	// protocol restrictions.
	GatewayReference GatewayReference `json:"gateway"`

	// Wolf-specific config for the session
	Config SessionInfo `json:"config"`
}

// Session State machine
// Pending -> Initializing -> WaitForPing -> Streaming -> Ended
type SessionStatus struct {
	// Represents the observations of a session's state.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// The ports allocated to the session on the shared gateway.
	Ports SessionPorts `json:"ports"`

	// The RTSP url to access the stream.
	WolfSessionID string `json:"wolfSessionID,omitempty"`
	StreamURL     string `json:"streamURL,omitempty"`

	DeploymentName string `json:"deploymentName,omitempty"`
	ServiceName    string `json:"serviceName,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Session `json:"items"`
}

type SessionInfo struct {
	//+kubebuilder:validation:Optional
	ClientIP string `json:"clientIP,omitempty"`

	//+kubebuilder:validation:Required
	AESKey string `json:"aesKey,omitempty"`

	//+kubebuilder:validation:Required
	AESIV string `json:"aesIV,omitempty"`

	//+kubebuilder:validation:Required
	VideoWidth int `json:"videoWidth,omitempty"`

	//+kubebuilder:validation:Required
	VideoHeight int `json:"videoHeight,omitempty"`

	//+kubebuilder:validation:Required
	VideoRefreshRate int `json:"videoRefreshRate,omitempty"`

	//+kubebuilder:validation:Required
	SurroundAudioFlags int `json:"surroundAudioFlags,omitempty"`
}

// Each session will have 4 ports allocated to it on the shared gateway.
// Using port forward allows us to avoid the need for a separate IP per session
// or a relay which could add latency to the stream.
type SessionPorts struct {
	RTSP     int32 `json:"rtsp,omitempty"`
	Control  int32 `json:"control,omitempty"`
	VideoRTP int32 `json:"videoRTP,omitempty"`
	AudioRTP int32 `json:"audioRTP,omitempty"`
}
