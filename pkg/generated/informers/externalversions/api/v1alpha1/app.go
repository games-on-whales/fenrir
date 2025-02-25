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

// Code generated by informer-gen. DO NOT EDIT.

package v1alpha1

import (
	context "context"
	time "time"

	pkgapiv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	versioned "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned"
	internalinterfaces "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions/internalinterfaces"
	apiv1alpha1 "games-on-whales.github.io/direwolf/pkg/generated/listers/api/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
)

// AppInformer provides access to a shared informer and lister for
// Apps.
type AppInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() apiv1alpha1.AppLister
}

type appInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}

// NewAppInformer constructs a new informer for App type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewAppInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredAppInformer(client, namespace, resyncPeriod, indexers, nil)
}

// NewFilteredAppInformer constructs a new informer for App type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredAppInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.DirewolfV1alpha1().Apps(namespace).List(context.Background(), options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.DirewolfV1alpha1().Apps(namespace).Watch(context.Background(), options)
			},
			ListWithContextFunc: func(ctx context.Context, options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.DirewolfV1alpha1().Apps(namespace).List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.DirewolfV1alpha1().Apps(namespace).Watch(ctx, options)
			},
		},
		&pkgapiv1alpha1.App{},
		resyncPeriod,
		indexers,
	)
}

func (f *appInformer) defaultInformer(client versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredAppInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *appInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&pkgapiv1alpha1.App{}, f.defaultInformer)
}

func (f *appInformer) Lister() apiv1alpha1.AppLister {
	return apiv1alpha1.NewAppLister(f.Informer().GetIndexer())
}
