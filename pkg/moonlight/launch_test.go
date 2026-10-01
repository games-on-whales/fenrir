package moonlight

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

type launchFixture struct {
	server  *RESTServer
	client  *fake.Clientset
	created atomic.Int32
}

// newLaunchFixture builds a RESTServer over a fake clientset seeded with one
// App (ID 1) and the given existing sessions. Created sessions get a name and
// a stream URL immediately, standing in for the operator.
func newLaunchFixture(t *testing.T, opts *RESTServerOptions, existing ...*v1alpha1types.Session) *launchFixture {
	t.Helper()

	app := &v1alpha1types.App{
		ObjectMeta: metav1.ObjectMeta{Name: "game", Namespace: testNamespace},
		Spec:       v1alpha1types.AppSpec{ID: 1},
	}

	objs := make([]runtime.Object, 0, len(existing))
	sessionIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, s := range existing {
		objs = append(objs, s)
		if err := sessionIndexer.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	appIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := appIndexer.Add(app); err != nil {
		t.Fatal(err)
	}

	f := &launchFixture{client: fake.NewSimpleClientset(objs...)}
	f.client.PrependReactor("create", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		s, ok := action.(k8stesting.CreateAction).GetObject().(*v1alpha1types.Session)
		if !ok {
			return false, nil, nil
		}
		n := f.created.Add(1)
		s.Name = fmt.Sprintf("%s%d", s.GenerateName, n)
		s.Status.StreamURL = "rtsp://example:48010"
		return false, nil, nil
	})

	emptyIndexer := func() cache.Indexer {
		return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	}
	if opts.LaunchTimeout == 0 {
		opts.LaunchTimeout = 5 * time.Second
	}
	f.server = NewRESTServer(
		nil,
		generic.NewLister[*v1alpha1types.Pairing](emptyIndexer()).Namespaced(testNamespace),
		generic.NewLister[*v1alpha1types.User](emptyIndexer()).Namespaced(testNamespace),
		generic.NewLister[*v1alpha1types.App](appIndexer).Namespaced(testNamespace),
		generic.NewLister[*v1alpha1types.Session](sessionIndexer).Namespaced(testNamespace),
		generic.NewLister[*corev1.Pod](emptyIndexer()).Namespaced(testNamespace),
		f.client.DirewolfV1alpha1().Sessions(testNamespace),
		*opts,
	)
	return f
}

func sessionFor(user string) *v1alpha1types.Session {
	return &v1alpha1types.Session{
		ObjectMeta: metav1.ObjectMeta{
			Name:      user + "-existing",
			Namespace: testNamespace,
			Labels:    map[string]string{"direwolf/user": user},
		},
		Spec: v1alpha1types.SessionSpec{
			UserReference: v1alpha1types.UserReference{Name: user},
		},
	}
}

