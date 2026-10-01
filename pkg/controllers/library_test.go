package controllers

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	dwfake "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

const libraryTestNS = "streaming"

var libraryTestNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// fakeLibraryExec answers the idle check's execs and records the rest.
type fakeLibraryExec struct {
	mu        sync.Mutex
	steam     string // steamapps/downloading listing
	heroic    string // download-manager.json contents
	err       error
	shutdowns int
}

func (e *fakeLibraryExec) exec(_ context.Context, _, pod, container string, command []string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if pod != LibraryPodName || container != libraryContainer {
		return "", errors.New("exec into the wrong container")
	}
	script := strings.Join(command, " ")
	switch {
	case strings.Contains(script, "-shutdown"):
		e.shutdowns++
		return "", nil
	case e.err != nil:
		return "", e.err
	case strings.Contains(script, "steamapps/downloading"):
		return e.steam, nil
	case slices.Contains(command, heroicDownloadQueue):
		return e.heroic, nil
	}
	return "", errors.New("unexpected exec: " + script)
}

type libraryFixture struct {
	k8s     *k8sfake.Clientset
	dw      *dwfake.Clientset
	exec    *fakeLibraryExec
	library *LibraryController
}

func newLibraryFixture(t *testing.T, k8sObjs []runtime.Object, dwObjs ...runtime.Object) *libraryFixture {
	t.Helper()
	f := &libraryFixture{
		k8s:  k8sfake.NewClientset(k8sObjs...),
		dw:   dwfake.NewSimpleClientset(dwObjs...),
		exec: &fakeLibraryExec{},
	}
	f.library = &LibraryController{
		Namespace:     libraryTestNS,
		K8sClient:     f.k8s,
		SessionClient: f.dw.DirewolfV1alpha1().Sessions(libraryTestNS),
		Exec:          f.exec.exec,
		now:           func() time.Time { return libraryTestNow },
		LibraryControllerOptions: LibraryControllerOptions{
			Image:        "library:test",
			HomePVC:      "steam-home",
			GamesPVC:     "games",
			GamesPath:    "/games",
			NodeSelector: map[string]string{"kubernetes.io/hostname": "talos04.xerktech.com"},
			Tolerations:  []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
			IdleTimeout:  DefaultLibraryIdleTimeout,
		},
	}
	return f
}

