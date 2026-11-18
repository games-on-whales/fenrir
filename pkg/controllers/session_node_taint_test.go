package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

const (
	testNamespace = "direwolf"
	testTaintKey  = "games-on-whales.io/session-active"
)

func testLobby(name, profile, app string) *direwolfv1alpha1.Lobby {
	return &direwolfv1alpha1.Lobby{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: direwolfv1alpha1.LobbySpec{
			ProfileReference: corev1.LocalObjectReference{Name: profile},
			AppReference:     corev1.LocalObjectReference{Name: app},
		},
	}
}

// testLobbyPod returns the StatefulSet pod the lobby controller would create
// for the lobby, scheduled to nodeName and tolerating the session taint.
func testLobbyPod(lobby *direwolfv1alpha1.Lobby, nodeName string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lobby.Spec.ProfileReference.Name + "-" + lobby.Spec.AppReference.Name + "-0",
			Namespace: testNamespace,
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	addSessionNodeTaintToleration(&pod.Spec, testTaintKey)
	return pod
}

func testNode(name string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"},
		Spec:       corev1.NodeSpec{Taints: taints},
	}
}

func newTaintTestController(objs ...runtime.Object) (*LobbyController, *k8sfake.Clientset) {
	client := k8sfake.NewClientset(objs...)
	return &LobbyController{
		K8sClient:                client,
		SessionControllerOptions: SessionControllerOptions{SessionNodeTaintKey: testTaintKey},
	}, client
}

func nodeTaints(t *testing.T, client *k8sfake.Clientset, name string) []corev1.Taint {
	t.Helper()
	node, err := client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return node.Spec.Taints
}

func hasTestTaint(t *testing.T, client *k8sfake.Clientset, name string) bool {
	t.Helper()
	return nodeHasSessionTaint(&corev1.Node{Spec: corev1.NodeSpec{Taints: nodeTaints(t, client, name)}}, testTaintKey)
}

func countNodePatches(client *k8sfake.Clientset) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "nodes" {
			n++
		}
	}
	return n
}

func TestSessionNodeTaintAddedForScheduledLobby(t *testing.T) {
	lobby := testLobby("l1", "alice", "steam")
	c, client := newTaintTestController(
		testNode("gpu-1"), testNode("gpu-2"),
		testLobbyPod(lobby, "gpu-1"),
	)

	require.NoError(t, c.syncSessionNodeTaints(context.Background(), []*direwolfv1alpha1.Lobby{lobby}))

	require.Equal(t, []corev1.Taint{{Key: testTaintKey, Value: "true", Effect: corev1.TaintEffectNoExecute}},
		nodeTaints(t, client, "gpu-1"))
	require.False(t, hasTestTaint(t, client, "gpu-2"))
}

func TestSessionNodeTaintIdempotent(t *testing.T) {
	lobby := testLobby("l1", "alice", "steam")
	c, client := newTaintTestController(testNode("gpu-1"), testLobbyPod(lobby, "gpu-1"))
	lobbies := []*direwolfv1alpha1.Lobby{lobby}

	require.NoError(t, c.syncSessionNodeTaints(context.Background(), lobbies))
	require.Equal(t, 1, countNodePatches(client))
	require.NoError(t, c.syncSessionNodeTaints(context.Background(), lobbies))
	require.Equal(t, 1, countNodePatches(client), "second sync must not patch")
	require.Len(t, nodeTaints(t, client, "gpu-1"), 1)
}

func TestSessionNodeTaintNotAddedBeforeScheduling(t *testing.T) {
	lobby := testLobby("l1", "alice", "steam")
	c, client := newTaintTestController(testNode("gpu-1"), testLobbyPod(lobby, ""))

	require.NoError(t, c.syncSessionNodeTaints(context.Background(), []*direwolfv1alpha1.Lobby{lobby}))
	require.False(t, hasTestTaint(t, client, "gpu-1"))
}

func TestSessionNodeTaintNotAddedForPodWithoutToleration(t *testing.T) {
	lobby := testLobby("l1", "alice", "steam")
	pod := testLobbyPod(lobby, "gpu-1")
	pod.Spec.Tolerations = nil
	c, client := newTaintTestController(testNode("gpu-1"), pod)

	require.NoError(t, c.syncSessionNodeTaints(context.Background(), []*direwolfv1alpha1.Lobby{lobby}))
	require.False(t, hasTestTaint(t, client, "gpu-1"))
}

func TestSessionNodeTaintRefCount(t *testing.T) {
	l1 := testLobby("l1", "alice", "steam")
	l2 := testLobby("l2", "bob", "firefox")
	taint := sessionNodeTaint(testTaintKey)
	c, client := newTaintTestController(
		testNode("gpu-1", taint),
		testLobbyPod(l1, "gpu-1"), testLobbyPod(l2, "gpu-1"),
	)
	ctx := context.Background()

	// l1 deleted (removed from the informer): l2 still holds the node.
	require.NoError(t, c.syncSessionNodeTaints(ctx, []*direwolfv1alpha1.Lobby{l2}))
	require.True(t, hasTestTaint(t, client, "gpu-1"))

	// l2 is terminating: a deleting lobby no longer holds the node.
	l2.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	require.NoError(t, c.syncSessionNodeTaints(ctx, []*direwolfv1alpha1.Lobby{l2}))
	require.False(t, hasTestTaint(t, client, "gpu-1"))
}

