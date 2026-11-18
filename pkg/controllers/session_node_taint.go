package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// sessionNodeTaintValue is the value of the session taint. The taint is
// ref-counted across all lobbies on a node, so it does not name a lobby.
const sessionNodeTaintValue = "true"

// sessionNodeTaintToleration returns the toleration that lobby pods (and any
// other component that must keep running on a node with an active session)
// need to survive the session taint.
func sessionNodeTaintToleration(key string) corev1.Toleration {
	return corev1.Toleration{
		Key:      key,
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoExecute,
	}
}

// addSessionNodeTaintToleration adds the session taint toleration to the pod
// spec unless an equivalent toleration is already present.
func addSessionNodeTaintToleration(spec *corev1.PodSpec, key string) {
	if key == "" || podSpecToleratesSessionTaint(spec, key) {
		return
	}
	spec.Tolerations = append(spec.Tolerations, sessionNodeTaintToleration(key))
}

func sessionNodeTaint(key string) corev1.Taint {
	return corev1.Taint{
		Key:    key,
		Value:  sessionNodeTaintValue,
		Effect: corev1.TaintEffectNoExecute,
	}
}

func podSpecToleratesSessionTaint(spec *corev1.PodSpec, key string) bool {
	taint := sessionNodeTaint(key)
	for i := range spec.Tolerations {
		if spec.Tolerations[i].ToleratesTaint(klog.Background(), &taint, false) {
			return true
		}
	}
	return false
}

func isSessionNodeTaint(t corev1.Taint, key string) bool {
	return t.Key == key && t.Effect == corev1.TaintEffectNoExecute
}

func nodeHasSessionTaint(node *corev1.Node, key string) bool {
	return slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool {
		return isSessionNodeTaint(t, key)
	})
}

// reconcileSessionNodeTaints recomputes, from cluster state, which nodes host
// an active lobby pod and makes the session taint match: nodes with at least
// one active lobby are tainted, nodes with none are untainted. It is a no-op
// when SessionNodeTaintKey is empty.
//
// State is derived entirely from the Lobby informer and the API server, so the
// result is the same regardless of operator restarts or event ordering.
func (c *LobbyController) reconcileSessionNodeTaints(ctx context.Context) error {
	if c.SessionNodeTaintKey == "" {
		return nil
	}
	lobbies, err := c.LobbyInformer.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list lobbies: %w", err)
	}
	return c.syncSessionNodeTaints(ctx, lobbies)
}

func (c *LobbyController) syncSessionNodeTaints(ctx context.Context, lobbies []*direwolfv1alpha1.Lobby) error {
	key := c.SessionNodeTaintKey
	if key == "" {
		return nil
	}

	// Serialize sweeps so two workers can't race an add against a remove.
	c.sessionTaintMu.Lock()
	defer c.sessionTaintMu.Unlock()

	// active: nodes with a scheduled, non-terminating pod for a non-deleting lobby.
	// blocked: nodes where such a pod does not tolerate the taint (e.g. its
	// StatefulSet was created before the option was enabled); tainting those
	// would evict the session itself.
	active := map[string]bool{}
	blocked := map[string]bool{}
	for _, lobby := range lobbies {
		if lobby.DeletionTimestamp != nil {
			continue
		}
		podName := c.statefulSetName(lobby) + "-0"
		pod, err := c.K8sClient.CoreV1().Pods(lobby.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if kerrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("get pod %s/%s: %w", lobby.Namespace, podName, err)
		}
		if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil ||
			pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		active[pod.Spec.NodeName] = true
		if !podSpecToleratesSessionTaint(&pod.Spec, key) {
			klog.Warningf("Lobby pod %s/%s on node %s does not tolerate %s; not tainting node",
				pod.Namespace, pod.Name, pod.Spec.NodeName, key)
			blocked[pod.Spec.NodeName] = true
		}
	}

	nodes, err := c.K8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	var errs []error
	for i := range nodes.Items {
		node := &nodes.Items[i]
		has := nodeHasSessionTaint(node, key)
		switch {
		case active[node.Name] && !blocked[node.Name] && !has:
			klog.Infof("Adding session taint %s to node %s", key, node.Name)
			errs = append(errs, c.setSessionNodeTaint(ctx, node.Name, true))
		case !active[node.Name] && has:
			klog.Infof("Removing session taint %s from node %s", key, node.Name)
			errs = append(errs, c.setSessionNodeTaint(ctx, node.Name, false))
		}
	}
	return errors.Join(errs...)
}

// setSessionNodeTaint adds or removes the session taint on a node. The patch
// carries the node's resourceVersion so a concurrent change to the taint list
// results in a conflict and a retry against fresh state instead of clobbering it.
func (c *LobbyController) setSessionNodeTaint(ctx context.Context, nodeName string, present bool) error {
	key := c.SessionNodeTaintKey
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := c.K8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get node: %w", err)
		}
		if nodeHasSessionTaint(node, key) == present {
			return nil
		}
		taints := slices.DeleteFunc(slices.Clone(node.Spec.Taints), func(t corev1.Taint) bool {
			return isSessionNodeTaint(t, key)
		})
		if present {
			taints = append(taints, sessionNodeTaint(key))
		}

		// Taints is replaced wholesale by a merge patch; including the
		// resourceVersion turns a concurrent node update into a 409 Conflict.
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": node.ResourceVersion},
			"spec":     map[string]any{"taints": taints},
		})
		if err != nil {
			return fmt.Errorf("marshal taint patch: %w", err)
		}
		if _, err := c.K8sClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{
			FieldManager: "direwolf-lobby-controller-taint",
		}); err != nil {
			return fmt.Errorf("patch node: %w", err)
		}
		return nil
	})
	if kerrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("update session taint on node %s: %w", nodeName, err)
	}
	return nil
}
