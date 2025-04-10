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

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	v1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	apiv1alpha1 "games-on-whales.github.io/direwolf/pkg/generated/applyconfiguration/api/v1alpha1"
	typedapiv1alpha1 "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	gentype "k8s.io/client-go/gentype"
)

// fakeSessions implements SessionInterface
type fakeSessions struct {
	*gentype.FakeClientWithListAndApply[*v1alpha1.Session, *v1alpha1.SessionList, *apiv1alpha1.SessionApplyConfiguration]
	Fake *FakeDirewolfV1alpha1
}

func newFakeSessions(fake *FakeDirewolfV1alpha1, namespace string) typedapiv1alpha1.SessionInterface {
	return &fakeSessions{
		gentype.NewFakeClientWithListAndApply[*v1alpha1.Session, *v1alpha1.SessionList, *apiv1alpha1.SessionApplyConfiguration](
			fake.Fake,
			namespace,
			v1alpha1.SchemeGroupVersion.WithResource("sessions"),
			v1alpha1.SchemeGroupVersion.WithKind("Session"),
			func() *v1alpha1.Session { return &v1alpha1.Session{} },
			func() *v1alpha1.SessionList { return &v1alpha1.SessionList{} },
			func(dst, src *v1alpha1.SessionList) { dst.ListMeta = src.ListMeta },
			func(list *v1alpha1.SessionList) []*v1alpha1.Session { return gentype.ToPointerSlice(list.Items) },
			func(list *v1alpha1.SessionList, items []*v1alpha1.Session) {
				list.Items = gentype.FromPointerSlice(items)
			},
		),
		fake,
	}
}
