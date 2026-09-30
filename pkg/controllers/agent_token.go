package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// wolf-agent proxies Wolf's unauthenticated API (which can run arbitrary
// containers), so it is never published on the session Service and it
// rejects any /api/v1/ request without the per-Deployment bearer token below.
// The token matters even though the port is unpublished: the pod IP is
// reachable cluster-wide, and on the node IP once pods use hostNetwork.
const (
	wolfAgentPort           = 8443
	wolfAgentTokenKey       = "token"
	wolfAgentTokenMountPath = "/etc/wolf-agent"
)

func agentTokenSecretName(deploymentName string) string {
	return deploymentName + "-wolf-agent-token"
}

// reconcileAgentToken ensures the wolf-agent token Secret exists for
// deployment and is controlled by it, so it is garbage-collected with it.
//
// A Secret left over from an earlier Deployment of the same name (GC not yet
// run) is replaced: otherwise a new pod could mount the old token just before
// GC deletes it, and the operator would read a fresh token that never matches.
func (c *SessionController) reconcileAgentToken(ctx context.Context, deployment *appsv1.Deployment) error {
	secrets := c.K8sClient.CoreV1().Secrets(deployment.Namespace)
	name := agentTokenSecretName(deployment.Name)

	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		if owner := metav1.GetControllerOf(existing); owner != nil && owner.UID == deployment.UID {
			return nil
		}
		// Only replace the Secret on behalf of the live Deployment. A stale
		// cached object (e.g. during a leader-election overlap) would
		// otherwise delete the current token and re-own it to a dead UID.
		live, getErr := c.K8sClient.AppsV1().Deployments(deployment.Namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("failed to get deployment %s/%s: %w", deployment.Namespace, deployment.Name, getErr)
		}
		if live.UID != deployment.UID {
			return fmt.Errorf("deployment %s/%s changed (uid %s, have %s); not replacing token secret", deployment.Namespace, deployment.Name, live.UID, deployment.UID)
		}
		delErr := secrets.Delete(ctx, name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &existing.UID},
		})
		if delErr != nil && !errors.IsNotFound(delErr) {
			return fmt.Errorf("failed to delete stale wolf-agent token secret %s/%s: %w", deployment.Namespace, name, delErr)
		}
	case !errors.IsNotFound(err):
		return fmt.Errorf("failed to get wolf-agent token secret %s/%s: %w", deployment.Namespace, name, err)
	}

	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return fmt.Errorf("failed to generate wolf-agent token: %w", err)
	}

	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: deployment.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deployment.Name,
				UID:        deployment.UID,
				Controller: new(true),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{wolfAgentTokenKey: []byte(hex.EncodeToString(raw))},
	}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create wolf-agent token secret %s/%s: %w", deployment.Namespace, name, err)
	}
	return nil
}

// agentToken reads the bearer token wolf-agent in deployment's pod expects.
func (c *SessionController) agentToken(ctx context.Context, deployment *appsv1.Deployment) (string, error) {
	name := agentTokenSecretName(deployment.Name)
	secret, err := c.K8sClient.CoreV1().Secrets(deployment.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get wolf-agent token secret %s/%s: %w", deployment.Namespace, name, err)
	}
	token := string(secret.Data[wolfAgentTokenKey])
	if token == "" {
		return "", fmt.Errorf("wolf-agent token secret %s/%s has no %q key", deployment.Namespace, name, wolfAgentTokenKey)
	}
	return token, nil
}

// agentPodIP returns the IP of a ready, non-terminating pod of deployment,
// which is how the operator reaches wolf-agent now that it is off the Service.
func (c *SessionController) agentPodIP(ctx context.Context, deployment *appsv1.Deployment) (string, error) {
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return "", fmt.Errorf("invalid selector on deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}
	if selector.Empty() {
		// An empty selector matches every pod in the namespace.
		return "", fmt.Errorf("deployment %s/%s has an empty selector", deployment.Namespace, deployment.Name)
	}
	pods, err := c.K8sClient.CoreV1().Pods(deployment.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods for deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" && podReady(pod) {
			return pod.Status.PodIP, nil
		}
	}
	return "", fmt.Errorf("no ready pod for deployment %s/%s (selector %s)", deployment.Namespace, deployment.Name, selector)
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
