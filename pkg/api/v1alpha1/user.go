package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Represents a User CRD
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type User struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec UserSpec `json:"spec,omitempty"`

	// +kubebuilder:subresource:status
	Status UserStatus `json:"status,omitempty"`
}

type UserSpec struct {
	// Resources defines the maximum resource requests and limits that the app
	// container can have. If an app requests exceeds these values,
	// the app will fail to start.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Volumes defines the volumes that can be mounted by the session's pods.
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`
	// SidecarPolicies defines the resource requests and limits for the injected
	// sidecar containers. If a policy for a sidecar is not defined here, the
	// operator will use its own built-in default values.
	// +optional
	SidecarPolicies *SidecarPolicies `json:"sidecarPolicies,omitempty"`
}

// SidecarPolicy defines the policy for a single sidecar container.
type SidecarPolicy struct {
	// Resources for the sidecar
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// VolumeMounts specifies the volumes to mount into the sidecar.
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`
	// SecurityContext defines the security options the container should be run with.
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`
}

type SidecarPolicies struct {
	// Policy for the 'wolf' streaming sidecar
	// +optional
	Wolf *SidecarPolicy `json:"wolf,omitempty"`
	// Policy for the 'pulseaudio' audio control container
	// +optional
	PulseAudio *SidecarPolicy `json:"pulseaudio,omitempty"`
	// Policy for the 'wolf' session starter container
	// +optional
	WolfAgent *SidecarPolicy `json:"wolfAgent,omitempty"`
}

type UserStatus struct {
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []User `json:"items"`
}