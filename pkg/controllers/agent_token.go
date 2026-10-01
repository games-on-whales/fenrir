package controllers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	stderrors "errors"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// wolf-agent proxies Wolf's unauthenticated API (which can run arbitrary
// containers), so it rejects any /api/v1/ request without the per-session
// bearer token below. Session pods use hostNetwork, so its port (from the
// session's port block) is reachable on the node IP by anything on the LAN.
//
// The same Secret holds a per-session serving cert the operator pins: any
// other host-networked process can bind the agent port (e.g. while wolf-agent
// restarts), and must not be handed the token.
const (
	wolfAgentTokenKey       = "token"
	wolfAgentTokenMountPath = "/etc/wolf-agent"
	// wolfAgentServerName is the cert's only SAN. The operator dials an IP,
	// so it verifies against this name and the pinned cert, not the address.
	wolfAgentServerName = "wolf-agent"
	// Outlives any session; the cert is pinned, not trusted for its dates.
	wolfAgentCertValidity = 365 * 24 * time.Hour
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

	certPEM, keyPEM, err := generateAgentCert()
	if err != nil {
		return err
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
		Data: map[string][]byte{
			wolfAgentTokenKey:       []byte(hex.EncodeToString(raw)),
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create wolf-agent token secret %s/%s: %w", session.Namespace, name, err)
	}
	return nil
}

// generateAgentCert makes wolf-agent's self-signed serving cert and key.
func generateAgentCert() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate wolf-agent key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate wolf-agent cert serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: wolfAgentServerName},
		DNSNames:              []string{wolfAgentServerName},
		NotBefore:             now.Add(-time.Hour), // tolerate node clock skew
		NotAfter:              now.Add(wolfAgentCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create wolf-agent cert: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal wolf-agent key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// errAgentCertMissing marks a token Secret from before wolf-agent's cert was
// pinned: its pod serves a cert the operator cannot verify.
var errAgentCertMissing = stderrors.New("wolf-agent token secret has no serving cert")

// agentCredentials reads the bearer token wolf-agent in session's pod expects
// and the TLS config that pins the cert it serves.
func (c *SessionController) agentCredentials(ctx context.Context, session *v1alpha1types.Session) (string, *tls.Config, error) {
	name := agentTokenSecretName(session.Name)
	secret, err := c.K8sClient.CoreV1().Secrets(session.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("failed to get wolf-agent token secret %s/%s: %w", session.Namespace, name, err)
	}
	token := string(secret.Data[wolfAgentTokenKey])
	if token == "" {
		return "", nil, fmt.Errorf("wolf-agent token secret %s/%s has no %q key", session.Namespace, name, wolfAgentTokenKey)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data[corev1.TLSCertKey]) {
		return "", nil, fmt.Errorf("%w: %s/%s", errAgentCertMissing, session.Namespace, name)
	}
	return token, &tls.Config{
		RootCAs:    roots,
		ServerName: wolfAgentServerName,
		MinVersion: tls.VersionTLS12,
	}, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
