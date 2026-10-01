package controllers

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	v1alpha1api "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	sigsyaml "sigs.k8s.io/yaml"
)

// TestSessionControllerReconcilePath builds a session CR, runs the controller's
// reconcile helper methods and logs the resulting Pod YAML. This does not call
// the full controller Run loop, but exercises the same code paths the
// controller would use to create the Pod from the App/User/Session CRs.
func TestSessionControllerReconcilePath(t *testing.T) {
	ctx := context.Background()
	sc, fakeK8s, sess, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml")

	// The nri-input plugin grants input devices only to pods carrying this label.
	if got := pod.Labels[v1alpha1types.SessionPodLabel]; got != v1alpha1types.SessionPodLabelValue {
		t.Errorf("pod label %s = %q, want %q", v1alpha1types.SessionPodLabel, got, v1alpha1types.SessionPodLabelValue)
	}
	// Job-like: one run per Session, owned by it, never restarted.
	if pod.Name != sess.Name {
		t.Errorf("pod name = %q, want the session's %q", pod.Name, sess.Name)
	}
	if !metav1.IsControlledBy(pod, sess) {
		t.Errorf("pod owners = %+v, want controlled by session %s", pod.OwnerReferences, sess.UID)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	// No per-session Service any more: the pod is on the host network.
	if svcs, _ := fakeK8s.CoreV1().Services(sess.Namespace).List(ctx, metav1.ListOptions{}); len(svcs.Items) != 0 {
		t.Errorf("reconcile created Services: %+v", svcs.Items)
	}
	if got := pod.Annotations[portBlockAnnotation]; got != strconv.Itoa(int(sess.Status.Ports.HTTP)) {
		t.Errorf("pod %s = %q, want %d", portBlockAnnotation, got, sess.Status.Ports.HTTP)
	}
	podSpec := pod.Spec
	if !podSpec.HostNetwork || podSpec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Errorf("pod hostNetwork=%v dnsPolicy=%q, want true/%q", podSpec.HostNetwork, podSpec.DNSPolicy, corev1.DNSClusterFirstWithHostNet)
	}
	if got := podSpec.NodeSelector["kubernetes.io/hostname"]; got != "talos04" {
		t.Errorf("nodeSelector kubernetes.io/hostname = %q, want talos04", got)
	}
	if !slices.ContainsFunc(podSpec.Tolerations, func(tol corev1.Toleration) bool { return tol.Key == "nvidia.com/gpu" }) {
		t.Errorf("pod does not tolerate the pinned node's taint: %+v", podSpec.Tolerations)
	}

	// wolf-agent must be started with the token file mounted from its Secret.
	var agent *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == "wolf-agent" {
			agent = &podSpec.Containers[i]
		}
	}
	if agent == nil {
		t.Fatal("no wolf-agent container")
	}
	if !slices.Contains(agent.Args, "--token-file="+wolfAgentTokenMountPath+"/"+wolfAgentTokenKey) {
		t.Errorf("wolf-agent args missing --token-file: %v", agent.Args)
	}

	// Every listener must be on the session's block, so session pods sharing
	// the node IP never collide.
	ports := sess.Status.Ports
	if ports != blockPorts(20000) {
		t.Fatalf("session ports = %+v, want the first block", ports)
	}
	if !slices.Contains(agent.Args, "--port=20006") || agent.Ports[0].ContainerPort != ports.WolfAgent ||
		agent.ReadinessProbe.HTTPGet.Port.IntVal != ports.WolfAgent {
		t.Errorf("wolf-agent not on port %d: args %v ports %+v", ports.WolfAgent, agent.Args, agent.Ports)
	}
	var wolf *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == "wolf" {
			wolf = &podSpec.Containers[i]
		}
	}
	if wolf == nil {
		t.Fatal("no wolf container")
	}
	wantEnv := map[string]int32{
		"WOLF_HTTP_PORT":       ports.HTTP,
		"WOLF_HTTPS_PORT":      ports.HTTPS,
		"WOLF_RTSP_SETUP_PORT": ports.RTSP,
		"WOLF_CONTROL_PORT":    ports.Control,
		"WOLF_VIDEO_PING_PORT": ports.VideoRTP,
		"WOLF_AUDIO_PING_PORT": ports.AudioRTP,
	}
	for name, port := range wantEnv {
		if !slices.Contains(wolf.Env, corev1.EnvVar{Name: name, Value: strconv.Itoa(int(port))}) {
			t.Errorf("wolf env missing %s=%d", name, port)
		}
		if !slices.ContainsFunc(wolf.Ports, func(p corev1.ContainerPort) bool { return p.ContainerPort == port }) {
			t.Errorf("wolf does not declare container port %d (%s)", port, name)
		}
	}
	var tokenVolume bool
	for _, v := range podSpec.Volumes {
		if v.Name == "wolf-agent-token" && v.Secret != nil && v.Secret.SecretName == agentTokenSecretName(sess.Name) {
			tokenVolume = true
		}
	}
	if !tokenVolume {
		t.Error("pod has no wolf-agent-token secret volume")
	}
	if _, tokenErr := sc.agentToken(ctx, sess); tokenErr != nil {
		t.Errorf("token secret not created by reconcilePod: %v", tokenErr)
	}

	out, err := sigsyaml.Marshal(pod)
	if err != nil {
		t.Fatalf("failed to marshal pod: %v", err)
	}
	t.Logf("Generated Pod YAML:\n%s", string(out))
}

