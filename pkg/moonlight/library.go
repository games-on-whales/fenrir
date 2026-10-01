package moonlight

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// libraryBusyMessage is what a launch gets while the Library is open.
const libraryBusyMessage = "The game library is open. Close it (or wait for it to stop) before launching a game"

// LibraryBusyCheck refuses launches while the operator's Library pod exists,
// terminating or not: the Library and game sessions share one Steam home,
// which has a single writer. It lists from the API server, not an informer,
// so a Library pod created a moment ago counts.
func LibraryBusyCheck(pods corev1client.PodInterface) BusyCheck {
	selector := labels.SelectorFromSet(labels.Set{
		v1alpha1types.LibraryPodLabel: v1alpha1types.LibraryPodLabelValue,
	}).String()
	return func(ctx context.Context) (string, error) {
		list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 1})
		if err != nil {
			return "", fmt.Errorf("failed to list Library pods: %w", err)
		}
		if len(list.Items) > 0 {
			return libraryBusyMessage, nil
		}
		return "", nil
	}
}
