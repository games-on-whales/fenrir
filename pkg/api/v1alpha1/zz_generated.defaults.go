//go:build !ignore_autogenerated
// +build !ignore_autogenerated

/*
Copyright The Kubernetes Authors.

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

// Code generated by defaulter-gen. DO NOT EDIT.

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// RegisterDefaults adds defaulters functions to the given scheme.
// Public to allow building arbitrary schemes.
// All generated defaulters are covering - they call all nested defaulters.
func RegisterDefaults(scheme *runtime.Scheme) error {
	scheme.AddTypeDefaultingFunc(&App{}, func(obj interface{}) { SetObjectDefaults_App(obj.(*App)) })
	scheme.AddTypeDefaultingFunc(&AppList{}, func(obj interface{}) { SetObjectDefaults_AppList(obj.(*AppList)) })
	return nil
}

func SetObjectDefaults_App(in *App) {
	if in.Spec.Template != nil {
		for i := range in.Spec.Template.Spec.Volumes {
			a := &in.Spec.Template.Spec.Volumes[i]
			if a.VolumeSource.ISCSI != nil {
				if a.VolumeSource.ISCSI.ISCSIInterface == "" {
					a.VolumeSource.ISCSI.ISCSIInterface = "default"
				}
			}
			if a.VolumeSource.RBD != nil {
				if a.VolumeSource.RBD.RBDPool == "" {
					a.VolumeSource.RBD.RBDPool = "rbd"
				}
				if a.VolumeSource.RBD.RadosUser == "" {
					a.VolumeSource.RBD.RadosUser = "admin"
				}
				if a.VolumeSource.RBD.Keyring == "" {
					a.VolumeSource.RBD.Keyring = "/etc/ceph/keyring"
				}
			}
			if a.VolumeSource.AzureDisk != nil {
				if a.VolumeSource.AzureDisk.CachingMode == nil {
					ptrVar1 := v1.AzureDataDiskCachingMode(v1.AzureDataDiskCachingReadWrite)
					a.VolumeSource.AzureDisk.CachingMode = &ptrVar1
				}
				if a.VolumeSource.AzureDisk.FSType == nil {
					var ptrVar1 string = "ext4"
					a.VolumeSource.AzureDisk.FSType = &ptrVar1
				}
				if a.VolumeSource.AzureDisk.ReadOnly == nil {
					var ptrVar1 bool = false
					a.VolumeSource.AzureDisk.ReadOnly = &ptrVar1
				}
				if a.VolumeSource.AzureDisk.Kind == nil {
					ptrVar1 := v1.AzureDataDiskKind(v1.AzureSharedBlobDisk)
					a.VolumeSource.AzureDisk.Kind = &ptrVar1
				}
			}
			if a.VolumeSource.ScaleIO != nil {
				if a.VolumeSource.ScaleIO.StorageMode == "" {
					a.VolumeSource.ScaleIO.StorageMode = "ThinProvisioned"
				}
				if a.VolumeSource.ScaleIO.FSType == "" {
					a.VolumeSource.ScaleIO.FSType = "xfs"
				}
			}
		}
		for i := range in.Spec.Template.Spec.InitContainers {
			a := &in.Spec.Template.Spec.InitContainers[i]
			for j := range a.Ports {
				b := &a.Ports[j]
				if b.Protocol == "" {
					b.Protocol = "TCP"
				}
			}
			if a.LivenessProbe != nil {
				if a.LivenessProbe.ProbeHandler.GRPC != nil {
					if a.LivenessProbe.ProbeHandler.GRPC.Service == nil {
						var ptrVar1 string = ""
						a.LivenessProbe.ProbeHandler.GRPC.Service = &ptrVar1
					}
				}
			}
			if a.ReadinessProbe != nil {
				if a.ReadinessProbe.ProbeHandler.GRPC != nil {
					if a.ReadinessProbe.ProbeHandler.GRPC.Service == nil {
						var ptrVar1 string = ""
						a.ReadinessProbe.ProbeHandler.GRPC.Service = &ptrVar1
					}
				}
			}
			if a.StartupProbe != nil {
				if a.StartupProbe.ProbeHandler.GRPC != nil {
					if a.StartupProbe.ProbeHandler.GRPC.Service == nil {
						var ptrVar1 string = ""
						a.StartupProbe.ProbeHandler.GRPC.Service = &ptrVar1
					}
				}
			}
		}
		for i := range in.Spec.Template.Spec.Containers {
			a := &in.Spec.Template.Spec.Containers[i]
			for j := range a.Ports {
				b := &a.Ports[j]
				if b.Protocol == "" {
					b.Protocol = "TCP"
				}
			}
			if a.LivenessProbe != nil {
				if a.LivenessProbe.ProbeHandler.GRPC != nil {
					if a.LivenessProbe.ProbeHandler.GRPC.Service == nil {
						var ptrVar1 string = ""
						a.LivenessProbe.ProbeHandler.GRPC.Service = &ptrVar1
					}
				}
			}
			if a.ReadinessProbe != nil {
				if a.ReadinessProbe.ProbeHandler.GRPC != nil {
					if a.ReadinessProbe.ProbeHandler.GRPC.Service == nil {
						var ptrVar1 string = ""
						a.ReadinessProbe.ProbeHandler.GRPC.Service = &ptrVar1
					}
				}
			}
			if a.StartupProbe != nil {
				if a.StartupProbe.ProbeHandler.GRPC != nil {
					if a.StartupProbe.ProbeHandler.GRPC.Service == nil {
						var ptrVar1 string = ""
						a.StartupProbe.ProbeHandler.GRPC.Service = &ptrVar1
					}
				}
			}
		}
		for i := range in.Spec.Template.Spec.EphemeralContainers {
			a := &in.Spec.Template.Spec.EphemeralContainers[i]
			for j := range a.EphemeralContainerCommon.Ports {
				b := &a.EphemeralContainerCommon.Ports[j]
				if b.Protocol == "" {
					b.Protocol = "TCP"
				}
			}
			if a.EphemeralContainerCommon.LivenessProbe != nil {
				if a.EphemeralContainerCommon.LivenessProbe.ProbeHandler.GRPC != nil {
					if a.EphemeralContainerCommon.LivenessProbe.ProbeHandler.GRPC.Service == nil {
						var ptrVar1 string = ""
						a.EphemeralContainerCommon.LivenessProbe.ProbeHandler.GRPC.Service = &ptrVar1
					}
				}
			}
			if a.EphemeralContainerCommon.ReadinessProbe != nil {
				if a.EphemeralContainerCommon.ReadinessProbe.ProbeHandler.GRPC != nil {
					if a.EphemeralContainerCommon.ReadinessProbe.ProbeHandler.GRPC.Service == nil {
						var ptrVar1 string = ""
						a.EphemeralContainerCommon.ReadinessProbe.ProbeHandler.GRPC.Service = &ptrVar1
					}
				}
			}
			if a.EphemeralContainerCommon.StartupProbe != nil {
				if a.EphemeralContainerCommon.StartupProbe.ProbeHandler.GRPC != nil {
					if a.EphemeralContainerCommon.StartupProbe.ProbeHandler.GRPC.Service == nil {
						var ptrVar1 string = ""
						a.EphemeralContainerCommon.StartupProbe.ProbeHandler.GRPC.Service = &ptrVar1
					}
				}
			}
		}
	}
}

func SetObjectDefaults_AppList(in *AppList) {
	for i := range in.Items {
		a := &in.Items[i]
		SetObjectDefaults_App(a)
	}
}
