package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// wolf-agent proxies Wolf's unauthenticated API (which can run arbitrary
// containers), so it rejects any /api/v1/ request without the per-session
// bearer token below. Session pods use hostNetwork, so its port (from the
// session's port block) is reachable on the node IP by anything on the LAN.
const (
	wolfAgentTokenKey       = "token"
	wolfAgentTokenMountPath = "/etc/wolf-agent"
)

func agentTokenSecretName(sessionName string) string {
	return sessionName + "-wolf-agent-token"
}

// reconcileAgentToken ensures the wolf-agent token Secret exists for session
// and is controlled by it, so it is garbage-collected with it.
//
// A Secret left over from an earlier Session of the same name (GC not yet
// run) is replaced: otherwise the new pod could mount the old token just before
// GC deletes it, and the operator would read a fresh token that never matches.
func (c *SessionController) reconcileAgentToken(ctx context.Context, session *v1alpha1types.Session) error {
	secrets := c.K8sClient.CoreV1().Secrets(session.Namespace)
	name := agentTokenSecretName(session.Name)

	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		if owner := metav1.GetControllerOf(existing); owner != nil && owner.UID == session.UID {
			return nil
		}
		// Only replace the Secret on behalf of the live Session. A stale
		// cached object (e.g. during a leader-election overlap) would
		// otherwise delete the current token and re-own it to a dead UID.
		live, getErr := c.SessionClient.Get(ctx, session.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("failed to get session %s/%s: %w", session.Namespace, session.Name, getErr)
		}
		if live.UID != session.UID {
			return fmt.Errorf("session %s/%s changed (uid %s, have %s); not replacing token secret", session.Namespace, session.Name, live.UID, session.UID)
		}
		delErr := secrets.Delete(ctx, name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &existing.UID},
		})
		if delErr != nil && !errors.IsNotFound(delErr) {
			return fmt.Errorf("failed to delete stale wolf-agent token secret %s/%s: %w", session.Namespace, name, delErr)
		}
	case !errors.IsNotFound(err):
		return fmt.Errorf("failed to get wolf-agent token secret %s/%s: %w", session.Namespace, name, err)
	}

	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return fmt.Errorf("failed to generate wolf-agent token: %w", err)
	}

	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: session.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1types.GroupVersion.String(),
				Kind:       "Session",
				Name:       session.Name,
				UID:        session.UID,
				Controller: new(true),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{wolfAgentTokenKey: []byte(hex.EncodeToString(raw))},
	}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create wolf-agent token secret %s/%s: %w", session.Namespace, name, err)
	}
	return nil
}

// agentToken reads the bearer token wolf-agent in session's pod expects.
func (c *SessionController) agentToken(ctx context.Context, session *v1alpha1types.Session) (string, error) {
	name := agentTokenSecretName(session.Name)
	secret, err := c.K8sClient.CoreV1().Secrets(session.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get wolf-agent token secret %s/%s: %w", session.Namespace, name, err)
	}
	token := string(secret.Data[wolfAgentTokenKey])
	if token == "" {
		return "", fmt.Errorf("wolf-agent token secret %s/%s has no %q key", session.Namespace, name, wolfAgentTokenKey)
	}
	return token, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
