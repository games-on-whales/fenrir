package moonlight

import (
	"context"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

func libraryPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace,
		Labels: map[string]string{v1alpha1types.LibraryPodLabel: v1alpha1types.LibraryPodLabelValue},
	}}
}

func TestLibraryBusyCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		pods []*corev1.Pod
		busy bool
	}{
		{"no pods", nil, false},
		{"unrelated pod", []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: testNamespace}}}, false},
		{"library pod", []*corev1.Pod{libraryPod("direwolf-library")}, true},
		{"terminating library pod", []*corev1.Pod{func() *corev1.Pod {
			p := libraryPod("direwolf-library")
			p.DeletionTimestamp = &metav1.Time{}
			p.Finalizers = []string{"test"}
			return p
		}()}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := k8sfake.NewClientset()
			for _, p := range tc.pods {
				if _, err := client.CoreV1().Pods(testNamespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			reason, err := LibraryBusyCheck(client.CoreV1().Pods(testNamespace))(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if (reason != "") != tc.busy {
				t.Errorf("reason = %q, want busy=%v", reason, tc.busy)
			}
		})
	}
}

// The Library started between the pre-create check and the Create: the
// launch must back its Session out and answer busy, not stream alongside it.
func TestLaunchBacksOutWhenBusyAfterCreate(t *testing.T) {
	var calls atomic.Int32
	f := newLaunchFixture(t, &RESTServerOptions{
		MaxConcurrentSessions: -1,
		BusyCheck: func(context.Context) (string, error) {
			if calls.Add(1) == 1 {
				return "", nil
			}
			return libraryBusyMessage, nil
		},
	})

	code, resp := f.launch(t, "bob")

	assertBusy(t, code, resp, libraryBusyMessage)
	f.waitForSessionCount(t, 0)
}