// reconcileFixtures runs the controller's reconcile helpers for the given User and
// App fixtures and returns the resulting session Pod.
func reconcileFixtures(t *testing.T, userPath, appPath string) (*SessionController, *k8sfake.Clientset, *v1alpha1api.Session, *corev1.Pod) {
	t.Helper()
	ctx := context.Background()

	userYamlData, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", userPath, err)
	}
	steamYamlData, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", appPath, err)
	}

	// Unmarshal into API types
	var user v1alpha1api.User
	if err := sigsyaml.Unmarshal(userYamlData, &user); err != nil {
		t.Fatalf("failed to unmarshal user yaml: %v", err)
	}
	var app v1alpha1api.App
	if err := sigsyaml.Unmarshal(steamYamlData, &app); err != nil {
		t.Fatalf("failed to unmarshal app yaml: %v", err)
	}

	// Ensure App is in same namespace as User for this test (controller resolves App by session namespace)
	app.Namespace = user.Namespace

	// Create fake clients pre-seeded with User and App
	fakeDirewolf := generatedclient.NewSimpleClientset(&user, &app)
	fakeK8s := k8sfake.NewSimpleClientset()

	// Create informer factories
	dwFactory := generatedinformers.NewSharedInformerFactory(fakeDirewolf, 0)
	k8sFactory := informers.NewSharedInformerFactory(fakeK8s, 0)

	// Build generic informers needed by NewSessionController
	sessionInformer := generic.NewInformer[*v1alpha1types.Session](dwFactory.Direwolf().V1alpha1().Sessions().Informer())
	appInformer := generic.NewInformer[*v1alpha1types.App](dwFactory.Direwolf().V1alpha1().Apps().Informer())
	userInformer := generic.NewInformer[*v1alpha1types.User](dwFactory.Direwolf().V1alpha1().Users().Informer())
	podInformer := generic.NewInformer[*corev1.Pod](k8sFactory.Core().V1().Pods().Informer())

	// Create a session client scoped to the test namespace
	sessionClient := fakeDirewolf.DirewolfV1alpha1().Sessions(user.Namespace)

	// Instantiate controller with nil gateway clients (not used in this test)
	sc := NewSessionController(
		fakeK8s,
		nil,
		nil,
		sessionClient,
		sessionInformer,
		appInformer,
		userInformer,
		podInformer,
		SessionControllerOptions{
			SessionPortRange:    PortRange{Min: 20000, Max: 20999},
			SessionNodeSelector: map[string]string{"kubernetes.io/hostname": "talos04"},
			SessionTolerations: []corev1.Toleration{
				{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpEqual, Value: "present", Effect: corev1.TaintEffectNoSchedule},
			},
		},
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

	// Create a Session CR that references the user and app
	sess := &v1alpha1api.Session{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sess-alex-steam",
			Namespace: user.Namespace,
			UID:       "sess-uid",
		},
		Spec: v1alpha1api.SessionSpec{
			UserReference:    v1alpha1api.UserReference{Name: user.Name},
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
	if _, err := fakeDirewolf.DirewolfV1alpha1().Sessions(user.Namespace).Create(ctx, sess, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create session in fake client: %v", err)
	}

	// Run the same sequence the controller uses (without reconcileActiveStreams that talks to wolf)
	// 1) allocatePorts
	if err := sc.allocatePorts(ctx, sess); err != nil {
		t.Fatalf("allocatePorts failed: %v", err)
	}
	// Mark PortsAllocated condition so reconcilePod proceeds
	sess.Status.Conditions = append(sess.Status.Conditions, metav1.Condition{Type: "PortsAllocated", Status: metav1.ConditionTrue, Reason: "Test", Message: "allocated"})

	// 3) reconcilePVC
	if err := sc.reconcilePVC(ctx, sess); err != nil {
		t.Fatalf("reconcilePVC failed: %v", err)
	}

	// 4) reconcilePod (creates the Pod)
	pod, err := sc.reconcilePod(ctx, sess)
	if err != nil {
		t.Fatalf("reconcilePod failed: %v", err)
	}
	return sc, fakeK8s, sess, pod
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

// TestSessionPodGPUFromDRAClaim checks the GPU reaches the session pod only through
// the App's DRA ResourceClaim: the pod keeps spec.resourceClaims, and both the game
// container and the wolf sidecar (NVENC) reference the claim.
func TestSessionPodGPUFromDRAClaim(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/nvidia_devices/user.yaml", "../../examples/nvidia_devices/firefox.yaml") //nolint:dogsled // only the Pod matters here
	spec := pod.Spec

	want := corev1.PodResourceClaim{Name: "gpu", ResourceClaimTemplateName: new("nvidia-gpu")}
	if len(spec.ResourceClaims) != 1 || !reflect.DeepEqual(spec.ResourceClaims[0], want) {
		t.Errorf("pod resourceClaims = %+v, want [%+v]", spec.ResourceClaims, want)
	}
	if spec.RuntimeClassName != nil {
		t.Errorf("runtimeClassName = %q, want unset", *spec.RuntimeClassName)
	}

	claimed := map[string]bool{}
	for _, c := range spec.Containers {
		if slices.Contains(c.Resources.Claims, corev1.ResourceClaim{Name: "gpu"}) {
			claimed[c.Name] = true
		}
		for _, e := range c.Env {
			if e.Name == "NVIDIA_VISIBLE_DEVICES" {
				t.Errorf("container %s has NVIDIA_VISIBLE_DEVICES=%q", c.Name, e.Value)
			}
		}
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			t.Errorf("container %s is privileged", c.Name)
		}
	}
	for _, name := range []string{"app", "wolf"} {
		if !claimed[name] {
			t.Errorf("container %s does not reference the gpu claim", name)
		}
	}
	for _, name := range []string{"wolf-agent", "pulseaudio"} {
		if claimed[name] {
			t.Errorf("sidecar %s should not reference the gpu claim", name)
		}
	}
}

func TestMergeResourceRequirementsKeepsClaims(t *testing.T) {
	defaults := corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}}
	overrides := &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}, {Name: "nic", Request: "vf"}}}

	got := mergeResourceRequirements(defaults, overrides)
	want := []corev1.ResourceClaim{{Name: "gpu"}, {Name: "nic", Request: "vf"}}
	if !reflect.DeepEqual(got.Claims, want) {
		t.Errorf("merged claims = %+v, want %+v", got.Claims, want)
	}
	if len(defaults.Claims) != 1 {
		t.Errorf("defaults mutated: %+v", defaults.Claims)
	}
}

