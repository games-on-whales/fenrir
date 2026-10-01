package controllers

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// appGPUClaim is the pod-level claim name for App.spec.gpu. It is attached to
// every App container and, through appResourceClaims, to the wolf sidecar, so
// Wolf encodes on the card the game renders on.
const (
	appGPUClaim             = "direwolf-gpu"
	defaultGPUDeviceClass   = "gpu.nvidia.com"
	gpuClaimMemoryCapacity  = resourcev1.QualifiedName("memory")
	gpuClaimDeviceRequestID = "gpu"
)

const sessionKind = "Session"

func gpuClaimName(sessionName string) string {
	return sessionName + "-gpu"
}

// addAppGPUClaim wires App.spec.gpu's per-Session ResourceClaim into the pod.
func addAppGPUClaim(spec *corev1.PodSpec, app *v1alpha1types.App, session *v1alpha1types.Session) error {
	if app.Spec.GPU == nil {
		return nil
	}
	if m := app.Spec.GPU.Memory; m != nil && m.Sign() <= 0 {
		return fmt.Errorf("app %s: spec.gpu.memory must be positive, got %s", app.Name, m.String())
	}
	if slices.ContainsFunc(spec.ResourceClaims, func(c corev1.PodResourceClaim) bool { return c.Name == appGPUClaim }) {
		return fmt.Errorf("app %s: spec.gpu and a template resourceClaim named %q conflict", app.Name, appGPUClaim)
	}
	spec.ResourceClaims = append(spec.ResourceClaims, corev1.PodResourceClaim{
		Name:              appGPUClaim,
		ResourceClaimName: new(gpuClaimName(session.Name)),
	})
	for i := range spec.Containers {
		spec.Containers[i].Resources.Claims = appendResourceClaims(spec.Containers[i].Resources.Claims,
			corev1.ResourceClaim{Name: appGPUClaim})
	}
	return nil
}

// buildGPUClaim is App.spec.gpu as a ResourceClaim: one device of the class,
// reserving gpu.memory of it, or the whole card when memory is unset (the
// NVIDIA DRA driver's consumable-capacity default).
func buildGPUClaim(app *v1alpha1types.App, session *v1alpha1types.Session) *resourcev1.ResourceClaim {
	request := resourcev1.ExactDeviceRequest{
		DeviceClassName: cmp.Or(app.Spec.GPU.DeviceClassName, defaultGPUDeviceClass),
	}
	if m := app.Spec.GPU.Memory; m != nil {
		request.Capacity = &resourcev1.CapacityRequirements{
			Requests: map[resourcev1.QualifiedName]resource.Quantity{gpuClaimMemoryCapacity: *m},
		}
	}
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gpuClaimName(session.Name),
			Namespace: session.Namespace,
			Labels:    map[string]string{"direwolf/app": app.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1types.GroupVersion.String(),
				Kind:       sessionKind,
				Name:       session.Name,
				UID:        session.UID,
				Controller: new(true),
			}},
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{{Name: gpuClaimDeviceRequestID, Exactly: &request}},
			},
		},
	}
}

// reconcileGPUClaim creates App.spec.gpu's ResourceClaim for session, owned
// by it so it is garbage collected with it (the pod references it by name,
// unlike a template's generated claims). A claim still owned by an earlier
// Session of the same name is waited out, not reused: it may be allocated to
// a card the new App's request doesn't fit.
func (c *SessionController) reconcileGPUClaim(ctx context.Context, session *v1alpha1types.Session) error {
	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}
	if app.Spec.GPU == nil {
		return nil
	}
	claims := c.K8sClient.ResourceV1().ResourceClaims(session.Namespace)
	name := gpuClaimName(session.Name)
	existing, err := claims.Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		if owner := metav1.GetControllerOf(existing); owner != nil && owner.UID == session.UID {
			return nil
		}
		return fmt.Errorf("GPU claim %s/%s belongs to an earlier session, waiting for its deletion", session.Namespace, name)
	case !errors.IsNotFound(err):
		return fmt.Errorf("failed to get GPU claim %s/%s: %w", session.Namespace, name, err)
	}
	_, err = claims.Create(ctx, buildGPUClaim(app, session), metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create GPU claim %s/%s: %w", session.Namespace, name, err)
	}
	return nil
}