// launch drives /launch as the given (already authenticated) user and returns
// the HTTP status and the parsed XML root.
func (f *launchFixture) launch(t *testing.T, user string) (int, Response) {
	t.Helper()

	ctx := context.WithValue(context.Background(), userContextKey{}, &v1alpha1types.User{
		ObjectMeta: metav1.ObjectMeta{Name: user, Namespace: testNamespace},
	})
	ctx = context.WithValue(ctx, pairingContextKey{}, &v1alpha1types.Pairing{
		ObjectMeta: metav1.ObjectMeta{Name: user + "-pairing", Namespace: testNamespace},
	})
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/launch?appid=1&rikey=00&rikeyid=1&mode=1920x1080x60", http.NoBody)
	rec := httptest.NewRecorder()
	f.server.launchHandler(rec, req)

	// Errorf, not Fatalf: this also runs on non-test goroutines.
	var resp Response
	if err := xml.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Errorf("unparseable response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

// slowListSessions delays launch-id (cleanup) Lists. It wraps the client
// rather than using a reactor: the fake runs reactors under one lock, which
// would stall every other call too and hide the race under test.
type slowListSessions struct {
	v1alpha1client.SessionInterface
	delay time.Duration
}

func (s slowListSessions) List(ctx context.Context, opts metav1.ListOptions) (*v1alpha1types.SessionList, error) { //nolint:gocritic // interface signature
	if strings.Contains(opts.LabelSelector, launchIDLabel) {
		// Like a real client, give up when ctx does.
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err() //nolint:wrapcheck // as client-go returns it
		}
	}
	return s.SessionInterface.List(ctx, opts) //nolint:wrapcheck // test passthrough
}

func (f *launchFixture) slowCleanupList(delay time.Duration) {
	f.server.SessionClient = slowListSessions{f.server.SessionClient, delay}
}

// waitForSessionCount waits for background cleanup to settle on want.
func (f *launchFixture) waitForSessionCount(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := f.sessionCount(t)
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session count = %d, want %d", n, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *launchFixture) sessionCount(t *testing.T) int {
	t.Helper()
	list, err := f.client.DirewolfV1alpha1().Sessions(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

func assertBusy(t *testing.T, code int, resp Response, wantMessage string) {
	t.Helper()
	// HTTP 200 so moonlight-qt parses the XML and shows the message.
	if code != http.StatusOK {
		t.Errorf("HTTP status = %d, want 200", code)
	}
	if resp.StatusCode != busyStatusCode || resp.StatusMessage != wantMessage {
		t.Errorf("response = %d %q, want %d %q", resp.StatusCode, resp.StatusMessage, busyStatusCode, wantMessage)
	}
}

func TestNewRESTServerDefaults(t *testing.T) {
	s := NewRESTServer(nil, nil, nil, nil, nil, nil, nil, RESTServerOptions{})
	if s.LaunchTimeout != 100*time.Second {
		t.Errorf("LaunchTimeout = %s, want 100s (under moonlight-qt's 120s)", s.LaunchTimeout)
	}
	if s.MaxConcurrentSessions != 1 {
		t.Errorf("MaxConcurrentSessions = %d, want 1", s.MaxConcurrentSessions)
	}
}

func TestLaunchBusyWhenAnotherUserStreams(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{}, sessionFor("alice"))

	code, resp := f.launch(t, "bob")

	assertBusy(t, code, resp, busyMessage)
	if f.created.Load() != 0 {
		t.Errorf("created %d sessions, want none", f.created.Load())
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("session count = %d, want alice's 1 untouched", n)
	}
}

func TestLaunchRelaunchBySameUserIsNotBusy(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{}, sessionFor("alice"))

	code, resp := f.launch(t, "alice")

	if code != http.StatusOK || resp.StatusCode != http.StatusOK {
		t.Fatalf("relaunch = HTTP %d / %d %q, want success", code, resp.StatusCode, resp.StatusMessage)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("session count = %d, want 1 (old replaced by new)", n)
	}
}

func TestLaunchBackToBackBySameUserLeavesOneSession(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{})

	// The informer never sees the first session; the second launch must
	// still replace it.
	f.launch(t, "alice")
	code, resp := f.launch(t, "alice")

	if code != http.StatusOK || resp.StatusCode != http.StatusOK {
		t.Fatalf("relaunch = HTTP %d / %d %q, want success", code, resp.StatusCode, resp.StatusMessage)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("session count = %d, want 1", n)
	}
}

func TestLaunchFreeHostSucceeds(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{})

	code, resp := f.launch(t, "bob")

	if code != http.StatusOK || resp.StatusCode != http.StatusOK {
		t.Fatalf("launch = HTTP %d / %d %q, want success", code, resp.StatusCode, resp.StatusMessage)
	}
	if f.created.Load() != 1 {
		t.Errorf("created %d sessions, want 1", f.created.Load())
	}
}

func TestLaunchLimitAboveOne(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{MaxConcurrentSessions: 2}, sessionFor("alice"))

	if code, resp := f.launch(t, "bob"); resp.StatusCode != http.StatusOK {
		t.Fatalf("second session = HTTP %d / %d %q, want success under limit 2", code, resp.StatusCode, resp.StatusMessage)
	}
	code, resp := f.launch(t, "carol")
	assertBusy(t, code, resp, busyMessage)
}

func TestLaunchUnlimited(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{MaxConcurrentSessions: -1}, sessionFor("alice"), sessionFor("bob"))

	if code, resp := f.launch(t, "carol"); resp.StatusCode != http.StatusOK {
		t.Fatalf("launch = HTTP %d / %d %q, want success with no limit", code, resp.StatusCode, resp.StatusMessage)
	}
}

func TestLaunchBusyCheck(t *testing.T) {
	const reason = "The Library is running"
	f := newLaunchFixture(t, &RESTServerOptions{
		MaxConcurrentSessions: -1,
		BusyCheck: func(context.Context) (string, error) {
			return reason, nil
		},
	})

	code, resp := f.launch(t, "bob")

	assertBusy(t, code, resp, reason)
	if f.created.Load() != 0 {
		t.Errorf("created %d sessions, want none", f.created.Load())
	}
}