// Server-side apply keys resources.claims by name alone, so one name must never
// appear twice; a whole-claim entry wins over a single-request one.
func TestAppendResourceClaimsOneEntryPerName(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dst, add  []corev1.ResourceClaim
		wantClaim []corev1.ResourceClaim
	}{
		{"request then whole", []corev1.ResourceClaim{{Name: "gpu", Request: "gpu"}}, []corev1.ResourceClaim{{Name: "gpu"}}, []corev1.ResourceClaim{{Name: "gpu"}}},
		{"whole then request", []corev1.ResourceClaim{{Name: "gpu"}}, []corev1.ResourceClaim{{Name: "gpu", Request: "gpu"}}, []corev1.ResourceClaim{{Name: "gpu"}}},
		{"two requests", nil, []corev1.ResourceClaim{{Name: "gpu", Request: "a"}, {Name: "gpu", Request: "b"}}, []corev1.ResourceClaim{{Name: "gpu"}}},
		{"same request", nil, []corev1.ResourceClaim{{Name: "gpu", Request: "a"}, {Name: "gpu", Request: "a"}}, []corev1.ResourceClaim{{Name: "gpu", Request: "a"}}},
		{"distinct names", []corev1.ResourceClaim{{Name: "gpu"}}, []corev1.ResourceClaim{{Name: "nic", Request: "vf"}}, []corev1.ResourceClaim{{Name: "gpu"}, {Name: "nic", Request: "vf"}}},
	} {
		if got := appendResourceClaims(tc.dst, tc.add...); !reflect.DeepEqual(got, tc.wantClaim) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.wantClaim)
		}
	}
}