func TestSessionNodeTaintRemovalPreservesOtherTaints(t *testing.T) {
	other := corev1.Taint{Key: "dedicated", Value: "gaming", Effect: corev1.TaintEffectNoSchedule}
	// Same key, different effect: not ours, must be left alone.
	sameKeyNoSchedule := corev1.Taint{Key: testTaintKey, Effect: corev1.TaintEffectNoSchedule}
	c, client := newTaintTestController(
		testNode("gpu-1", other, sessionNodeTaint(testTaintKey), sameKeyNoSchedule),
	)

	require.NoError(t, c.syncSessionNodeTaints(context.Background(), nil))
	require.Equal(t, []corev1.Taint{other, sameKeyNoSchedule}, nodeTaints(t, client, "gpu-1"))
}

func TestSessionNodeTaintFollowsPod(t *testing.T) {
	lobby := testLobby("l1", "alice", "steam")
	c, client := newTaintTestController(
		testNode("gpu-1", sessionNodeTaint(testTaintKey)), testNode("gpu-2"),
		testLobbyPod(lobby, "gpu-2"),
	)

	require.NoError(t, c.syncSessionNodeTaints(context.Background(), []*direwolfv1alpha1.Lobby{lobby}))
	require.False(t, hasTestTaint(t, client, "gpu-1"))
	require.True(t, hasTestTaint(t, client, "gpu-2"))
}

func TestSessionNodeTaintIgnoresTerminatingAndMissingPods(t *testing.T) {
	l1 := testLobby("l1", "alice", "steam")
	l2 := testLobby("l2", "bob", "firefox") // no pod at all
	pod := testLobbyPod(l1, "gpu-1")
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	pod.Finalizers = []string{"test"}
	c, client := newTaintTestController(testNode("gpu-1", sessionNodeTaint(testTaintKey)), pod)

	require.NoError(t, c.syncSessionNodeTaints(context.Background(), []*direwolfv1alpha1.Lobby{l1, l2}))
	require.False(t, hasTestTaint(t, client, "gpu-1"))
}

func TestSessionNodeTaintRetriesOnConflict(t *testing.T) {
	lobby := testLobby("l1", "alice", "steam")
	c, client := newTaintTestController(testNode("gpu-1"), testLobbyPod(lobby, "gpu-1"))
	conflicts := 1
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, kerrors.NewConflict(corev1.Resource("nodes"), "gpu-1", errors.New("conflict"))
		}
		return false, nil, nil
	})

	require.NoError(t, c.syncSessionNodeTaints(context.Background(), []*direwolfv1alpha1.Lobby{lobby}))
	require.True(t, hasTestTaint(t, client, "gpu-1"))
	require.Equal(t, 2, countNodePatches(client))
}

func TestSessionNodeTaintDisabled(t *testing.T) {
	lobby := testLobby("l1", "alice", "steam")
	c, client := newTaintTestController(testNode("gpu-1", sessionNodeTaint(testTaintKey)), testLobbyPod(lobby, "gpu-1"))
	c.SessionNodeTaintKey = ""

	require.NoError(t, c.syncSessionNodeTaints(context.Background(), nil))
	require.Empty(t, client.Actions())
}

// TestSessionNodeTaintReleasedOnLobbyDeletion exercises the path the lobby
// controller takes when a Lobby is deleted: Reconcile is called with a nil
// object and the sweep reads the remaining lobbies from the informer.
func TestSessionNodeTaintReleasedOnLobbyDeletion(t *testing.T) {
	lobby := testLobby("l1", "alice", "steam")
	c, client := newTaintTestController(testNode("gpu-1"), testLobbyPod(lobby, "gpu-1"))

	dwClient := generatedclient.NewSimpleClientset(lobby)
	factory := generatedinformers.NewSharedInformerFactory(dwClient, 0)
	lobbyInformer := factory.Direwolf().V1alpha1().Lobbies().Informer()
	c.LobbyInformer = generic.NewInformer[*direwolfv1alpha1.Lobby](lobbyInformer)
	stop := make(chan struct{})
	defer close(stop)
	factory.Start(stop)
	require.True(t, cache.WaitForCacheSync(stop, lobbyInformer.HasSynced))

	ctx := context.Background()
	require.NoError(t, c.reconcileSessionNodeTaints(ctx))
	require.True(t, hasTestTaint(t, client, "gpu-1"))

	require.NoError(t, dwClient.DirewolfV1alpha1().Lobbies(testNamespace).Delete(ctx, lobby.Name, metav1.DeleteOptions{}))
	require.Eventually(t, func() bool {
		l, _ := c.LobbyInformer.List(labels.Everything())
		return len(l) == 0
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, c.Reconcile(testNamespace, lobby.Name, nil))
	require.False(t, hasTestTaint(t, client, "gpu-1"))
}

func TestAddSessionNodeTaintToleration(t *testing.T) {
	spec := &corev1.PodSpec{}
	addSessionNodeTaintToleration(spec, testTaintKey)
	addSessionNodeTaintToleration(spec, testTaintKey)
	require.Equal(t, []corev1.Toleration{{
		Key: testTaintKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute,
	}}, spec.Tolerations)

	// An App template that already tolerates everything gets nothing added.
	spec = &corev1.PodSpec{Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}}}
	addSessionNodeTaintToleration(spec, testTaintKey)
	require.Len(t, spec.Tolerations, 1)

	// Disabled: nothing added.
	spec = &corev1.PodSpec{}
	addSessionNodeTaintToleration(spec, "")
	require.Empty(t, spec.Tolerations)
}
