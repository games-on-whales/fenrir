/*
Copyright 2022 The Kubernetes Authors.

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

package generic

import (
	"fmt"
	"net/http"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
)

var _ Lister[runtime.Object] = lister[runtime.Object]{}

type namespacedLister[T runtime.Object] struct {
	indexer   cache.Indexer
	namespace string
}

func (w namespacedLister[T]) List(selector labels.Selector) ([]T, error) {
	if selector == nil {
		selector = labels.Everything()
	}

	var ret []T
	err := cache.ListAllByNamespace(w.indexer, w.namespace, selector, func(m any) {
		v, ok := m.(T)
		if !ok {
			return
		}
		ret = append(ret, v)
	})
	if err != nil {
		return nil, fmt.Errorf("cache couldn't list namespaces: %w", err)
	}
	return ret, nil
}

func (w namespacedLister[T]) Get(name string) (T, error) {
	var result T
	key := w.namespace + "/" + name
	obj, exists, err := w.indexer.GetByKey(key)
	if err != nil {
		return result, fmt.Errorf("object couldn't be acquired by key(%s): %w", key, err)
	}
	if !exists {
		return result, &kerrors.StatusError{ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusNotFound,
			Reason:  metav1.StatusReasonNotFound,
			Message: name + " not found",
		}}
	}
	v, ok := obj.(T)
	if !ok {
		return result, fmt.Errorf("unexpected type %T in indexer, expected %T", obj, result)
	}
	return v, nil
}

type lister[T runtime.Object] struct {
	indexer cache.Indexer
}

func (w lister[T]) List(selector labels.Selector) ([]T, error) {
	var ret []T
	err := cache.ListAll(w.indexer, selector, func(m any) {
		v, ok := m.(T)
		if !ok {
			return
		}
		ret = append(ret, v)
	})
	if err != nil {
		return nil, fmt.Errorf("cache couldn't be accessed: %w", err)
	}
	return ret, nil
}

func (w lister[T]) Get(name string) (T, error) {
	var result T
	obj, exists, err := w.indexer.GetByKey(name)
	if err != nil {
		return result, fmt.Errorf("object couldn't be acquired by key(%s): %w", name, err)
	}
	if !exists {
		return result, &kerrors.StatusError{ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusNotFound,
			Reason:  metav1.StatusReasonNotFound,
			Message: name + " not found",
		}}
	}
	v, ok := obj.(T)
	if !ok {
		return result, fmt.Errorf("unexpected type %T in indexer, expected %T", obj, result)
	}
	return v, nil
}

func (w lister[T]) Namespaced(namespace string) NamespacedLister[T] {
	return namespacedLister[T]{namespace: namespace, indexer: w.indexer}
}

func NewLister[T runtime.Object](indexer cache.Indexer) Lister[T] {
	return lister[T]{indexer: indexer}
}