// mountsAt maps each mount path of the named container to its volume name.
func mountsAt(t *testing.T, spec *corev1.PodSpec, name string) map[string]string {
	t.Helper()
	for _, c := range spec.Containers {
		if c.Name != name {
			continue
		}
		out := map[string]string{}
		for _, m := range c.VolumeMounts {
			if prev, dup := out[m.MountPath]; dup {
				t.Errorf("container %s mounts %s twice (%s, %s)", name, m.MountPath, prev, m.Name)
			}
			out[m.MountPath] = m.Name
		}
		return out
	}
	t.Fatalf("no container %s", name)
	return nil
}

func TestSessionPodSharesHotplugVolumes(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/nvidia_devices/user.yaml", "../../examples/nvidia_devices/firefox.yaml") //nolint:dogsled // only the Pod matters here
	spec := pod.Spec

	for _, v := range []string{hotplugDevVolume, hotplugUdevVolume} {
		i := slices.IndexFunc(spec.Volumes, func(vol corev1.Volume) bool { return vol.Name == v })
		if i < 0 || spec.Volumes[i].EmptyDir == nil {
			t.Errorf("volume %s missing or not an emptyDir", v)
		}
	}
	for _, name := range []string{"app", "wolf-agent"} {
		got := mountsAt(t, &spec, name)
		if got["/dev/input"] != hotplugDevVolume || got["/run/udev"] != hotplugUdevVolume {
			t.Errorf("container %s mounts /dev/input=%q /run/udev=%q", name, got["/dev/input"], got["/run/udev"])
		}
	}
	for _, name := range []string{"wolf", "pulseaudio"} {
		got := mountsAt(t, &spec, name)
		if _, ok := got["/dev/input"]; ok {
			t.Errorf("container %s should not mount /dev/input", name)
		}
	}
}

func TestSessionPodKeepsAppsOwnInputMount(t *testing.T) {
	// steam.yaml mounts the host's /dev/input into the app itself.
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	got := mountsAt(t, &pod.Spec, "app")
	if got["/dev/input"] != "input-events" {
		t.Errorf("app /dev/input = %q, want the App's own input-events", got["/dev/input"])
	}
	if got["/run/udev"] != hotplugUdevVolume {
		t.Errorf("app /run/udev = %q, want %s", got["/run/udev"], hotplugUdevVolume)
	}
}

