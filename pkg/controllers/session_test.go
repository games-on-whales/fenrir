package controllers

import (
	"context"
	"os"
	"testing"
	"time"

	v1alpha1api "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"k8s.io/utils/ptr"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	sigsyaml "sigs.k8s.io/yaml"
)

// TestSessionControllerReconcilePath builds a session CR, runs the controller's
// reconcile helper methods and logs the resulting Deployment YAML. This does
// not call the full controller Run loop, but exercises the same code paths the
// controller would use to create the Deployment from the App/Profile/Session CRs.
func TestSessionControllerReconcilePath(t *testing.T) {
	ctx := context.Background()

	// Read fixtures
	profileYamlData, err := os.ReadFile("../../examples/profile.yaml")
	if err != nil {
		t.Fatalf("failed to read profile.yaml: %v", err)
	}
	steamYamlData, err := os.ReadFile("../../examples/steam.yaml")
	if err != nil {
		t.Fatalf("failed to read steam.yaml: %v", err)
	}

	// Unmarshal into API types
	var profile v1alpha1api.Profile
	if err := sigsyaml.Unmarshal(profileYamlData, &profile); err != nil {
		t.Fatalf("failed to unmarshal profile yaml: %v", err)
	}
	var app v1alpha1api.App
	if err := sigsyaml.Unmarshal(steamYamlData, &app); err != nil {
		t.Fatalf("failed to unmarshal app yaml: %v", err)
	}

	// Ensure App is in same namespace as profile for this test (controller resolves App by session namespace)
	app.Namespace = profile.Namespace

	// Create fake clients pre-seeded with profile and App
	fakeDirewolf := generatedclient.NewSimpleClientset(&profile, &app)
	fakeK8s := k8sfake.NewSimpleClientset()

	// Create informer factories
	dwFactory := generatedinformers.NewSharedInformerFactory(fakeDirewolf, 0)
	k8sFactory := informers.NewSharedInformerFactory(fakeK8s, 0)

	// Build generic informers needed by NewSessionController
	sessionInformer := generic.NewInformer[*v1alpha1types.Session](dwFactory.Direwolf().V1alpha1().Sessions().Informer())
	appInformer := generic.NewInformer[*v1alpha1types.App](dwFactory.Direwolf().V1alpha1().Apps().Informer())
	profileInformer := generic.NewInformer[*v1alpha1types.Profile](dwFactory.Direwolf().V1alpha1().Profiles().Informer())
	deploymentInformer := generic.NewInformer[*appsv1.Deployment](k8sFactory.Apps().V1().Deployments().Informer())

	// Create a session client scoped to the test namespace
	sessionClient := fakeDirewolf.DirewolfV1alpha1().Sessions(profile.Namespace)

	// Instantiate controller with nil gateway clients (not used in this test)
	sc := NewSessionController(
		fakeK8s,
		nil,
		nil,
		sessionClient,
		sessionInformer,
		appInformer,
		profileInformer,
		deploymentInformer,
		SessionControllerOptions{},
	)

	// Start informers and wait for caches
	stopCh := make(chan struct{})
	defer close(stopCh)
	dwFactory.Start(stopCh)
	k8sFactory.Start(stopCh)
	// Wait for cache sync
	if !cacheWaitForSync(k8sFactory, dwFactory, 5*time.Second) {
		t.Fatal("informers failed to sync")
	}

	// Create a Session CR that references the profile and app
	sess := &v1alpha1api.Session{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sess-alex-steam",
			Namespace: profile.Namespace,
		},
		Spec: v1alpha1api.SessionSpec{
			ProfileReference: v1alpha1api.ProfileReference{Name: profile.Name},
			GameReference:    v1alpha1api.GameReference{Name: app.Name},
			PairingReference: v1alpha1api.PairingReference{Name: "pairing1"},
			GatewayReference: v1alpha1api.GatewayReference{Name: "gw1"},
			Config: v1alpha1api.SessionInfo{
				AESKey:             "k1",
				AESIV:              "i1",
				VideoWidth:         1920,
				VideoHeight:        1080,
				VideoRefreshRate:   60,
				SurroundAudioFlags: 0,
			},
		},
	}

	// Create the Session in the fake client so informers can list it if necessary
	if _, err := fakeDirewolf.DirewolfV1alpha1().Sessions(profile.Namespace).Create(ctx, sess, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create session in fake client: %v", err)
	}

	// Run the same sequence the controller uses (without reconcileActiveStreams that talks to wolf)
	// 1) allocatePorts
	if err := sc.allocatePorts(ctx, sess); err != nil {
		t.Fatalf("allocatePorts failed: %v", err)
	}
	// Mark PortsAllocated condition so reconcilePod proceeds
	sess.Status.Conditions = append(sess.Status.Conditions, metav1.Condition{Type: "PortsAllocated", Status: metav1.ConditionTrue, Reason: "Test", Message: "allocated"})

	// Pre-create an empty ConfigMap that reconcileConfigMap will apply/patch.
	cmName := sc.deploymentName(sess)
	_, err = fakeK8s.CoreV1().ConfigMaps(profile.Namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: profile.Namespace,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to pre-create configmap: %v", err)
	}

	// 2) reconcileConfigMap, we're no longer using the config map toml
	// if err := sc.reconcileConfigMap(ctx, sess); err != nil {
	// 	t.Fatalf("reconcileConfigMap failed: %v", err)
	// }

	// 3) reconcilePVC
	if err := sc.reconcilePVC(ctx, sess); err != nil {
		t.Fatalf("reconcilePVC failed: %v", err)
	}

	// Pre-create a minimal deployment so the fake k8s client's Apply can update it
	deployName := sc.deploymentName(sess)
	_, err = fakeK8s.AppsV1().Deployments(profile.Namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: profile.Namespace,
		},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](0)},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to pre-create deployment: %v", err)
	}

	// 4) reconcilePod (creates/updates Deployment)
	if err := sc.reconcilePod(ctx, sess); err != nil {
		t.Fatalf("reconcilePod failed: %v", err)
	}

	// Pre-create a minimal service so Apply will succeed
	svcName := sess.Name + "-rtp"
	_, err = fakeK8s.CoreV1().Services(profile.Namespace).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: profile.Namespace},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "direwolf-worker"}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to pre-create service: %v", err)
	}

	// 5) reconcileService (creates Service)
	if err := sc.reconcileService(ctx, sess); err != nil {
		t.Fatalf("reconcileService failed: %v", err)
	}

	// Fetch the created deployment and log YAML
	deploymentName := sc.deploymentName(sess)
	dep, err := fakeK8s.AppsV1().Deployments(profile.Namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get deployment from fake k8s: %v", err)
	}
	out, err := sigsyaml.Marshal(dep)
	if err != nil {
		t.Fatalf("failed to marshal deployment: %v", err)
	}
	t.Logf("Generated Deployment YAML:\n%s", string(out))
}

// cacheWaitForSync waits for both informer factories to sync (typed and direwolf)
func cacheWaitForSync(k8sFactory informers.SharedInformerFactory, dwFactory generatedinformers.SharedInformerFactory, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ch := make(chan bool, 1)
	go func() {
		// Wait for both factories' known informers to sync
		k8sFactory.WaitForCacheSync(ctx.Done())
		dwFactory.WaitForCacheSync(ctx.Done())
		ch <- true
	}()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}