func TestLaunchBusyCheckError(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{
		BusyCheck: func(context.Context) (string, error) {
			return "", errors.New("boom")
		},
	})

	code, _ := f.launch(t, "bob")

	// Fail closed: an unknown state must not start a second stream.
	if code != http.StatusInternalServerError {
		t.Errorf("HTTP status = %d, want 500", code)
	}
	if f.created.Load() != 0 {
		t.Errorf("created %d sessions, want none", f.created.Load())
	}
}

func TestLaunchFailureDeletesSession(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{LaunchTimeout: 300 * time.Millisecond})
	// The operator never publishes a stream URL.
	f.client.PrependReactor("get", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		name := get.GetName()
		return true, &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}, nil
	})

	if code, _ := f.launch(t, "alice"); code != http.StatusInternalServerError {
		t.Fatalf("HTTP status = %d, want 500 on timeout", code)
	}
	f.waitForSessionCount(t, 0)
	// And so it doesn't lock out the next user.
	if _, resp := f.launch(t, "bob"); resp.StatusCode == busyStatusCode {
		t.Errorf("bob got busy after alice's launch failed")
	}
}

func TestLaunchFailureAnswersBeforeCleanup(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{LaunchTimeout: 300 * time.Millisecond})
	f.client.PrependReactor("get", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		return true, &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: get.GetName(), Namespace: testNamespace}}, nil
	})
	// The cleanup's List stalls.
	release := make(chan struct{})
	cleanupDone := make(chan struct{})
	f.client.PrependReactor("list", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		list, ok := action.(k8stesting.ListAction)
		if !ok || list.GetListRestrictions().Labels.String() == "" ||
			!strings.Contains(list.GetListRestrictions().Labels.String(), launchIDLabel) {
			return false, nil, nil
		}
		<-release
		defer close(cleanupDone)
		return false, nil, nil
	})

	// Released by timer too, so a regression fails instead of deadlocking.
	var once sync.Once
	unstall := func() { once.Do(func() { close(release) }) }
	timer := time.AfterFunc(3*time.Second, unstall)
	defer timer.Stop()

	start := time.Now()
	code, _ := f.launch(t, "alice")
	elapsed := time.Since(start)
	unstall()
	<-cleanupDone

	// moonlight-qt only accepts the reply once the handler returns.
	if code != http.StatusInternalServerError || elapsed > 2*time.Second {
		t.Errorf("launch = HTTP %d after %s, want 500 without waiting on the cleanup", code, elapsed)
	}
}

func TestLaunchFailureCleanupHoldsSlot(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{LaunchTimeout: 300 * time.Millisecond})
	// Alice's session never becomes ready; bob's does.
	f.client.PrependReactor("get", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || !strings.HasPrefix(get.GetName(), "alice-") {
			return false, nil, nil
		}
		return true, &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: get.GetName(), Namespace: testNamespace}}, nil
	})
	// Alice's cleanup is slow to start deleting.
	f.slowCleanupList(100 * time.Millisecond) // within bob's 300ms budget

	if code, _ := f.launch(t, "alice"); code != http.StatusInternalServerError {
		t.Fatalf("alice HTTP status = %d, want 500", code)
	}
	// Bob launches right away: he must queue behind the cleanup rather than
	// count alice's orphan.
	if code, resp := f.launch(t, "bob"); resp.StatusCode != http.StatusOK {
		t.Errorf("bob = HTTP %d / %d %q, want success after alice's cleanup", code, resp.StatusCode, resp.StatusMessage)
	}
}

func TestLaunchFailureCleanupQueuesForBusySlot(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	f := newLaunchFixture(t, &RESTServerOptions{
		LaunchTimeout: 400 * time.Millisecond,
		BusyCheck: func(context.Context) (string, error) {
			// Call 2 is carol's: she holds the slot while alice fails.
			if calls.Add(1) == 2 {
				<-release
				return "held", nil
			}
			return "", nil
		},
	})
	f.client.PrependReactor("get", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || !strings.HasPrefix(get.GetName(), "alice-") {
			return false, nil, nil
		}
		return true, &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: get.GetName(), Namespace: testNamespace}}, nil
	})
	f.slowCleanupList(50 * time.Millisecond)

	aliceDone := make(chan int, 1)
	go func() {
		code, _ := f.launch(t, "alice")
		aliceDone <- code
	}()
	time.Sleep(100 * time.Millisecond) // alice is in her readiness wait
	carolDone := make(chan struct{})
	go func() {
		defer close(carolDone)
		f.launch(t, "carol")
	}()
	if code := <-aliceDone; code != http.StatusInternalServerError {
		t.Fatalf("alice HTTP status = %d, want 500", code)
	}

	// Bob queues behind alice's cleanup, which queued behind carol.
	bobDone := make(chan Response, 1)
	go func() {
		_, resp := f.launch(t, "bob")
		bobDone <- resp
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-carolDone

	if resp := <-bobDone; resp.StatusCode != http.StatusOK {
		t.Errorf("bob = %d %q, want success after alice's cleanup", resp.StatusCode, resp.StatusMessage)
	}
}