func TestWolfCommandPicksClaimedRenderNode(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dri := t.TempDir()
	script := strings.ReplaceAll(wolfCommand[2], "/dev/dri/", dri+"/")
	script = strings.ReplaceAll(script, "exec /entrypoint.sh", `echo "$WOLF_RENDER_NODE"`)
	run := func(env ...string) string {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(); got != "" {
		t.Errorf("no render node: WOLF_RENDER_NODE = %q, want unset", got)
	}
	// [ -c ] needs a char device; /dev/null stands in for the claimed node.
	if err := os.Symlink("/dev/null", dri+"/renderD129"); err != nil {
		t.Fatal(err)
	}
	if got, want := run(), dri+"/renderD129"; got != want {
		t.Errorf("WOLF_RENDER_NODE = %q, want %q", got, want)
	}
	// The Wolf image's own ENV points at a node this card doesn't have.
	if got, want := run("WOLF_RENDER_NODE="+dri+"/renderD128"), dri+"/renderD129"; got != want {
		t.Errorf("image default kept: WOLF_RENDER_NODE = %q, want %q", got, want)
	}
	// A node that exists (set by the App or a User policy) is kept.
	if got := run("WOLF_RENDER_NODE=/dev/null"); got != "/dev/null" {
		t.Errorf("existing WOLF_RENDER_NODE overridden: got %q", got)
	}
}

func TestSessionPodStartsWolfThroughRenderNodeWrapper(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	for _, c := range pod.Spec.Containers {
		if c.Name == "wolf" {
			if !slices.Equal(c.Command, wolfCommand) {
				t.Errorf("wolf command = %q, want wolfCommand", c.Command)
			}
			return
		}
	}
	t.Fatal("no wolf container")
}

func TestSessionPodKeepsAppsOwnHome(t *testing.T) {
	// A Steam App mounts the Steam home it shares with the Library at
	// /home/retro; a second (wolf-data) mount there makes the pod invalid.
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-own-home.yaml") //nolint:dogsled // only the Pod matters here
	if got := mountsAt(t, &pod.Spec, "app")[AppHomePath]; got != "steam-home" {
		t.Errorf("app %s = %q, want the App's own steam-home", AppHomePath, got)
	}
}

func TestSessionPodMountsAppHomeByDefault(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	if got := mountsAt(t, &pod.Spec, "app")[AppHomePath]; got != "wolf-data" {
		t.Errorf("app %s = %q, want wolf-data", AppHomePath, got)
	}
}

func TestValidateNoHotplugOverride(t *testing.T) {
	for p, wantErr := range map[string]bool{
		"/dev/input":        true,
		"/dev/input/":       true,
		"/dev/input/event":  true,
		"dev/input":         true,
		"run/udev/data":     true,
		"dev/../dev/input":  true,
		"/var/run/udev":     true,
		"var/run/udev/data": true,
		"/run/udev":         true,
		"/run/udev/data":    true,
		"/dev/inputs":       false,
		"/run":              false,
		"/etc/wolf":         false,
	} {
		err := validateNoHotplugOverride([]corev1.VolumeMount{{Name: "v", MountPath: p}})
		if (err != nil) != wantErr {
			t.Errorf("mount at %s: err = %v, want error %v", p, err, wantErr)
		}
	}
}

func TestWithHotplugMountsSeesRelativePaths(t *testing.T) {
	got := withHotplugMounts([]corev1.VolumeMount{{Name: "own", MountPath: "dev/input"}})
	if len(got) != 2 || got[1].MountPath != "/run/udev" {
		t.Errorf("mounts = %+v, want the App's dev/input kept and only /run/udev added", got)
	}
}

func TestBuildPodRejectsWolfAgentHotplugOverride(t *testing.T) {
	sc, _, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml")
	user, err := sc.UserInformer.Namespaced(sess.Namespace).Get(sess.Spec.UserReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	// The informer's copy: buildPod reads it back below.
	user.Spec.SidecarPolicies.WolfAgent = &v1alpha1api.SidecarPolicy{
		VolumeMounts: []corev1.VolumeMount{{Name: user.Spec.Volumes[0].Name, MountPath: "run/udev/data"}},
	}
	if _, err := sc.buildPod(sess); err == nil || !strings.Contains(err.Error(), "reserves for hotplugged devices") {
		t.Errorf("buildPod err = %v, want the hotplug override rejected", err)
	}
}
