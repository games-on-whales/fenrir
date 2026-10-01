package controllers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
)

func testSession(name string, uid types.UID) *v1alpha1types.Session {
	return &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: uid}}
}

func tokenController(live ...*v1alpha1types.Session) *SessionController {
	dw := generatedclient.NewSimpleClientset()
	for _, s := range live {
		_ = dw.Tracker().Add(s)
	}
	return &SessionController{K8sClient: k8sfake.NewSimpleClientset(), SessionClient: dw.DirewolfV1alpha1().Sessions("ns")}
}

func TestReconcileAgentTokenIsStableAndOwned(t *testing.T) {
	ctx := context.Background()
	sc := tokenController()
	sess := testSession("alex-steam-abcde", "uid-1")

	if err := sc.reconcileAgentToken(ctx, sess); err != nil {
		t.Fatal(err)
	}
	first, err := testAgentToken(ctx, sc, sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Errorf("token length = %d, want 64 hex chars (32 random bytes)", len(first))
	}

	// Reconciling again must not rotate the token: the running agent read
	// it once at startup.
	if err = sc.reconcileAgentToken(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if again, _ := testAgentToken(ctx, sc, sess); again != first {
		t.Error("token rotated on second reconcile")
	}

	secret, err := sc.K8sClient.CoreV1().Secrets("ns").Get(ctx, "alex-steam-abcde-wolf-agent-token", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(secret); owner == nil || owner.Kind != "Session" || owner.UID != "uid-1" {
		t.Errorf("secret controller = %+v, want Session uid-1", owner)
	}

	// A different token per Session.
	other := testSession("sam-steam-fghij", "uid-2")
	if err := sc.reconcileAgentToken(ctx, other); err != nil {
		t.Fatal(err)
	}
	if tok, _ := testAgentToken(ctx, sc, other); tok == first {
		t.Error("two sessions share a token")
	}
}

func TestReconcileAgentTokenReplacesStaleSecret(t *testing.T) {
	ctx := context.Background()
	// The live Session is the recreated one.
	sc := tokenController(testSession("alex-steam-abcde", "new-uid"))

	if err := sc.reconcileAgentToken(ctx, testSession("alex-steam-abcde", "old-uid")); err != nil {
		t.Fatal(err)
	}
	old, _ := testAgentToken(ctx, sc, testSession("alex-steam-abcde", "old-uid"))

	// Same name, new Session (recreated before GC removed the Secret).
	sess := testSession("alex-steam-abcde", "new-uid")
	if err := sc.reconcileAgentToken(ctx, sess); err != nil {
		t.Fatal(err)
	}
	secret, err := sc.K8sClient.CoreV1().Secrets("ns").Get(ctx, agentTokenSecretName(sess.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(secret); owner == nil || owner.UID != "new-uid" {
		t.Errorf("secret controller = %+v, want new-uid", owner)
	}
	current, _ := testAgentToken(ctx, sc, sess)
	if current == old {
		t.Error("stale token reused for new session")
	}

	// A stale cached object for the old Session must not take the Secret back.
	if err := sc.reconcileAgentToken(ctx, testSession("alex-steam-abcde", "old-uid")); err == nil {
		t.Error("expected error reconciling with a stale session object")
	}
	if tok, _ := testAgentToken(ctx, sc, sess); tok != current {
		t.Error("stale session object rotated the live token")
	}
}

func TestAgentTokenMissing(t *testing.T) {
	sc := tokenController()
	if _, err := testAgentToken(context.Background(), sc, testSession("s", "u")); err == nil {
		t.Fatal("expected error when token secret is missing")
	}
}

func TestPodReady(t *testing.T) {
	for _, tc := range []struct {
		conds []corev1.PodCondition
		want  bool
	}{
		{nil, false},
		{[]corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}, false},
		{[]corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}, {Type: corev1.PodReady, Status: corev1.ConditionTrue}}, true},
	} {
		if got := podReady(&corev1.Pod{Status: corev1.PodStatus{Conditions: tc.conds}}); got != tc.want {
			t.Errorf("podReady(%+v) = %v, want %v", tc.conds, got, tc.want)
		}
	}
}

func testAgentToken(ctx context.Context, sc *SessionController, sess *v1alpha1types.Session) (string, error) {
	token, _, err := sc.agentCredentials(ctx, sess)
	return token, err
}

// The Secret carries a serving cert for wolf-agent that the operator's TLS
// config accepts, one per Session.
func TestReconcileAgentTokenIssuesPinnedCert(t *testing.T) {
	ctx := context.Background()
	sc := tokenController()
	alex, sam := testSession("alex-steam-abcde", "uid-1"), testSession("sam-steam-fghij", "uid-2")
	for _, s := range []*v1alpha1types.Session{alex, sam} {
		if err := sc.reconcileAgentToken(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	serve := func(sess *v1alpha1types.Session) tls.Certificate {
		secret, err := sc.K8sClient.CoreV1().Secrets("ns").Get(ctx, agentTokenSecretName(sess.Name), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
		if err != nil {
			t.Fatalf("secret cert/key are not a key pair: %v", err)
		}
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return pair
	}
	_, alexTLS, err := sc.agentCredentials(ctx, alex)
	if err != nil {
		t.Fatal(err)
	}
	accepts := func(pair tls.Certificate) bool {
		_, err := pair.Leaf.Verify(x509.VerifyOptions{Roots: alexTLS.RootCAs, DNSName: alexTLS.ServerName})
		return err == nil
	}
	if !accepts(serve(alex)) {
		t.Error("alex's TLS config rejects alex's cert")
	}
	if accepts(serve(sam)) {
		t.Error("alex's TLS config accepts sam's cert")
	}
}