func TestLaunchQueuedBeforeFailureStillDrainsCleanup(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	f := newLaunchFixture(t, &RESTServerOptions{
		LaunchTimeout: time.Second,
		BusyCheck: func(context.Context) (string, error) {
			// Call 2 is carol's: she holds the slot while alice fails.
			if calls.Add(1) == 2 {
				<-release
				return "held", nil
			}
			return "", nil
		},
	})
	f.client.PrependReactor("get", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || !strings.HasPrefix(get.GetName(), "alice-") {
			return false, nil, nil
		}
		return true, &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: get.GetName(), Namespace: testNamespace}}, nil
	})

	aliceDone := make(chan int, 1)
	go func() {
		code, _ := f.launch(t, "alice")
		aliceDone <- code
	}()
	time.Sleep(100 * time.Millisecond) // alice is in her readiness wait
	carolDone := make(chan struct{})
	go func() {
		defer close(carolDone)
		f.launch(t, "carol")
	}()
	time.Sleep(50 * time.Millisecond)
	// Bob queues for the slot before alice fails, so he's ahead of her
	// cleanup goroutine: only the drain in createSession can save him.
	bobDone := make(chan Response, 1)
	go func() {
		_, resp := f.launch(t, "bob")
		bobDone <- resp
	}()
	if code := <-aliceDone; code != http.StatusInternalServerError {
		t.Fatalf("alice HTTP status = %d, want 500", code)
	}
	close(release)
	<-carolDone

	if resp := <-bobDone; resp.StatusCode != http.StatusOK {
		t.Errorf("bob = %d %q, want success: alice's orphan must be drained before counting", resp.StatusCode, resp.StatusMessage)
	}
}

func TestLaunchDrainStopsAtCallerDeadline(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{})
	f.slowCleanupList(200 * time.Millisecond)
	for i := range 5 {
		f.server.queueCleanup(fmt.Sprintf("stale%d", i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	f.server.drainCleanups(ctx)

	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Errorf("drain took %s, want it to stop at the caller's 300ms deadline", elapsed)
	}
	f.server.cleanupMu.Lock()
	left := f.server.pendingCleanups
	f.server.cleanupMu.Unlock()
	// stale0 finished at 200ms; stale1 was in flight at the deadline and
	// must be kept along with the rest.
	if want := []string{"stale1", "stale2", "stale3", "stale4"}; !slices.Equal(left, want) {
		t.Errorf("left queued = %v, want %v", left, want)
	}
}

func TestLaunchStalledCleanupsDontOutlastLaunch(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{LaunchTimeout: time.Second})
	f.slowCleanupList(10 * time.Second)
	for i := range 3 {
		f.server.queueCleanup(fmt.Sprintf("stale%d", i))
	}

	start := time.Now()
	f.launch(t, "bob")

	// The drain in createSession shares bob's budget; detached, each stalled
	// cleanup would take its own time.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("launch took %s, want an answer within its 1s budget", elapsed)
	}
}

func TestLaunchCountsUsersNotSessions(t *testing.T) {
	second := sessionFor("alice")
	second.Name = "alice-second"
	f := newLaunchFixture(t, &RESTServerOptions{MaxConcurrentSessions: 2}, sessionFor("alice"), second)

	if code, resp := f.launch(t, "bob"); resp.StatusCode != http.StatusOK {
		t.Fatalf("launch = HTTP %d / %d %q, want success: alice is one user", code, resp.StatusCode, resp.StatusMessage)
	}
}

func TestLaunchWaiterHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	f := newLaunchFixture(t, &RESTServerOptions{
		BusyCheck: func(context.Context) (string, error) {
			select {
			case entered <- struct{}{}:
				<-release
			default:
			}
			return "", nil
		},
	})
	aliceDone := make(chan struct{})
	go func() {
		defer close(aliceDone)
		f.launch(t, "alice")
	}()
	<-entered
	// Let alice finish before the test returns; she reports through t.
	defer func() {
		close(release)
		<-aliceDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := f.server.createSession(ctx, &v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: "bob"}}, func(context.Context) (*v1alpha1types.Session, error) {
			return nil, nil
		}, func() {})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Errorf("createSession succeeded while the slot was held")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter ignored its context and stayed blocked on the launch slot")
	}
}

