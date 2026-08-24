package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// App is a Games on Whales container that uses wolf's sockets to stream to the user
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

	// DeviceClassName is the Kubernetes DRA DeviceClass used for wolf
	// resource claims when running this app.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="default-wolf"
	DeviceClassName string `json:"deviceClassName,omitempty"`

	// The pod manifest for the application, it defines the pod
	// +kubebuilder:validation:Optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Template corev1.PodTemplateSpec `json:"template" xml:"-"`

	// A template for a PersistentVolumeClaim to be created for the app
	// If provided, the operator will include them in the pvc
	// must also be defined in the pod template's spec.volumes field.
	// +kubebuilder:validation:Optional
	// +kubebuilder:pruning:PreserveUnknownFields
	VolumeClaimTemplates []corev1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"  xml:"-"`
}

// AppList
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []App `json:"items"`
}