func (f *libraryFixture) podExists(t *testing.T) bool {
	t.Helper()
	_, err := f.k8s.CoreV1().Pods(libraryTestNS).Get(context.Background(), LibraryPodName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// libraryPod is a Library pod created an hour ago whose last browser
// activity was lastActivity ago.
func libraryPod(lastActivity time.Duration, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              LibraryPodName,
			Namespace:         libraryTestNS,
			UID:               "library-uid",
			CreationTimestamp: metav1.NewTime(libraryTestNow.Add(-time.Hour)),
			Labels:            map[string]string{v1alpha1types.LibraryPodLabel: v1alpha1types.LibraryPodLabelValue},
			Annotations: map[string]string{
				libraryActivityAnnotation: libraryTestNow.Add(-lastActivity).Format(time.RFC3339),
			},
		},
		Status: corev1.PodStatus{
			PodIP:      "10.0.0.9",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func TestLibraryLastActivity(t *testing.T) {
	created := libraryTestNow.Add(-time.Hour)
	for _, tc := range []struct {
		name       string
		annotation string
		want       time.Time
	}{
		{"no browser yet", "", created},
		{"browser after start", libraryTestNow.Add(-time.Minute).Format(time.RFC3339), libraryTestNow.Add(-time.Minute)},
		{"stale annotation from before the pod", created.Add(-time.Hour).Format(time.RFC3339), created},
		{"garbage", "yesterday", created},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)}}
			if tc.annotation != "" {
				pod.Annotations = map[string]string{libraryActivityAnnotation: tc.annotation}
			}
			if got := libraryLastActivity(pod); !got.Equal(tc.want) {
				t.Errorf("libraryLastActivity = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHeroicQueueLen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		store   string
		want    int
		wantErr bool
	}{
		{"no store", "", 0, false},
		{"empty queue", `{"queue":[],"finished":[{"params":{"appName":"a"}}]}`, 0, false},
		{"downloading", `{"queue":[{"params":{"appName":"a"},"type":"install"}],"finished":[]}`, 1, false},
		{"no queue key", `{"finished":[]}`, 0, false},
		// A store caught mid-write must not pass for "nothing downloading".
		{"truncated", `{"queue":[{"par`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := heroicQueueLen([]byte(tc.store))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("heroicQueueLen = %d, %v; want %d, err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// The idle rule: stop only after IdleTimeout without browser traffic AND
// with nothing downloading; when in doubt, keep it up.
func TestLibraryIdleRule(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lastActivity time.Duration
		ready        bool
		steam        string
		heroic       string
		execErr      error
		phase        corev1.PodPhase
		wantStopped  bool
		wantRequeue  time.Duration
		wantShutdown bool
	}{
		{name: "browser active", lastActivity: 14 * time.Minute, ready: true, wantRequeue: time.Minute},
		{name: "idle, nothing downloading", lastActivity: 15 * time.Minute, ready: true, wantStopped: true, wantShutdown: true},
		{name: "idle, finished Heroic downloads only", lastActivity: time.Hour, ready: true,
			heroic: `{"queue":[],"finished":[{}]}`, wantStopped: true, wantShutdown: true},
		{name: "idle, Steam downloading", lastActivity: time.Hour, ready: true,
			steam: "/games/SteamLibrary/steamapps/downloading/570\n", wantRequeue: libraryCheckInterval},
		{name: "idle, Heroic downloading", lastActivity: time.Hour, ready: true,
			heroic: `{"queue":[{}],"finished":[]}`, wantRequeue: libraryCheckInterval},
		{name: "idle, Heroic store unreadable", lastActivity: time.Hour, ready: true,
			heroic: `{"queue":`, wantRequeue: libraryCheckInterval},
		{name: "idle, exec fails", lastActivity: time.Hour, ready: true,
			execErr: errors.New("container not running"), wantRequeue: libraryCheckInterval},
		// Never came up: nothing to ask, nothing to shut down cleanly.
		{name: "idle, never ready", lastActivity: time.Hour, ready: false, wantStopped: true},
		{name: "failed while browser active", lastActivity: time.Minute, phase: corev1.PodFailed, wantStopped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := libraryPod(tc.lastActivity, tc.ready)
			pod.Status.Phase = tc.phase
			f := newLibraryFixture(t, []runtime.Object{pod})
			f.exec.steam, f.exec.heroic, f.exec.err = tc.steam, tc.heroic, tc.execErr

			requeue, err := f.library.reconcileIdle(context.Background(), pod)
			if err != nil {
				t.Fatal(err)
			}

			if stopped := !f.podExists(t); stopped != tc.wantStopped {
				t.Errorf("stopped = %v, want %v", stopped, tc.wantStopped)
			}
			if requeue != tc.wantRequeue {
				t.Errorf("requeue after %v, want %v", requeue, tc.wantRequeue)
			}
			if (f.exec.shutdowns > 0) != tc.wantShutdown {
				t.Errorf("steam -shutdown ran %d times, want ran=%v", f.exec.shutdowns, tc.wantShutdown)
			}
		})
	}
}

// A failed `steam -shutdown` must not keep the pod (and the Steam lock).
func TestLibraryStopDeletesEvenIfShutdownFails(t *testing.T) {
	pod := libraryPod(time.Hour, true)
	f := newLibraryFixture(t, []runtime.Object{pod})
	f.library.Exec = func(_ context.Context, _, _, _ string, command []string) (string, error) {
		if strings.Contains(strings.Join(command, " "), "-shutdown") {
			return "", errors.New("steam still running after 60s")
		}
		return "", nil
	}

	if _, err := f.library.reconcileIdle(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if f.podExists(t) {
		t.Error("Library pod kept after a failed Steam shutdown")
	}
}

func TestLibraryEnsurePodStartsLibrary(t *testing.T) {
	f := newLibraryFixture(t, nil)

	pod, busy, err := f.library.EnsurePod(context.Background())
	if err != nil || busy != "" {
		t.Fatalf("EnsurePod = busy %q, err %v", busy, err)
	}

	if pod.Labels[v1alpha1types.LibraryPodLabel] != v1alpha1types.LibraryPodLabelValue {
		t.Error("Library pod lacks the label moonlight-proxy's busy check selects on")
	}
	if pod.Spec.NodeSelector["kubernetes.io/hostname"] != "talos04.xerktech.com" || len(pod.Spec.Tolerations) != 1 {
		t.Errorf("Library pod not pinned to the session node: %v %v", pod.Spec.NodeSelector, pod.Spec.Tolerations)
	}
	mounts := map[string]string{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		mounts[m.MountPath] = m.Name
	}
	claims := map[string]string{}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.PersistentVolumeClaim != nil:
			claims[v.Name] = v.PersistentVolumeClaim.ClaimName
		case v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory:
			claims[v.Name] = "memory"
		}
	}
	for path, want := range map[string]string{libraryHome: "steam-home", "/games": "games", "/dev/shm": "memory"} {
		if got := claims[mounts[path]]; got != want {
			t.Errorf("%s is backed by %q, want %q", path, got, want)
		}
	}
	var passwordFrom *corev1.SecretKeySelector
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "PASSWORD" && e.ValueFrom != nil {
			passwordFrom = e.ValueFrom.SecretKeyRef
		}
	}
	if passwordFrom == nil || passwordFrom.Name != libraryAuthSecret {
		t.Error("Library pod's Selkies login is not set from the auth secret")
	} else if secret, getErr := f.k8s.CoreV1().Secrets(libraryTestNS).Get(context.Background(), libraryAuthSecret, metav1.GetOptions{}); getErr != nil || len(secret.Data[passwordFrom.Key]) < 32 {
		t.Errorf("auth secret = %v, %v; want a generated password", secret, getErr)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("Library pod gets a service account token")
	}

	// A second visit finds the same pod rather than creating another.
	again, _, err := f.library.EnsurePod(context.Background())
	if err != nil || again.UID != pod.UID {
		t.Errorf("second EnsurePod = %v, %v; want the existing pod", again, err)
	}
}

// The Steam lock, operator side: no Library while a game holds Steam.
func TestLibraryEnsurePodRefusedWhileGameRuns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		k8sObjs []runtime.Object
		dwObjs  []runtime.Object
	}{
		{name: "session", dwObjs: []runtime.Object{&v1alpha1types.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "alex-game-x", Namespace: libraryTestNS},
		}}},
		// Its Session is gone but the pod (and Steam in it) is still stopping.
		{name: "terminating session pod", k8sObjs: []runtime.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "alex-game-x", Namespace: libraryTestNS,
			Labels: map[string]string{v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue},
		}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLibraryFixture(t, tc.k8sObjs, tc.dwObjs...)

			pod, busy, err := f.library.EnsurePod(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if busy == "" || pod != nil {
				t.Errorf("EnsurePod = %v, busy %q; want refused", pod, busy)
			}
			if f.podExists(t) {
				t.Error("Library pod created while a game runs")
			}
		})
	}
}