func TestLaunchSlotWorkIsBounded(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	f := newLaunchFixture(t, &RESTServerOptions{
		BusyCheck: func(ctx context.Context) (string, error) {
			deadline, hasDeadline = ctx.Deadline()
			return "", nil
		},
	})

	// httptest requests have no deadline of their own.
	f.launch(t, "alice")

	if !hasDeadline || time.Until(deadline) > 30*time.Second {
		t.Errorf("work under the launch slot has deadline %v (set=%v), want within 30s", deadline, hasDeadline)
	}
}

func TestLaunchDeadlineCoversWholeLaunch(t *testing.T) {
	var deadline time.Time
	f := newLaunchFixture(t, &RESTServerOptions{
		LaunchTimeout: 2 * time.Second,
		BusyCheck: func(ctx context.Context) (string, error) {
			deadline, _ = ctx.Deadline()
			return "", nil
		},
	})

	f.launch(t, "alice")

	// The slot work shares the launch's own budget rather than adding to it,
	// so slot time + readiness wait can't outlast the client.
	if time.Until(deadline) > 2*time.Second {
		t.Errorf("slot deadline %v is beyond the 2s launch timeout", deadline)
	}
	if DefaultLaunchTimeout >= ClientLaunchTimeout {
		t.Errorf("DefaultLaunchTimeout %s must be under ClientLaunchTimeout %s", DefaultLaunchTimeout, ClientLaunchTimeout)
	}
}

func TestLaunchCreateErrorDeletesStoredSession(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{})
	entered := make(chan struct{})
	release := make(chan struct{})
	// The API server stores the Session but the response is lost.
	f.client.PrependReactor("create", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		obj, ok := create.GetObject().(*v1alpha1types.Session)
		if !ok || obj.Labels["direwolf/user"] != "alice" {
			return false, nil, nil
		}
		obj.Name = obj.GenerateName + "stored"
		if err := f.client.Tracker().Create(action.GetResource(), obj, action.GetNamespace()); err != nil {
			return true, nil, fmt.Errorf("storing session: %w", err)
		}
		entered <- struct{}{}
		<-release
		return true, nil, errors.New("response timed out")
	})

	aliceCode := make(chan int, 1)
	go func() {
		code, _ := f.launch(t, "alice")
		aliceCode <- code
	}()
	<-entered

	// Bob queues on the launch slot behind alice's doomed Create.
	type result struct {
		code int
		resp Response
	}
	bobResult := make(chan result, 1)
	go func() {
		code, resp := f.launch(t, "bob")
		bobResult <- result{code, resp}
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)

	if code := <-aliceCode; code != http.StatusInternalServerError {
		t.Errorf("alice HTTP status = %d, want 500", code)
	}
	bob := <-bobResult
	if bob.resp.StatusCode != http.StatusOK {
		t.Errorf("bob = HTTP %d / %d %q, want success: alice's stored session must be gone first", bob.code, bob.resp.StatusCode, bob.resp.StatusMessage)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("session count = %d, want only bob's", n)
	}
}

func TestLaunchReadinessWaitSharesLaunchDeadline(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{
		LaunchTimeout: time.Second,
		BusyCheck: func(context.Context) (string, error) {
			time.Sleep(700 * time.Millisecond)
			return "", nil
		},
	})
	// The operator never publishes a stream URL.
	f.client.PrependReactor("get", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		name := get.GetName()
		return true, &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}, nil
	})

	start := time.Now()
	f.launch(t, "alice")

	// 0.7s in the slot + a fresh 1s wait would be ~1.7s.
	if elapsed := time.Since(start); elapsed > 1400*time.Millisecond {
		t.Errorf("launch took %s, want the whole launch within its 1s budget", elapsed)
	}
}

func TestLaunchConcurrentUsersRespectLimit(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{})

	const users = 8
	var wg sync.WaitGroup
	var ok, busy atomic.Int32
	for i := range users {
		wg.Go(func() {
			_, resp := f.launch(t, fmt.Sprintf("user%d", i))
			switch resp.StatusCode {
			case http.StatusOK:
				ok.Add(1)
			case busyStatusCode:
				busy.Add(1)
			}
		})
	}
	wg.Wait()

	if ok.Load() != 1 || busy.Load() != users-1 {
		t.Errorf("ok=%d busy=%d, want 1 and %d", ok.Load(), busy.Load(), users-1)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("session count = %d, want 1", n)
	}
}
