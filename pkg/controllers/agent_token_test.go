package controllers

import (
	"context"
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
	first, err := sc.agentToken(ctx, sess)
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
	if again, _ := sc.agentToken(ctx, sess); again != first {
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
	if tok, _ := sc.agentToken(ctx, other); tok == first {
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
	old, _ := sc.agentToken(ctx, testSession("alex-steam-abcde", "old-uid"))

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
	current, _ := sc.agentToken(ctx, sess)
	if current == old {
		t.Error("stale token reused for new session")
	}

	// A stale cached object for the old Session must not take the Secret back.
	if err := sc.reconcileAgentToken(ctx, testSession("alex-steam-abcde", "old-uid")); err == nil {
		t.Error("expected error reconciling with a stale session object")
	}
	if tok, _ := sc.agentToken(ctx, sess); tok != current {
		t.Error("stale session object rotated the live token")
	}
}

func TestAgentTokenMissing(t *testing.T) {
	sc := tokenController()
	if _, err := sc.agentToken(context.Background(), testSession("s", "u")); err == nil {
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