// A launch that passed moonlight-proxy's check before the Library pod existed
// creates its Session after: the Library must back out.
func TestLibraryEnsurePodBacksOutOfRacingLaunch(t *testing.T) {
	f := newLibraryFixture(t, nil)
	f.k8s.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		_, err := f.dw.DirewolfV1alpha1().Sessions(libraryTestNS).Create(context.Background(),
			&v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: "racer", Namespace: libraryTestNS}}, metav1.CreateOptions{})
		return false, nil, err //nolint:wrapcheck // test reactor
	})

	pod, busy, err := f.library.EnsurePod(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if busy == "" || pod != nil {
		t.Errorf("EnsurePod = %v, busy %q; want backed out", pod, busy)
	}
	if f.podExists(t) {
		t.Error("Library pod left running beside a game session")
	}
}

func (f *libraryFixture) server(t *testing.T, pods ...*corev1.Pod) *LibraryServer {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, p := range pods {
		if err := indexer.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	lister := generic.NewLister[*corev1.Pod](indexer).Namespaced(libraryTestNS)
	return NewLibraryServer(f.library, lister, []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")})
}

// userVisit is what a browser sends on a page load the user asked for.
var userVisit = http.Header{"Sec-Fetch-User": {"?1"}, "Sec-Fetch-Mode": {"navigate"}}

func serveLibrary(s *LibraryServer, peer string, header http.Header) *httptest.ResponseRecorder {
	return serveLibraryRequest(s, http.MethodGet, "/", peer, header)
}

func serveLibraryRequest(s *LibraryServer, method, target, peer string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, http.NoBody)
	req.RemoteAddr = peer
	maps.Copy(req.Header, header)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestLibraryServer(t *testing.T) {
	t.Run("untrusted peer", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		rec := serveLibrary(f.server(t), "10.2.0.5:5555", nil)
		if rec.Code != http.StatusForbidden || f.podExists(t) {
			t.Errorf("HTTP %d, pod created %v; want 403 and no pod", rec.Code, f.podExists(t))
		}
	})
	t.Run("visit starts the Library", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		rec := serveLibrary(f.server(t), "10.1.0.5:5555", userVisit)
		if !f.podExists(t) || !strings.Contains(rec.Body.String(), "Starting") {
			t.Errorf("HTTP %d %q, pod created %v; want the Starting page", rec.Code, rec.Body.String(), f.podExists(t))
		}
	})
	// Selkies reloads its tab (location.reload(), no Sec-Fetch-User) when the
	// stream drops: a tab left open across an idle shutdown would otherwise
	// restart the Library at once and hold the Steam lock indefinitely.
	t.Run("script reload does not start the Library", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		rec := serveLibrary(f.server(t), "10.1.0.5:5555", http.Header{"Sec-Fetch-Mode": {"navigate"}})
		if f.podExists(t) || !strings.Contains(rec.Body.String(), libraryStartPath) {
			t.Errorf("HTTP %d %q, pod created %v; want the stopped page with a Start button", rec.Code, rec.Body.String(), f.podExists(t))
		}
	})
	t.Run("Start button starts the Library", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		rec := serveLibraryRequest(f.server(t), http.MethodPost, libraryStartPath, "10.1.0.5:5555", http.Header{"Sec-Fetch-Site": {"same-origin"}})
		if rec.Code != http.StatusSeeOther || !f.podExists(t) {
			t.Errorf("HTTP %d, pod created %v; want 303 and a pod", rec.Code, f.podExists(t))
		}
	})
	t.Run("GET on the start path is refused", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		rec := serveLibraryRequest(f.server(t), http.MethodGet, libraryStartPath, "10.1.0.5:5555", userVisit)
		if rec.Code != http.StatusForbidden || f.podExists(t) {
			t.Errorf("HTTP %d, pod created %v; want 403 and no pod", rec.Code, f.podExists(t))
		}
	})
	t.Run("cross-site start is refused", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		rec := serveLibraryRequest(f.server(t), http.MethodPost, libraryStartPath, "10.1.0.5:5555", http.Header{"Sec-Fetch-Site": {"cross-site"}})
		if rec.Code != http.StatusForbidden || f.podExists(t) {
			t.Errorf("HTTP %d, pod created %v; want 403 and no pod", rec.Code, f.podExists(t))
		}
	})
	// A tab left open after an idle shutdown keeps reconnecting its
	// WebSocket; that must not restart the Library.
	t.Run("websocket does not start the Library", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		rec := serveLibrary(f.server(t), "10.1.0.5:5555", http.Header{"Upgrade": {"websocket"}})
		if rec.Code != http.StatusServiceUnavailable || f.podExists(t) {
			t.Errorf("HTTP %d, pod created %v; want 503 and no pod", rec.Code, f.podExists(t))
		}
	})
	t.Run("game running", func(t *testing.T) {
		f := newLibraryFixture(t, nil, &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: libraryTestNS}})
		rec := serveLibrary(f.server(t), "10.1.0.5:5555", userVisit)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "one place") || f.podExists(t) {
			t.Errorf("HTTP %d %q; want 409 with the Steam lock message", rec.Code, rec.Body.String())
		}
	})
	t.Run("ready pod is proxied", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		wantPassword, err := f.library.AuthPassword(context.Background())
		if err != nil || wantPassword == "" {
			t.Fatalf("AuthPassword = %q, %v", wantPassword, err)
		}
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Selkies' nginx demands the operator-held password.
			if user, pass, ok := r.BasicAuth(); !ok || user != libraryAuthUser || pass != wantPassword {
				w.Header().Set("WWW-Authenticate", `Basic realm="Login"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("selkies"))
		}))
		defer backend.Close()
		pod := libraryPod(time.Hour, true)
		s := f.server(t, pod)
		// Point the proxy's dialer at the test backend whatever the pod IP.
		transport, ok := s.proxy.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("proxy transport is %T", s.proxy.Transport)
		}
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, backend.Listener.Addr().String())
			if err != nil {
				return nil, err //nolint:wrapcheck // test dialer
			}
			return &activityConn{Conn: conn, touch: s.touch}, nil
		}

		rec := serveLibrary(s, "10.1.0.5:5555", http.Header{"Authorization": {"Basic YWJjOndyb25n"}})
		if rec.Body.String() != "selkies" {
			t.Errorf("HTTP %d %q, want the pod's page", rec.Code, rec.Body.String())
		}
		if s.lastSeen.Load() == 0 {
			t.Error("proxied traffic not counted as activity")
		}

		// A pod started with another password: the stale cache is dropped on
		// the 401 and the next request re-reads the Secret.
		stale := "stale"
		s.password.Store(&stale)
		rec = serveLibrary(s, "10.1.0.5:5555", nil)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("stale password: HTTP %d, WWW-Authenticate %q; want a 401 without a browser prompt", rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
		if rec = serveLibrary(s, "10.1.0.5:5555", nil); rec.Body.String() != "selkies" {
			t.Errorf("after a 401: HTTP %d %q, want the pod's page with the re-read password", rec.Code, rec.Body.String())
		}
	})
}

// The idle decision is made on the informer's copy; a browser that came back
// since (activity already on the pod) must not have Steam killed under it.
func TestLibraryStopRechecksFreshActivity(t *testing.T) {
	stale := libraryPod(time.Hour, true)
	fresh := libraryPod(time.Minute, true)
	f := newLibraryFixture(t, []runtime.Object{fresh})

	requeue, err := f.library.reconcileIdle(context.Background(), stale)
	if err != nil {
		t.Fatal(err)
	}
	if !f.podExists(t) || f.exec.shutdowns != 0 {
		t.Errorf("stopped a Library whose browser came back (shutdowns=%d)", f.exec.shutdowns)
	}
	if requeue <= 0 {
		t.Errorf("requeue = %v, want the rest of the idle timeout", requeue)
	}
}

func TestValidateLibraryGamesPath(t *testing.T) {
	for path, ok := range map[string]bool{
		"/games": true, "/mnt/games": true,
		"games": false, "/": false, "/config": false, "/config/games": false, "/dev/shm": false, "/dev": false,
	} {
		if err := ValidateLibraryGamesPath(path); (err == nil) != ok {
			t.Errorf("ValidateLibraryGamesPath(%q) = %v, want ok=%v", path, err, ok)
		}
	}
}
