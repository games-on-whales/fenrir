package controllers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	sigsyaml "sigs.k8s.io/yaml"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

const portsTestNS = "direwolf"

type portsFixture struct {
	dw   *generatedclient.Clientset
	sc   *SessionController
	app  string
	user string
}

// newPortsFixture builds a SessionController over fake clients seeded with
// the example App and k8sObjects, with informers synced.
func newPortsFixture(t *testing.T, k8sObjects ...runtime.Object) *portsFixture {
	t.Helper()
	steam, err := os.ReadFile("../../examples/steam.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var app v1alpha1types.App
	if err = sigsyaml.Unmarshal(steam, &app); err != nil {
		t.Fatal(err)
	}
	app.Namespace = portsTestNS
	userYAML, err := os.ReadFile("../../examples/user.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var user v1alpha1types.User
	if err = sigsyaml.Unmarshal(userYAML, &user); err != nil {
		t.Fatal(err)
	}
	user.Namespace = portsTestNS

	dw := generatedclient.NewSimpleClientset(&app, &user)
	k8s := k8sfake.NewClientset(k8sObjects...)
	dwF := generatedinformers.NewSharedInformerFactory(dw, 0)
	kF := informers.NewSharedInformerFactory(k8s, 0)
	sc := NewSessionController(k8s, nil, nil, dw.DirewolfV1alpha1().Sessions(portsTestNS),
		generic.NewInformer[*v1alpha1types.Session](dwF.Direwolf().V1alpha1().Sessions().Informer()),
		generic.NewInformer[*v1alpha1types.App](dwF.Direwolf().V1alpha1().Apps().Informer()),
		generic.NewInformer[*v1alpha1types.User](dwF.Direwolf().V1alpha1().Users().Informer()),
		generic.NewInformer[*corev1.Pod](kF.Core().V1().Pods().Informer()),
		SessionControllerOptions{SessionPortRange: PortRange{Min: 20000, Max: 20999}, DisconnectGracePeriod: 10 * time.Minute})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	dwF.Start(stop)
	kF.Start(stop)
	if !cacheWaitForSync(kF, dwF, 5*time.Second) {
		t.Fatal("informers failed to sync")
	}
	return &portsFixture{dw: dw, sc: sc, app: app.Name, user: user.Name}
}

func (f *portsFixture) session(name, user string) *v1alpha1types.Session {
	return &v1alpha1types.Session{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: portsTestNS, UID: types.UID(name + "-uid")},
		Spec: v1alpha1types.SessionSpec{
			UserReference: v1alpha1types.UserReference{Name: user},
			GameReference: v1alpha1types.GameReference{Name: f.app},
			Config:        v1alpha1types.SessionInfo{AESKey: "k", AESIV: "i", ClientIP: "192.0.2.10"},
		},
	}
}

// waitInformer waits until the session informer does (or no longer) holds name.
func (f *portsFixture) waitInformer(t *testing.T, name string, present bool) {
	t.Helper()
	for range 500 {
		_, err := f.sc.SessionInformer.Namespaced(portsTestNS).Get(name)
		if (err == nil) == present {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("informer never saw %s present=%v", name, present)
}

// Deleting a Session frees its block for the next one.
func TestSessionDeletionReleasesPorts(t *testing.T) {
	ctx := context.Background()
	f := newPortsFixture(t)

	alex := f.session("alex-1", "alex")
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(ctx, alex, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitInformer(t, alex.Name, true)
	if err := f.sc.allocatePorts(ctx, alex); err != nil {
		t.Fatal(err)
	}
	if alex.Status.Ports != blockPorts(20000) {
		t.Fatalf("alex got %+v, want the first block", alex.Status.Ports)
	}

	if err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Delete(ctx, alex.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitInformer(t, alex.Name, false)
	if err := f.sc.Reconcile(portsTestNS, alex.Name, nil); err != nil {
		t.Fatal(err)
	}

	sam := f.session("sam-1", "sam")
	if err := f.sc.allocatePorts(ctx, sam); err != nil {
		t.Fatal(err)
	}
	if sam.Status.Ports != blockPorts(20000) {
		t.Fatalf("sam got %+v, want alex's released block", sam.Status.Ports)
	}
}

// Blocks recorded in status before an operator restart are not handed out again.
func TestClaimRecordedPortsOnStart(t *testing.T) {
	f := newPortsFixture(t)
	running := f.session("alex-1", "alex")
	running.Status.Ports = blockPorts(20000)

	f.sc.claimRecordedPorts([]*v1alpha1types.Session{running})

	sam := f.session("sam-1", "sam")
	if err := f.sc.allocatePorts(context.Background(), sam); err != nil {
		t.Fatal(err)
	}
	if sam.Status.Ports != blockPorts(20000+sessionPortBlockSize) {
		t.Fatalf("sam got %+v, which a running pod still holds", sam.Status.Ports)
	}
}

// fakeAgent is a wolf-agent stand-in: it lists the Wolf sessions in sessions
// and records adds and stops.
type fakeAgent struct {
	*httptest.Server
	sessions string // JSON array for /api/v1/sessions

	mu      sync.Mutex
	added   int
	last    wolfapi.Session // body of the last add
	stopped []string
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	a := &fakeAgent{sessions: "[]"}
	a.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions":
			fmt.Fprintf(w, `{"success":true,"sessions":%s}`, a.sessions)
		case "/api/v1/sessions/add":
			a.mu.Lock()
			a.added++
			if err := json.NewDecoder(r.Body).Decode(&a.last); err != nil {
				t.Errorf("decoding AddSession body: %v", err)
			}
			a.mu.Unlock()
			fmt.Fprint(w, `{"success":true,"session_id":"4242"}`)
		case "/api/v1/sessions/stop":
			var req struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			a.mu.Lock()
			a.stopped = append(a.stopped, req.SessionID)
			a.mu.Unlock()
			fmt.Fprint(w, `{"success":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	a.TLS = &tls.Config{Certificates: []tls.Certificate{testAgentKeyPair(t)}}
	a.StartTLS()
	t.Cleanup(a.Close)
	return a
}

// testAgentCert is the serving cert every fakeAgent presents and tokenSecret
// pins.
var testAgentCert = sync.OnceValues(func() ([][]byte, error) {
	certPEM, keyPEM, err := generateAgentCert()
	return [][]byte{certPEM, keyPEM}, err
})

func testAgentKeyPair(t *testing.T) tls.Certificate {
	t.Helper()
	pems, err := testAgentCert()
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(pems[0], pems[1])
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// base is the port block whose wolf-agent port is the fake agent's.
func (a *fakeAgent) base(t *testing.T) int32 {
	t.Helper()
	addr, ok := a.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", a.Listener.Addr())
	}
	return int32(addr.Port) - portOffsetWolfAgent //nolint:gosec // a TCP port fits in int32
}

// readyPod is sess's running pod, as reconcilePod would have created it.
func readyPod(sess *v1alpha1types.Session) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: sess.Name, Namespace: sess.Namespace,
			Annotations: map[string]string{portBlockAnnotation: strconv.Itoa(int(sess.Status.Ports.HTTP))},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1types.GroupVersion.String(), Kind: "Session",
				Name: sess.Name, UID: sess.UID, Controller: new(true),
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "127.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func tokenSecret(sess *v1alpha1types.Session) *corev1.Secret {
	pems, err := testAgentCert()
	if err != nil {
		panic(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: agentTokenSecretName(sess.Name), Namespace: sess.Namespace},
		Data: map[string][]byte{
			wolfAgentTokenKey:       []byte("token"),
			corev1.TLSCertKey:       pems[0],
			corev1.TLSPrivateKeyKey: pems[1],
		},
	}
}

// The operator dials wolf-agent on the block's agent port and advertises the
// block's RTSP port on the pod (= node) IP.
func TestReconcileActiveStreamsUsesPortBlock(t *testing.T) {
	agent := newFakeAgent(t)
	probe := newPortsFixture(t)
	sess := probe.session("alex-1", "alex")
	sess.Status.Ports = blockPorts(agent.base(t))
	sess.Spec.Config.ClientIP = "192.0.2.10"
	f := newPortsFixture(t, tokenSecret(sess))
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(context.Background(), sess, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	if agent.added != 1 {
		t.Errorf("wolf-agent on the block's agent port got %d adds, want 1", agent.added)
	}
	if want := fmt.Sprintf("rtsp://127.0.0.1:%d", sess.Status.Ports.RTSP); sess.Status.StreamURL != want {
		t.Errorf("StreamURL = %q, want %q", sess.Status.StreamURL, want)
	}
	if sess.Status.WolfSessionID != "4242" {
		t.Errorf("WolfSessionID = %q", sess.Status.WolfSessionID)
	}
	if agent.last.ClientIP != "192.0.2.10" {
		t.Errorf("Wolf AddSession client_ip = %q, want the Moonlight client's IP", agent.last.ClientIP)
	}
}

// Anything else bound to the agent port (another host-networked process, say
// while wolf-agent restarts) must fail the handshake before seeing the token,
// whether it serves its own cert or another session's.
func TestReconcileActiveStreamsPinsAgentCert(t *testing.T) {
	otherCert, otherKey, err := generateAgentCert()
	if err != nil {
		t.Fatal(err)
	}
	otherPair, err := tls.X509KeyPair(otherCert, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]*tls.Config{
		"stranger cert":          nil, // httptest's own cert
		"another session's cert": {Certificates: []tls.Certificate{otherPair}},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var gotAuth []string
			stranger := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotAuth = append(gotAuth, r.Header.Get("Authorization"))
				mu.Unlock()
			}))
			stranger.TLS = cfg
			stranger.StartTLS()
			t.Cleanup(stranger.Close)
			agent := &fakeAgent{Server: stranger}

			probe := newPortsFixture(t)
			sess := probe.session("alex-1", "alex")
			sess.Status.Ports = blockPorts(agent.base(t))
			f := newPortsFixture(t, tokenSecret(sess))

			if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err == nil {
				t.Fatal("reconcileActiveStreams succeeded against an unpinned cert")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(gotAuth) != 0 {
				t.Errorf("stranger received %d requests (Authorization %q)", len(gotAuth), gotAuth)
			}
		})
	}
}

// A token Secret from before the cert was pinned cannot be verified against,
// so the Session ends rather than handing its token to an unverified peer.
func TestReconcileActiveStreamsEndsSessionWithoutAgentCert(t *testing.T) {
	agent := newFakeAgent(t)
	probe := newPortsFixture(t)
	sess := probe.session("alex-1", "alex")
	sess.Status.Ports = blockPorts(agent.base(t))
	secret := tokenSecret(sess)
	delete(secret.Data, corev1.TLSCertKey)
	f := newPortsFixture(t, secret)
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(context.Background(), sess, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess))
	if !errors.Is(err, errSessionEnded) {
		t.Fatalf("err = %v, want errSessionEnded", err)
	}
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Get(context.Background(), sess.Name, metav1.GetOptions{}); err == nil {
		t.Error("session still exists")
	}
}
