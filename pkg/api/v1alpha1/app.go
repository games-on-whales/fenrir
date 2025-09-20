package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type App struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec AppSpec `json:"spec"`
}

type AppSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// Name of the app to be presented to the user
	Title string `json:"title" xml:"AppTitle" toml:"title"`

	// Globally unique ID of the application. If there is a collision, the app
	// will be excluded from the list of available apps.
	ID int `json:"id" xml:"ID"`

	// +kubebuilder:validation:Required
	// Whether the app supports HDR
	IsHDRSupported bool `json:"isHDRSupported" xml:"IsHdrSupported"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=byte
	// PNG image of the app
	AppAssetWebP []byte `json:"appAssetWebP" xml:"-"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:X-kubernetes-preserve-unknown-fields:true
	Template *v1.PodTemplateSpec `json:"template" xml:"-"`

	// Unstructured wolf configuration for app to be merged with the default
	// configuration
	// +kubebuilder:validation:Optional
	// +kubebuilder:pruning:PreserveUnknownFields
	WolfConfig WolfConfig `json:"wolfConfig" xml:"-"`

	// +kubebuilder:validation:Optional
	// Configuration of the wolf-data volume mounted at the home dir of the app
	// Only used if ephemeral is set to false
	VolumeConfig *VolumeConfig `json:"volumeConfig,omitempty"`
}

type VolumeConfig struct {
	// +optional
	// Ephemeral, if true, it'll use EmptyDir volume
	// instead of creating a PVC
	// Defaults to false
	Ephemeral *bool `json:"ephemeral,omitempty"`
	// +optional
	// PVC configuration for the app
	// This works when ephemeral is set to false
	PVC *PVCConfig `json:"pvc,omitempty"`
}

type PVCConfig struct {
	// +optional
	// Name of the Storage class to provision the PVC from
	// Defaults to the default storage class
	StorageClassName string `json:"storageClassName,omitempty"`
	// +optional
	// Size of the requested storage of the PVC
	// Defaults to 10Gi if not specified
	Size string `json:"size,omitempty"`
}
type WolfConfig struct {
	StartAudioServer       *bool `json:"startAudioServer,omitempty" toml:"start_audio_server,omitempty"`
	StartVirtualCompositor *bool `json:"startVirtualCompositor,omitempty" toml:"start_video_compositor,omitempty"`

	Title string `json:"title,omitempty" toml:"title,omitempty"`
	ID    string `json:"id,omitempty" toml:"id,omitempty"`

	Audio *WolfStreamConfig `json:"audio,omitempty" toml:"audio,omitempty"`
	Video *WolfStreamConfig `json:"video,omitempty" toml:"video,omitempty"`

	Runner *WolfRunnerConfig `json:"runner,omitempty" toml:"runner,omitempty"`
}

type WolfStreamConfig struct {
	// +kubebuilder:validation:Optional
	Source string `json:"source,omitempty" toml:"source,omitempty"`

	// +kubebuilder:validation:Optional
	Sink string `json:"sink,omitempty" toml:"sink,omitempty"`
}

type WolfRunnerConfig struct {
	Type       string `json:"type,omitempty" toml:"type,omitempty"`
	RunCommand string `json:"runCommand,omitempty" toml:"run_cmd,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []App `json:"items"`
}
