package controllers

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func testDeployment(uid types.UID) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "alex-steam", Namespace: "ns", UID: uid},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"direwolf/app": "steam", "direwolf/user": "alex"}},
		},
	}
}

func TestReconcileAgentTokenIsStableAndOwned(t *testing.T) {
	ctx := context.Background()
	sc := &SessionController{K8sClient: k8sfake.NewSimpleClientset()}
	dep := testDeployment("uid-1")

	if err := sc.reconcileAgentToken(ctx, dep); err != nil {
		t.Fatal(err)
	}
	first, err := sc.agentToken(ctx, dep)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Errorf("token length = %d, want 64 hex chars (32 random bytes)", len(first))
	}

	// Reconciling again must not rotate the token: the running agent read
	// it once at startup.
	if err = sc.reconcileAgentToken(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if again, _ := sc.agentToken(ctx, dep); again != first {
		t.Error("token rotated on second reconcile")
	}

	secret, err := sc.K8sClient.CoreV1().Secrets("ns").Get(ctx, "alex-steam-wolf-agent-token", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(secret); owner == nil || owner.Kind != "Deployment" || owner.UID != "uid-1" {
		t.Errorf("secret controller = %+v, want Deployment uid-1", owner)
	}

	// A different token per Deployment.
	other := testDeployment("uid-2")
	other.Name = "sam-steam"
	if err := sc.reconcileAgentToken(ctx, other); err != nil {
		t.Fatal(err)
	}
	if tok, _ := sc.agentToken(ctx, other); tok == first {
		t.Error("two deployments share a token")
	}
}

func TestReconcileAgentTokenReplacesStaleSecret(t *testing.T) {
	ctx := context.Background()
	// The live Deployment is the recreated one.
	sc := &SessionController{K8sClient: k8sfake.NewSimpleClientset(testDeployment("new-uid"))}

	if err := sc.reconcileAgentToken(ctx, testDeployment("old-uid")); err != nil {
		t.Fatal(err)
	}
	old, _ := sc.agentToken(ctx, testDeployment("old-uid"))

	// Same name, new Deployment (recreated before GC removed the Secret).
	dep := testDeployment("new-uid")
	if err := sc.reconcileAgentToken(ctx, dep); err != nil {
		t.Fatal(err)
	}
	secret, err := sc.K8sClient.CoreV1().Secrets("ns").Get(ctx, agentTokenSecretName(dep.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(secret); owner == nil || owner.UID != "new-uid" {
		t.Errorf("secret controller = %+v, want new-uid", owner)
	}
	current, _ := sc.agentToken(ctx, dep)
	if current == old {
		t.Error("stale token reused for new deployment")
	}

	// A stale cached object for the old Deployment must not take the Secret back.
	if err := sc.reconcileAgentToken(ctx, testDeployment("old-uid")); err == nil {
		t.Error("expected error reconciling with a stale deployment object")
	}
	if tok, _ := sc.agentToken(ctx, dep); tok != current {
		t.Error("stale deployment object rotated the live token")
	}
}

func TestAgentTokenMissing(t *testing.T) {
	sc := &SessionController{K8sClient: k8sfake.NewSimpleClientset()}
	if _, err := sc.agentToken(context.Background(), testDeployment("u")); err == nil {
		t.Fatal("expected error when token secret is missing")
	}
}

func TestAgentPodIP(t *testing.T) {
	pod := func(name, ip string, phase corev1.PodPhase, ready bool, lbls map[string]string) *corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: lbls},
			Status: corev1.PodStatus{
				Phase:      phase,
				PodIP:      ip,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			},
		}
	}
	match := map[string]string{"direwolf/app": "steam", "direwolf/user": "alex"}
	otherUser := map[string]string{"direwolf/app": "steam", "direwolf/user": "sam"}

	sc := &SessionController{K8sClient: k8sfake.NewSimpleClientset(
		pod("other-user", "10.0.0.9", corev1.PodRunning, true, otherUser),
		pod("not-ready", "10.0.0.2", corev1.PodRunning, false, match),
		pod("pending", "10.0.0.3", corev1.PodPending, false, match),
		pod("good", "fd00::5", corev1.PodRunning, true, match),
	)}
	ip, err := sc.agentPodIP(context.Background(), testDeployment("u"))
	if err != nil {
		t.Fatal(err)
	}
	if ip != "fd00::5" {
		t.Errorf("ip = %q, want fd00::5", ip)
	}

	empty := testDeployment("u")
	empty.Spec.Selector = &metav1.LabelSelector{}
	if _, err := sc.agentPodIP(context.Background(), empty); err == nil {
		t.Error("empty selector must be rejected, not match every pod")
	}

	none := &SessionController{K8sClient: k8sfake.NewSimpleClientset(
		pod("not-ready", "10.0.0.2", corev1.PodRunning, false, match),
	)}
	if _, err := none.agentPodIP(context.Background(), testDeployment("u")); err == nil {
		t.Error("expected error with no ready pod")
	}
}
