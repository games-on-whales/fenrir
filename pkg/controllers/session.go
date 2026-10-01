package controllers

import (
	"bytes"
	"context"
	"crypto/tls"
	stderrors "errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
	// "github.com/pelletier/go-toml/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	v1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	gatewayv1alpha2 "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/typed/apis/v1alpha2"
)

// unstartedSessionTTL is how long a Session may go without a Wolf session
// before it's reaped. It must outlast moonlight-proxy's --launch-timeout
// (capped below moonlight-qt's 120s): cold starts (image pull,
// wolf boot) set WolfSessionID late, and reaping earlier would kill a launch
// the client is still waiting on.
const unstartedSessionTTL = 3 * time.Minute

var (
	WOLF_IMAGE = func() string {
		if im := os.Getenv("WOLF_IMAGE"); im != "" {
			return im
		}

		return "ghcr.io/games-on-whales/wolf:stable"
	}()
)

// streamPollInterval is how often the operator asks wolf-agent whether an
// attached stream is still up. Nothing else reports a Moonlight disconnect
// (wolf-agent stops Wolf's session on pause, with no Kubernetes access).
const streamPollInterval = 15 * time.Second

// wolfAgentTimeout bounds each call to wolf-agent. The session controller has
// two workers and polls every attached session: without a bound, two hung
// agents would stall every Session's reconcile.
const wolfAgentTimeout = 10 * time.Second

// errSessionEnded is returned by reconcilePod once it has deleted the Session:
// its pod finished, so there is nothing left to reconcile.
var errSessionEnded = stderrors.New("session ended")

// podCreatedCondition is True once the session's pod has been created. The pod
// is never recreated, so a Session with it True and no pod has ended.
const podCreatedCondition = "PodCreated"

// legacyDeploymentCondition marks a Session from before sessions ran in bare
// pods (see expiredReason).
const legacyDeploymentCondition = "DeploymentCreated"

type SessionControllerOptions struct {
	WolfAgentImage string
	// Host ports session port blocks are allocated from.
	SessionPortRange PortRange
	// Node labels session pods are pinned to. Session pods use hostNetwork, so
	// this selects the node whose IP Moonlight clients stream from.
	SessionNodeSelector map[string]string
	// Taints of that node session pods tolerate.
	SessionTolerations []corev1.Toleration
	// How long a session's pod outlives its client's disconnect, so /resume
	// can re-attach to the still-running game. Zero ends the session on
	// disconnect.
	DisconnectGracePeriod time.Duration
}

// Session Controller manages the lifecycle of a streaming session for
// a given game, of a given user.
// If is responsible for:
//   - 1. Setting up port forwards via Gateway API
//   - 2. Setting up service, pods, etc. for session
//   - 3. Polling the pods wolf-agent to find when session is complete, cleaning up
//   - 4. Calling fake-udev to set up the controllers for the game (wolf-agent instead, probably)
//   - 5. Cleaning up all resources when session is complete
//
// Watchers lists of users and games to:
//   - 1. Delete sessions for games that were deleted
type SessionController struct {
	SessionClient   v1alpha1client.SessionInterface
	SessionInformer generic.Informer[*v1alpha1types.Session]

	AppInformer  generic.Informer[*v1alpha1types.App]
	UserInformer generic.Informer[*v1alpha1types.User]

	TCPRouteClient gatewayv1alpha2.TCPRouteInterface
	UDPRouteClient gatewayv1alpha2.UDPRouteInterface

	K8sClient kubernetes.Interface

	ports *portAllocator

	controller    generic.Controller[*v1alpha1types.Session]
	podController generic.Controller[*corev1.Pod]
	SessionControllerOptions
}

// NewSessionController creates a new session controller.
func NewSessionController(
	k8sClient kubernetes.Interface,
	tcpRouteClient gatewayv1alpha2.TCPRouteInterface,
	udpRouteClient gatewayv1alpha2.UDPRouteInterface,
	sessionClient v1alpha1client.SessionInterface,
	sessionInformer generic.Informer[*v1alpha1types.Session],
	appInformer generic.Informer[*v1alpha1types.App],
	userInformer generic.Informer[*v1alpha1types.User],
	podInformer generic.Informer[*corev1.Pod],
	options SessionControllerOptions,
) *SessionController {
	res := &SessionController{
		K8sClient:                k8sClient,
		TCPRouteClient:           tcpRouteClient,
		UDPRouteClient:           udpRouteClient,
		SessionClient:            sessionClient,
		SessionInformer:          sessionInformer,
		AppInformer:              appInformer,
		UserInformer:             userInformer,
		ports:                    newPortAllocator(options.SessionPortRange),
		SessionControllerOptions: options,
	}

	res.controller = generic.NewController(
		sessionInformer,
		res.Reconcile,
		generic.ControllerOptions{
			Name:    "session-controller",
			Workers: 2,
		},
	)

	// A pod change (ready, a container exited) re-reconciles its Session.
	res.podController = generic.NewController(
		podInformer,
		func(_, _ string, newObj *corev1.Pod) error {
			// Load bearing. If we pass nil it will be casted to interface and
			// not be comparable with nil :)
			if newObj == nil {
				return nil
			}
			return res.reconcileDependant(newObj)
		},
		generic.ControllerOptions{
			Name:    "session-controller-pod",
			Workers: 1,
		},
	)

	return res
}

func (c *SessionController) Run(ctx context.Context) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !cache.WaitForCacheSync(sessionCtx.Done(), c.SessionInformer.HasSynced) {
		return fmt.Errorf("failed to sync session informer")
	}

	// Build initial listing of sessions
	sessions, err := c.SessionInformer.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %v", err)
	}

	// Before any reconcile runs, so a new session cannot be handed a block a
	// running pod still listens on.
	c.claimRecordedPorts(sessions)

	go func() {
		defer cancel()
		err := c.podController.Run(sessionCtx)
		if err != nil {
			klog.Errorf("Failed to run pod controller: %v", err)
		}
	}()

	return c.controller.Run(sessionCtx)
}

func (c *SessionController) HasSynced() bool {
	return c.SessionInformer.HasSynced()
}

type K8sObject interface {
	metav1.Object
	runtime.Object
}

func (c *SessionController) reconcileDependant(obj K8sObject) error {
	// If object doesnt have direwolf/user and direwolf/app labels, skip
	if obj.GetLabels() == nil {
		return nil
	}

	if _, ok := obj.GetLabels()["direwolf/user"]; !ok {
		return nil
	}

	if _, ok := obj.GetLabels()["direwolf/app"]; !ok {
		return nil
	}

	klog.Infof("Reconciling dependant %s %s/%s", obj.GetObjectKind().GroupVersionKind().String(), obj.GetNamespace(), obj.GetName())

	// Lookup sessions associated with his object
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Kind == "Session" {
			klog.Infof("Found owner %s/%s", owner.Name, owner.UID)
			c.controller.Enqueue(obj.GetNamespace(), owner.Name)
		}
	}

	return nil
}

func (c *SessionController) Reconcile(namespace, name string, newObj *v1alpha1types.Session) error {
	klog.Infof("Reconciling session %s/%s", namespace, name)
	defer klog.Infof("Finished Reconciling session %s/%s", namespace, name)

	if newObj == nil {
		// Session was deleted. Its pod, and through the pod its generated
		// ResourceClaims, are garbage collected via owner references; the
		// port block is ours to free.
		return c.releaseUnusedPorts()
	} else if reason := c.expiredReason(newObj, time.Now()); reason != "" {
		return c.endSession(context.TODO(), newObj, reason)
	}
	oldStatus := newObj.Status.DeepCopy()
	portsError := c.allocatePorts(context.TODO(), newObj)

	if portsError != nil {
		klog.Errorf("Failed to allocate ports: %s", portsError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "PortsAllocated",
			Status:  metav1.ConditionFalse,
			Reason:  "PortsAllocationFailed",
			Message: portsError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "PortsAllocated",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	// configError := c.reconcileConfigMap(context.TODO(), newObj)
	// if configError != nil {
	// 	klog.Errorf("Failed to reconcile configmap: %s", configError)
	// 	meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
	// 		Type:    "ConfigMapCreated",
	// 		Status:  metav1.ConditionFalse,
	// 		Reason:  "ConfigMapCreationFailed",
	// 		Message: configError.Error(),
	// 	})
	// } else {
	// 	meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
	// 		Type:   "ConfigMapCreated",
	// 		Status: metav1.ConditionTrue,
	// 		Reason: "Success",
	// 	})
	// }

	if pvcError := c.reconcilePVC(context.TODO(), newObj); pvcError != nil {
		klog.Errorf("Failed to reconcile pvc: %s", pvcError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "VolumeCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "PVCAllocationFailed",
			Message: pvcError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "VolumeCreated",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	pod, podError := c.reconcilePod(context.TODO(), newObj)
	switch {
	case stderrors.Is(podError, errSessionEnded):
		return nil
	case podError != nil:
		klog.Errorf("Failed to reconcile pod: %s", podError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    podCreatedCondition,
			Status:  metav1.ConditionFalse,
			Reason:  "PodCreationFailed",
			Message: podError.Error(),
		})
	default:
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   podCreatedCondition,
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	// Gateway not yet supported
	// if gatewayError := c.reconcileGateway(context.TODO(), newObj); gatewayError != nil {
	// 	klog.Errorf("Failed to reconcile gateway: %s", gatewayError)
	// 	meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
	// 		Type:    "RoutesCreated",
	// 		Status:  metav1.ConditionFalse,
	// 		Reason:  "GatewayConfigurationFailed",
	// 		Message: gatewayError.Error(),
	// 	})
	// } else {
	// 	meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
	// 		Type:   "RoutesCreated",
	// 		Status: metav1.ConditionTrue,
	// 		Reason: "Success",
	// 	})
	// }

	streamError := podError
	if streamError == nil {
		streamError = c.reconcileActiveStreams(context.TODO(), newObj, pod)
	}
	switch {
	case stderrors.Is(streamError, errSessionEnded):
		return nil
	case streamError != nil:
		klog.Errorf("Failed to reconcile active streams: %s", streamError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "StreamStarted",
			Status:  metav1.ConditionFalse,
			Reason:  "StreamStartFailed",
			Message: streamError.Error(),
		})
	default:
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "StreamStarted",
			Status: metav1.ConditionTrue,
			Reason: "WaitForPing", //!TOOD: use actual current stream status?
		})
	}

	// Set the new status, if it is changed
	if !reflect.DeepEqual(&newObj.Status, oldStatus) {
		err := c.writeStatus(context.TODO(), newObj, oldStatus)
		// Failed to update status....nothing to do but try again with
		// exponential backoff. Could be API server issue. Depends on response
		// code?
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	if streamError == nil {
		switch {
		case newObj.Status.WolfSessionID != "":
			// Poll for the client going away.
			c.controller.EnqueueAfter(namespace, name, streamPollInterval)
		case newObj.Status.DisconnectedAt != nil:
			// Wake up to end the session once the grace period is over.
			c.controller.EnqueueAfter(namespace, name, time.Until(newObj.Status.DisconnectedAt.Add(c.DisconnectGracePeriod)))
		}
	}

	// Stream setup is inherently retriable: while the pod is starting up it
	// isn't ready and wolf-agent isn't listening yet ("connection
	// refused"), and no informer event is guaranteed to arrive once it
	// becomes ready. Returning nil here used to stall the session as soon as
	// the status stopped changing, until the unstarted-session reaper
	// deleted it and Moonlight timed out. Return the error so the workqueue
	// requeues with backoff and the stream is started as soon as the agent
	// is reachable.
	return streamError
}

// // !TODO: Unused. Part of testing gateway implementation. The final idea is for
// // Direwolf to dynamically set up port forwards / UDPRoutes via Kubernetes
// // Gateway API for RTSP, ENet, Video RTP, Audio RTP.
// func (c *SessionController) reconcileGateway(ctx context.Context, session *v1alpha1types.Session) error {
// 	// 1. Decide the ports this session will use for RTSP, Enet, Video RTP, Audio RTP
// 	// 2. Create TCPRoute for RTSP, UDP routes for Enet, RTP via Gateway API
// 	if !meta.IsStatusConditionPresentAndEqual(session.Status.Conditions, "PortsAllocated", metav1.ConditionTrue) {
// 		return fmt.Errorf("waiting for PortsAllocated")
// 	}

// 	_, err := c.UDPRouteClient.Apply(
// 		ctx,
// 		gatewayv1alpha2ac.UDPRoute(session.Name, session.Namespace).
// 			WithOwnerReferences(metav1ac.OwnerReference().
// 				WithName(session.Name).
// 				WithAPIVersion(v1alpha1.GroupVersion.String()).
// 				WithKind("Session").
// 				WithUID(session.UID).
// 				WithController(true)).
// 			WithLabels(
// 				map[string]string{
// 					"app":           "direwolf-worker",
// 					"direwolf/app":  session.Spec.GameReference.Name,
// 					"direwolf/user": session.Spec.UserReference.Name,
// 				}).
// 			WithSpec(
// 				gatewayv1alpha2ac.UDPRouteSpec().
// 					WithParentRefs(gatewayv1ac.ParentReference().
// 						WithKind("Gateway").
// 						WithGroup("gateway.networking.k8s.io").
// 						WithName(gatewayv1.ObjectName(session.Spec.GatewayReference.Name)).
// 						WithNamespace(gatewayv1.Namespace(session.Spec.GatewayReference.Namespace))).
// 					WithRules(
// 						gatewayv1alpha2ac.UDPRouteRule().
// 							WithName(gatewayv1.SectionName(session.Name)).
// 							WithBackendRefs(
// 								gatewayv1ac.BackendRef().
// 									WithPort(gatewayv1.PortNumber(session.Status.Ports.Control)).
// 									WithKind(gatewayv1.Kind("Service")).
// 									WithName(gatewayv1.ObjectName(session.Namespace)).
// 									WithNamespace(gatewayv1.Namespace(session.Namespace)),
// 								gatewayv1ac.BackendRef().
// 									WithPort(gatewayv1.PortNumber(session.Status.Ports.VideoRTP)).
// 									WithKind(gatewayv1.Kind("Service")).
// 									WithName(gatewayv1.ObjectName(session.Namespace)).
// 									WithNamespace(gatewayv1.Namespace(session.Namespace)),
// 								gatewayv1ac.BackendRef().
// 									WithPort(gatewayv1.PortNumber(session.Status.Ports.AudioRTP)).
// 									WithKind(gatewayv1.Kind("Service")).
// 									WithName(gatewayv1.ObjectName(session.Namespace)).
// 									WithNamespace(gatewayv1.Namespace(session.Namespace)),
// 							),
// 					),
// 			),
// 		metav1.ApplyOptions{
// 			FieldManager: "direwolf-session-controller-udp-route",
// 			Force:        true,
// 		},
// 	)
// 	if err != nil {
// 		return fmt.Errorf("failed to apply udp route: %s", err)
// 	}

// 	_, err = c.TCPRouteClient.Apply(
// 		ctx,
// 		gatewayv1alpha2ac.TCPRoute(session.Name, session.Namespace).
// 			WithOwnerReferences(metav1ac.OwnerReference().
// 				WithName(session.Name).
// 				WithAPIVersion(v1alpha1.GroupVersion.String()).
// 				WithKind("Session").
// 				WithUID(session.UID).
// 				WithController(true)).
// 			WithLabels(
// 				map[string]string{
// 					"app":           "direwolf-worker",
// 					"direwolf/app":  session.Spec.GameReference.Name,
// 					"direwolf/user": session.Spec.UserReference.Name,
// 				}).
// 			WithSpec(
// 				gatewayv1alpha2ac.TCPRouteSpec().
// 					WithParentRefs(gatewayv1ac.ParentReference().
// 						WithKind("Gateway").
// 						WithGroup("gateway.networking.k8s.io").
// 						WithName(gatewayv1.ObjectName(session.Spec.GatewayReference.Name)).
// 						WithNamespace(gatewayv1.Namespace(session.Spec.GatewayReference.Namespace))).
// 					WithRules(
// 						gatewayv1alpha2ac.TCPRouteRule().
// 							WithName(gatewayv1.SectionName(session.Name)).
// 							WithBackendRefs(
// 								gatewayv1ac.BackendRef().
// 									WithPort(gatewayv1.PortNumber(session.Status.Ports.RTSP)).
// 									WithKind(gatewayv1.Kind("Service")).
// 									WithName(gatewayv1.ObjectName(session.Namespace)).
// 									WithNamespace(gatewayv1.Namespace(session.Namespace)),
// 							),
// 					),
// 			),
// 		metav1.ApplyOptions{
// 			FieldManager: "direwolf-session-controller-TCP-route",
// 			Force:        true,
// 		},
// 	)
// 	if err != nil {
// 		return fmt.Errorf("failed to apply TCP route: %s", err)
// 	}

// 	return nil
// }

// mergeResourceRequirements merges a default and an override ResourceRequirements object for sidecars.
// It gives precedence to the values specified in the overrides.
func mergeResourceRequirements(defaults corev1.ResourceRequirements, overrides *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if overrides == nil {
		return defaults
	}

	// Start with a copy of the defaults
	merged := defaults.DeepCopy()

	// Ensure maps are initialized
	if merged.Limits == nil {
		merged.Limits = make(corev1.ResourceList)
	}
	if merged.Requests == nil {
		merged.Requests = make(corev1.ResourceList)
	}

	// Override limits
	for resourceName, quantity := range overrides.Limits {
		merged.Limits[resourceName] = quantity
	}

	// Override requests
	for resourceName, quantity := range overrides.Requests {
		merged.Requests[resourceName] = quantity
	}

	merged.Claims = appendResourceClaims(merged.Claims, overrides.Claims...)

	return *merged
}

// appendResourceClaims appends claims to dst, keeping one entry per claim name:
// server-side apply keys resources.claims by name alone, so duplicates fail the
// apply. When entries for one name differ (whole claim vs. a single request, or
// two requests), the whole claim is kept, as it covers every request.
func appendResourceClaims(dst []corev1.ResourceClaim, claims ...corev1.ResourceClaim) []corev1.ResourceClaim {
	for _, claim := range claims {
		i := slices.IndexFunc(dst, func(c corev1.ResourceClaim) bool { return c.Name == claim.Name })
		switch {
		case i < 0:
			dst = append(dst, claim)
		case dst[i] != claim:
			dst[i] = corev1.ResourceClaim{Name: claim.Name}
		}
	}
	return dst
}

// appResourceClaims returns the DRA claims referenced by the App's containers.
// Wolf gets the same claims so it encodes (NVENC) on the card the game renders on.
func appResourceClaims(containers []corev1.Container) []corev1.ResourceClaim {
	var claims []corev1.ResourceClaim
	for _, c := range containers {
		claims = appendResourceClaims(claims, c.Resources.Claims...)
	}
	return claims
}

// validateAppResources checks if the app's resource requirements are within the user's policy.
// It returns an error if any app request/limit exceeds the user policy.
// If the policy is nil, it allows any resources.
func validateAppResources(appResources corev1.ResourceRequirements, userPolicy *corev1.ResourceRequirements) (corev1.ResourceRequirements, error) {
	// If there's no policy, the app's resources are inherently valid.
	if userPolicy == nil {
		return appResources, nil
	}

	// Validate Limits
	for resourceName, appLimit := range appResources.Limits {
		if userLimit, ok := userPolicy.Limits[resourceName]; ok {
			// Cmp returns 1 if appLimit > userLimit
			if appLimit.Cmp(userLimit) > 0 {
				return corev1.ResourceRequirements{}, fmt.Errorf(
					"app limit for resource %q (%s) exceeds user policy limit (%s)",
					resourceName, appLimit.String(), userLimit.String(),
				)
			}
		}
	}

	// Validate Requests, I'm not sure if this is needed because we could just limit using... limits.
	for resourceName, appRequest := range appResources.Requests {
		if userRequest, ok := userPolicy.Requests[resourceName]; ok {
			// Cmp returns 1 if appRequest > userRequest
			if appRequest.Cmp(userRequest) > 0 {
				return corev1.ResourceRequirements{}, fmt.Errorf(
					"app request for resource %q (%s) exceeds user policy request (%s)",
					resourceName, appRequest.String(), userRequest.String(),
				)
			}
		}
	}

	// All checks passed. The app's requested resources are valid.
	return appResources, nil
}

// validateVolumeMounts checks if all volume mounts in the provided slice
// correspond to a volume defined in the validVolumes map.
func validateVolumeMounts(mounts []corev1.VolumeMount, validVolumes map[string]struct{}, sidecarName string) error {
	for _, vm := range mounts {
		if _, ok := validVolumes[vm.Name]; !ok {
			return fmt.Errorf("validation failed: volumeMount %q in %s sidecar policy refers to a volume that is not defined in the UserSpec.volumes", vm.Name, sidecarName)
		}
	}
	return nil
}

// writeStatus writes session's status, computed from a read whose status was
// oldStatus. On a conflict it re-applies the status to the latest object, but
// only if that object's status is still oldStatus, i.e. only its spec moved
// (e.g. /resume brought new keys). Dropping the write then would lose a
// just-added Wolf session's ID, leaving that stream orphaned in Wolf.
//
// If the status moved too, this read was stale: another reconcile wrote a
// newer status, and copying ours over it would erase what it recorded (the
// current Wolf session ID), so the write is dropped and the error returned.
func (c *SessionController) writeStatus(ctx context.Context, session *v1alpha1types.Session, oldStatus *v1alpha1types.SessionStatus) error {
	toWrite := session
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, updateErr := c.SessionClient.UpdateStatus(ctx, toWrite, metav1.UpdateOptions{
			FieldManager: "session-controller-status",
		})
		if updateErr == nil {
			return nil
		}
		if errors.IsConflict(updateErr) {
			latest, getErr := c.SessionClient.Get(ctx, session.Name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("failed to get session: %w", getErr)
			}
			if !reflect.DeepEqual(&latest.Status, oldStatus) {
				return fmt.Errorf("session %s/%s status changed since it was read, not overwriting it", session.Namespace, session.Name)
			}
			latest.Status = session.Status
			toWrite = latest
		}
		return fmt.Errorf("failed to update session status: %w", updateErr)
	})
	if err != nil {
		return fmt.Errorf("failed to write session status: %w", err)
	}
	return nil
}

// confirmFresh fails unless session is the live object. It guards every
// change to Wolf: the informer can still hold the Session from before our own
// last status write, and acting on that (re-adding or stopping a stream the
// write already superseded) would leave Wolf and the recorded status apart.
func (c *SessionController) confirmFresh(ctx context.Context, session *v1alpha1types.Session) error {
	live, err := c.SessionClient.Get(ctx, session.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get session %s/%s: %w", session.Namespace, session.Name, err)
	}
	if live.ResourceVersion != session.ResourceVersion {
		return fmt.Errorf("session %s/%s changed since it was read (resourceVersion %s, have %s), retrying", session.Namespace, session.Name, live.ResourceVersion, session.ResourceVersion)
	}
	return nil
}

// expiredReason says why session should be deleted without further
// reconciling, or "" if it should not:
//   - its client disconnected longer than the grace period ago;
//   - it never got a stream within unstartedSessionTTL of its creation (a
//     launch that never became ready). A disconnected session is exempt: its grace period
//     governs it.
func (c *SessionController) expiredReason(session *v1alpha1types.Session, now time.Time) string {
	if d := session.Status.DisconnectedAt; d != nil {
		if now.Sub(d.Time) >= c.DisconnectGracePeriod {
			return fmt.Sprintf("client disconnected at %s and did not resume within %s", d.Format(time.RFC3339), c.DisconnectGracePeriod)
		}
		return ""
	}
	if meta.FindStatusCondition(session.Status.Conditions, legacyDeploymentCondition) != nil &&
		meta.FindStatusCondition(session.Status.Conditions, podCreatedCondition) == nil {
		// Its game runs in a Deployment's pod, which this operator neither
		// watches nor talks to; a new pod would only collide with it on the
		// port block. Deleting the Session garbage-collects the Deployment.
		return "started by an operator that ran sessions in Deployments"
	}
	if session.Status.WolfSessionID == "" && session.CreationTimestamp.Add(unstartedSessionTTL).Before(now) {
		return fmt.Sprintf("older than %s and has no wolf session ID", unstartedSessionTTL)
	}
	return ""
}

// reconcilePod ensures the session's pod exists and returns it. The pod runs
// once, like a Job's: restartPolicy Never, and never recreated. Once it has
// finished (a container exited, or it was deleted), the Session is deleted and
// errSessionEnded returned.
func (c *SessionController) reconcilePod(ctx context.Context, session *v1alpha1types.Session) (*corev1.Pod, error) {
	if !meta.IsStatusConditionPresentAndEqual(session.Status.Conditions, "PortsAllocated", metav1.ConditionTrue) {
		return nil, stderrors.New("waiting for PortsAllocated")
	}

	podName := c.podName(session)
	existing, err := c.podController.Informer().Namespaced(session.Namespace).Get(podName)
	switch {
	case err == nil:
		return c.checkPod(ctx, session, existing)
	case !errors.IsNotFound(err):
		return nil, fmt.Errorf("failed to get pod %s/%s: %w", session.Namespace, podName, err)
	case meta.IsStatusConditionTrue(session.Status.Conditions, podCreatedCondition):
		// Created before, so either deleted since or not yet in the cache:
		// ask the API server which.
		live, getErr := c.K8sClient.CoreV1().Pods(session.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if errors.IsNotFound(getErr) {
			return nil, c.endSessionErr(ctx, session, "its pod was deleted")
		} else if getErr != nil {
			return nil, fmt.Errorf("failed to get pod %s/%s: %w", session.Namespace, podName, getErr)
		}
		return c.checkPod(ctx, session, live)
	}

	pod, err := c.buildPod(session)
	if err != nil {
		return nil, err
	}
	// Before the pod, which mounts it.
	if tokenErr := c.reconcileAgentToken(ctx, session); tokenErr != nil {
		return nil, tokenErr
	}
	// Before the pod, which references it by name.
	if claimErr := c.reconcileGPUClaim(ctx, session); claimErr != nil {
		return nil, claimErr
	}
	created, err := c.K8sClient.CoreV1().Pods(session.Namespace).Create(ctx, pod, metav1.CreateOptions{
		FieldManager: "direwolf-session-controller-pod",
	})
	if errors.IsAlreadyExists(err) {
		// Created by an earlier reconcile whose status update was lost.
		live, getErr := c.K8sClient.CoreV1().Pods(session.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if getErr != nil {
			return nil, fmt.Errorf("failed to get pod %s/%s: %w", session.Namespace, podName, getErr)
		}
		return c.checkPod(ctx, session, live)
	} else if err != nil {
		return nil, fmt.Errorf("failed to create pod: %w", err)
	}
	return created, nil
}

// checkPod returns pod if the session can still use it, and otherwise ends the
// session. Nothing restarts a container of a restartPolicy Never pod, so one
// exited container (the game quitting, or a sidecar crashing) ends it.
func (c *SessionController) checkPod(ctx context.Context, session *v1alpha1types.Session, pod *corev1.Pod) (*corev1.Pod, error) {
	if !metav1.IsControlledBy(pod, session) {
		// Left over from an earlier Session of the same name; garbage
		// collection removes it.
		return nil, fmt.Errorf("pod %s/%s belongs to another session, waiting for its deletion", pod.Namespace, pod.Name)
	}
	switch {
	case pod.DeletionTimestamp != nil:
		return nil, c.endSessionErr(ctx, session, "its pod is being deleted")
	case pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed:
		return nil, c.endSessionErr(ctx, session, fmt.Sprintf("its pod %s", pod.Status.Phase))
	case pod.Annotations[portBlockAnnotation] != strconv.Itoa(int(session.Status.Ports.HTTP)):
		// Its block was re-allocated (e.g. lost across an operator restart),
		// so the pod no longer serves the ports the session advertises.
		return nil, c.endSessionErr(ctx, session, fmt.Sprintf("its pod is on port block %s, not %d", pod.Annotations[portBlockAnnotation], session.Status.Ports.HTTP))
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			return nil, c.endSessionErr(ctx, session, fmt.Sprintf("container %s exited (code %d, %s)", cs.Name, t.ExitCode, t.Reason))
		}
	}
	return pod, nil
}

// endSession deletes the Session, which garbage-collects its pod (and the
// pod's generated ResourceClaims) and frees its port block.
func (c *SessionController) endSession(ctx context.Context, session *v1alpha1types.Session, reason string) error {
	klog.Infof("Ending session %s/%s: %s", session.Namespace, session.Name, reason)
	err := c.SessionClient.Delete(ctx, session.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &session.UID},
	})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete session %s/%s: %w", session.Namespace, session.Name, err)
	}
	return nil
}

// endSessionErr is endSession for reconcile steps: errSessionEnded once the
// Session is deleted, so the caller stops reconciling it.
func (c *SessionController) endSessionErr(ctx context.Context, session *v1alpha1types.Session, reason string) error {
	if err := c.endSession(ctx, session, reason); err != nil {
		return err
	}
	return errSessionEnded
}

// buildPod renders the session's pod from its App, User and port block.
func (c *SessionController) buildPod(session *v1alpha1types.Session) (*corev1.Pod, error) {
	// Get the user object to access resource policies
	user, err := c.UserInformer.Namespaced(session.Namespace).Get(session.Spec.UserReference.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get user %s: %w", session.Spec.UserReference.Name, err)
	}

	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get app: %w", err)
	}
	// Prepare environment variables for the wolf container.
	// The GPU is not selected here: it comes only from the App's DRA
	// ResourceClaims (see appResourceClaims), which the DRA driver injects via CDI.
	// TODO: find a better method of injecting env vars / configs into the pod.
	wolfEnvVars := map[string]string{
		"PUID": "1000",
		"PGID": "1000",
		// Wolf's image starts supervisord with `user=root`; the GOW entrypoint
		// gosu's to UNAME first, so any other user exits with "Can't drop
		// privilege as nonroot user" and ends every Session at once.
		"UNAME":                  "root",
		"XDG_RUNTIME_DIR":        "/tmp/.X11-unix",
		"PULSE_SERVER":           "unix:/tmp/.X11-unix/pulse-socket",
		"HOST_APPS_STATE_FOLDER": "/mnt/data/wolf",
		"WOLF_SOCKET_PATH":       "/etc/wolf/wolf.sock",
		// "WOLF_CFG_FILE":          "/etc/wolf/cfg/config.toml", // no longer needed
		// "WOLF_PRIVATE_CERT_FILE": "/mnt/data/wolf/cfg/cert.pem",
		// "WOLF_PRIVATE_KEY_FILE": "/mnt/data/wolf/cfg/key.pem",
		// "WOLF_PULSE_IMAGE":       "ghcr.io/games-on-whales/pulseaudio:master",
		// "WOLF_CFG_FOLDER":        "/etc/wolf/cfg",
		// Keeping those for later
		// "GST_VAAPI_ALL_DRIVERS":      "1",
		// "GST_DEBUG":                  "2",
		// "__GL_SYNC_TO_VBLANK":        "0",
		// "LIBVA_DRIVER_NAME":          "nvidia",
		// "LD_LIBRARY_PATH":            "/usr/local/nvidia/lib:/usr/local/nvidia/lib64:/usr/local/lib",
	}

	// Check if runtime variables are defined in the App spec and override defaults
	if app.Spec.WolfConfig.RuntimeVariables != nil {
		runtimeVars := app.Spec.WolfConfig.RuntimeVariables
		if runtimeVars.LogLevel != "" {
			wolfEnvVars["WOLF_LOG_LEVEL"] = runtimeVars.LogLevel
		}
		if runtimeVars.TimeZone != "" {
			wolfEnvVars["TZ"] = runtimeVars.TimeZone
		}
		if runtimeVars.RenderNode != "" {
			wolfEnvVars["WOLF_RENDER_NODE"] = runtimeVars.RenderNode
		}
	}

	// The pod is on the host network: move every Wolf listener onto the
	// session's port block.
	ports := session.Status.Ports
	for name, port := range map[string]int32{
		"WOLF_HTTP_PORT":       ports.HTTP,
		"WOLF_HTTPS_PORT":      ports.HTTPS,
		"WOLF_RTSP_SETUP_PORT": ports.RTSP,
		"WOLF_CONTROL_PORT":    ports.Control,
		"WOLF_VIDEO_PING_PORT": ports.VideoRTP,
		"WOLF_AUDIO_PING_PORT": ports.AudioRTP,
	} {
		wolfEnvVars[name] = strconv.Itoa(int(port))
	}
	var podToCreate corev1.PodTemplateSpec
	if app.Spec.Template != nil {
		podToCreate.ObjectMeta = app.Spec.Template.ObjectMeta
		podToCreate.Spec = *app.Spec.Template.Spec.DeepCopy()
	}
	if err := addAppGPUClaim(&podToCreate.Spec, app, session); err != nil {
		return nil, err
	}

	if podToCreate.Labels == nil {
		podToCreate.Labels = map[string]string{}
	}

	podToCreate.Labels["app"] = "direwolf-worker"
	podToCreate.Labels["direwolf/app"] = session.Spec.GameReference.Name
	podToCreate.Labels["direwolf/user"] = session.Spec.UserReference.Name
	podToCreate.Labels[v1alpha1types.SessionPodLabel] = v1alpha1types.SessionPodLabelValue

	// if podToCreate.Spec.SecurityContext == nil {
	// 	podToCreate.Spec.SecurityContext = &corev1.PodSecurityContext{}
	// }

	// if podToCreate.Spec.SecurityContext.SeccompProfile == nil {
	// 	podToCreate.Spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{
	// 		Type: corev1.SeccompProfileTypeUnconfined,
	// 	}
	// }

	// if podToCreate.Spec.SecurityContext.AppArmorProfile == nil {
	// 	podToCreate.Spec.SecurityContext.AppArmorProfile = &corev1.AppArmorProfile{
	// 		Type: corev1.AppArmorProfileTypeUnconfined,
	// 	}
	// }

	wolfEnvVarsSlice := make([]corev1.EnvVar, 0, len(wolfEnvVars))
	for k, v := range wolfEnvVars {
		wolfEnvVarsSlice = append(wolfEnvVarsSlice, corev1.EnvVar{Name: k, Value: v})
	}

	// Inject volume mounts into existing containers
	for i := range podToCreate.Spec.Containers {
		podToCreate.Spec.Containers[i].VolumeMounts = append(podToCreate.Spec.Containers[i].VolumeMounts,
			corev1.VolumeMount{
				Name:      "wolf-runtime",
				MountPath: "/tmp/.X11-unix",
			},
		)
		// The per-App home, unless the App mounts its own there: a Steam App
		// mounts the Steam home it shares with the Library, and a second mount
		// at the same path makes the pod invalid ("mountPath must be unique").
		podToCreate.Spec.Containers[i].VolumeMounts = withMountUnlessTaken(podToCreate.Spec.Containers[i].VolumeMounts,
			corev1.VolumeMount{
				Name:      "wolf-data",
				MountPath: AppHomePath,
				SubPath:   fmt.Sprintf("state/%s", app.Name),
			},
		)

		podToCreate.Spec.Containers[i].Env = append(podToCreate.Spec.Containers[i].Env, []corev1.EnvVar{
			// Standard GOW envars
			{Name: "DISPLAY", Value: ":0"},
			// Container must have extra logic to wait for this to be set up
			// unfortunately.
			{Name: "WAYLAND_DISPLAY", Value: "wayland-1"},
			{Name: "TZ", Value: wolfEnvVars["TZ"]},
			{Name: "UNAME", Value: "retro"},
			{Name: "XDG_RUNTIME_DIR", Value: "/tmp/.X11-unix"}, // "UID":             "1000",
			// "GID":             "1000",
			{Name: "PULSE_SERVER", Value: "unix:/tmp/.X11-unix/pulse-socket"},
			// PULSE_SINK & PULSE_SOURCE set at runtime calculated based off session ID.
			// But would be nice if unnecessary

			// Assorted NVIDIA. Unsure if required. Probabky not.
			// just gonna uncomment to make sure that this is not the reason firefox keeps crashing and failing to play videos.
			// yeah now no audio, probably because i'm developing on an integrated amd gpu.
			{Name: "LIBVA_DRIVER_NAME", Value: "nvidia"},
			{Name: "LD_LIBRARY_PATH", Value: "/usr/local/nvidia/lib:/usr/local/nvidia/lib64:/usr/local/lib"},
			{Name: "GST_VAAPI_ALL_DRIVERS", Value: "1"},
			{Name: "GST_DEBUG", Value: "2"},

			// Gamescape envar injection. Ham-handed. Why not.
			{Name: "GAMESCOPE_WIDTH", Value: fmt.Sprint(session.Spec.Config.VideoWidth)},
			{Name: "GAMESCOPE_HEIGHT", Value: fmt.Sprint(session.Spec.Config.VideoHeight)},
			{Name: "GAMESCOPE_REFRESH", Value: fmt.Sprint(session.Spec.Config.VideoRefreshRate)},
		}...)

		// Validate the main app container's resources against the user's policy.
		validatedResources, err := validateAppResources(podToCreate.Spec.Containers[i].Resources, user.Spec.Resources)
		if err != nil {
			// The error will be handled by the main Reconcile loop to update the session status.
			return nil, fmt.Errorf("resource validation for main app container failed: %w", err)
		}
		podToCreate.Spec.Containers[i].Resources = validatedResources
	}

	podToCreate.Spec.InitContainers = append(podToCreate.Spec.InitContainers,
		corev1.Container{
			Name:  "init",
			Image: "ghcr.io/games-on-whales/base:edge",
			// This will need to be updated / removed since /etc/wolf/cfg is no longer used by wolf
			// Also, we're no longer injecting the app info into the config.toml
			Command: []string{
				"sh", "-c", `
				mkdir -p /mnt/data/wolf/cfg
				cp /certs/* /mnt/data/wolf/cfg/
				chown 1000:1000 /mnt/data/wolf
				chmod 777 /mnt/data/wolf
				chown -R 1000:1000 /mnt/data/wolf/cfg
				chmod 777 /mnt/data/wolf/cfg
				chown -R ubuntu:ubuntu /tmp/.X11-unix
				chmod 1777 -R /tmp/.X11-unix
				mkdir -p /etc/wolf/cfg
				# cp -LR /cfg/* /etc/wolf/cfg
				chown -R ubuntu:ubuntu /etc/wolf
				chmod 777 -R /etc/wolf
			`,
			},
			VolumeMounts: []corev1.VolumeMount{
				// {
				// 	Name:      "wolf-tls-secret",
				// 	MountPath: "/certs",
				// 	ReadOnly:  true,
				// },
				{
					Name:      "wolf-cfg",
					MountPath: "/etc/wolf",
				},
				{
					Name:      "wolf-data",
					MountPath: "/mnt/data/wolf",
				},
				{
					Name:      "wolf-runtime",
					MountPath: "/tmp/.X11-unix",
				},
				// {
				// 	Name:      "config",
				// 	MountPath: "/cfg",
				// },
			},
		},
	)

	// Define default resources for sidecars
	wolfAgentDefaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
	}
	pulseAudioDefaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
	}
	wolfDefaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
	}

	// Prepare sidecar policies
	var wolfAgentEnv, pulseAudioEnv, wolfEnv []corev1.EnvVar
	var wolfAgentResources, pulseAudioResources, wolfResources corev1.ResourceRequirements
	var wolfAgentVolumeMounts, pulseAudioVolumeMounts, wolfVolumeMounts []corev1.VolumeMount
	var wolfAgentSecurityContext, pulseAudioSecurityContext, wolfSecurityContext *corev1.SecurityContext
	var podHostIPC bool // Variable to track if HostIPC should be enabled for the pod

	// Set defaults first
	wolfAgentResources = wolfAgentDefaultResources
	pulseAudioResources = pulseAudioDefaultResources
	wolfResources = wolfDefaultResources

	// Create a set of valid volume names for quick lookup
	validVolumes := make(map[string]struct{})
	for _, volume := range user.Spec.Volumes {
		validVolumes[volume.Name] = struct{}{}
	}

	if user.Spec.SidecarPolicies != nil {
		policies := user.Spec.SidecarPolicies
		if policies.WolfAgent != nil {
			if err := validateVolumeMounts(policies.WolfAgent.VolumeMounts, validVolumes, "wolfAgent"); err != nil {
				return nil, err
			}
			if err := validateNoHotplugOverride(policies.WolfAgent.VolumeMounts); err != nil {
				return nil, err
			}
			wolfAgentEnv = policies.WolfAgent.Env
			// klog.debug("User defined env vars: %+v", wolfAgentEnv)
			wolfAgentResources = mergeResourceRequirements(wolfAgentDefaultResources, policies.WolfAgent.Resources)
			wolfAgentVolumeMounts = policies.WolfAgent.VolumeMounts
			wolfAgentSecurityContext = policies.WolfAgent.SecurityContext
			if policies.WolfAgent.HostIPC != nil && *policies.WolfAgent.HostIPC {
				podHostIPC = true
			}
		}
		if policies.PulseAudio != nil {
			if err := validateVolumeMounts(policies.PulseAudio.VolumeMounts, validVolumes, "pulseAudio"); err != nil {
				return nil, err
			}
			pulseAudioEnv = policies.PulseAudio.Env
			// klog.Infof("User defined env vars: %+v", pulseAudioEnv)
			pulseAudioResources = mergeResourceRequirements(pulseAudioDefaultResources, policies.PulseAudio.Resources)
			pulseAudioVolumeMounts = policies.PulseAudio.VolumeMounts
			pulseAudioSecurityContext = policies.PulseAudio.SecurityContext
			if policies.PulseAudio.HostIPC != nil && *policies.PulseAudio.HostIPC {
				podHostIPC = true
			}
		}
		if policies.Wolf != nil {
			if err := validateVolumeMounts(policies.Wolf.VolumeMounts, validVolumes, "wolf"); err != nil {
				return nil, err
			}
			wolfEnv = policies.Wolf.Env
			// klog.Infof("User defined env vars: %+v", wolfEnv)
			wolfResources = mergeResourceRequirements(wolfDefaultResources, policies.Wolf.Resources)
			wolfVolumeMounts = policies.Wolf.VolumeMounts
			wolfSecurityContext = policies.Wolf.SecurityContext
			if policies.Wolf.HostIPC != nil && *policies.Wolf.HostIPC {
				podHostIPC = true
			}
		}
	}

	// Captured before the sidecars are appended: these are the App's containers only.
	wolfResources.Claims = appendResourceClaims(wolfResources.Claims, appResourceClaims(podToCreate.Spec.Containers)...)

	// Apply HostIPC setting to the pod spec if requested by any sidecar policy
	podToCreate.Spec.HostIPC = podHostIPC

	// Session pods share the node IP with moonlight-proxy, since Moonlight can
	// only be redirected to another port. Every listener is on the session's
	// port block and declared as a container port: on the host network those
	// default to hostPorts, so the scheduler holds a pod whose block is still
	// bound by a terminating predecessor instead of letting its bind fail.
	podToCreate.Spec.HostNetwork = true
	if podToCreate.Spec.DNSPolicy == "" {
		podToCreate.Spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}
	if len(c.SessionNodeSelector) > 0 {
		if podToCreate.Spec.NodeSelector == nil {
			podToCreate.Spec.NodeSelector = map[string]string{}
		}
		maps.Copy(podToCreate.Spec.NodeSelector, c.SessionNodeSelector)
	}
	podToCreate.Spec.Tolerations = append(podToCreate.Spec.Tolerations, c.SessionTolerations...)

	// The App's containers only: they see hotplugged controllers.
	for i := range podToCreate.Spec.Containers {
		ctr := &podToCreate.Spec.Containers[i]
		ctr.VolumeMounts = withHotplugMounts(ctr.VolumeMounts)
	}

	podToCreate.Spec.Containers = append(podToCreate.Spec.Containers,
		corev1.Container{
			Name:            "wolf-agent",
			Image:           c.WolfAgentImage,
			ImagePullPolicy: corev1.PullAlways,
			// ImagePullPolicy: corev1.PullIfNotPresent,
			Args: []string{
				"--socket=/etc/wolf/wolf.sock",
				fmt.Sprintf("--port=%d", ports.WolfAgent),
				"--token-file=" + wolfAgentTokenMountPath + "/" + wolfAgentTokenKey,
			},
			Ports: []corev1.ContainerPort{
				{
					Name:          "wa",
					ContainerPort: ports.WolfAgent,
				},
			},
			Env: append([]corev1.EnvVar{
				{
					Name:  "XDG_RUNTIME_DIR",
					Value: "/tmp/.X11-unix",
				},
				// {
				// 	Name:  "PUID",
				// 	Value: "1000",
				// },
				// {
				// 	Name:  "PGID",
				// 	Value: "1000",
				// },
				{
					Name:  "WOLF_SOCKET_PATH",
					Value: "/etc/wolf/wolf.sock",
				},
				{
					Name:  "DIREWOLF_USER",
					Value: session.Spec.UserReference.Name,
				},
				{
					Name:  "DIREWOLF_APP",
					Value: session.Spec.GameReference.Name,
				},
				{
					Name: "POD_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.name",
						},
					},
				},
				{
					Name: "POD_NAMESPACE",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.namespace",
						},
					},
				},
			}, wolfAgentEnv...,
			),
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/readyz",
						Port:   intstr.FromInt32(ports.WolfAgent),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/livez",
						Port:   intstr.FromInt32(ports.WolfAgent),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
			Resources:       wolfAgentResources,
			SecurityContext: wolfAgentSecurityContext,
			VolumeMounts: append([]corev1.VolumeMount{
				{
					Name:      "wolf-cfg",
					MountPath: "/etc/wolf",
				},
				{
					Name:      "wolf-runtime",
					MountPath: "/tmp/.X11-unix",
				},
				{
					Name:      "wolf-agent-token",
					MountPath: wolfAgentTokenMountPath,
					ReadOnly:  true,
				},
			}, withHotplugMounts(wolfAgentVolumeMounts)...),
		},
		corev1.Container{
			Name:  "pulseaudio",
			Image: "ghcr.io/games-on-whales/pulseaudio:edge",
			Env: append([]corev1.EnvVar{
				{Name: "TZ", Value: wolfEnvVars["TZ"]},
				{Name: "UNAME", Value: "retro"},
				{Name: "XDG_RUNTIME_DIR", Value: "/tmp/pulse"},
				// "UID":             "1000",
				// "GID":             "1000",
			}, pulseAudioEnv...),

			Resources:       pulseAudioResources,
			SecurityContext: pulseAudioSecurityContext,
			VolumeMounts: append([]corev1.VolumeMount{
				{
					Name:      "wolf-runtime",
					MountPath: "/tmp/pulse",
				},
			}, pulseAudioVolumeMounts...),
		},
		corev1.Container{
			Name:    "wolf",
			Image:   WOLF_IMAGE,
			Command: wolfCommand,
			Env:     append(wolfEnvVarsSlice, wolfEnv...),
			// Declared so they become hostPorts; see HostNetwork above.
			Ports: []corev1.ContainerPort{
				{Name: "http", ContainerPort: ports.HTTP, Protocol: corev1.ProtocolTCP},
				{Name: "https", ContainerPort: ports.HTTPS, Protocol: corev1.ProtocolTCP},
				{Name: "rtsp", ContainerPort: ports.RTSP, Protocol: corev1.ProtocolTCP},
				{Name: "enet", ContainerPort: ports.Control, Protocol: corev1.ProtocolUDP},
				{Name: "video", ContainerPort: ports.VideoRTP, Protocol: corev1.ProtocolUDP},
				{Name: "audio", ContainerPort: ports.AudioRTP, Protocol: corev1.ProtocolUDP},
			},
			Resources:       wolfResources,
			SecurityContext: wolfSecurityContext,
			VolumeMounts: append([]corev1.VolumeMount{
				{
					Name:      "wolf-cfg",
					MountPath: "/etc/wolf",
				},
				{
					Name:      "wolf-runtime",
					MountPath: "/tmp/.X11-unix",
				},
				{
					Name:      "wolf-data",
					MountPath: "/mnt/data/wolf",
				},
				// {
				// 	Name:      "dev-input",
				// 	MountPath: "/dev/input",
				// },
				// {
				// 	Name:      "dev-uinput",
				// 	MountPath: "/dev/uinput",
				// },
				// {
				// 	Name:      "host-udev",
				// 	MountPath: "/run/udev", //Need to find a more secure way to mount this
				// },
			}, wolfVolumeMounts...),
		},
	)

	var wolfDataVolumeSource corev1.VolumeSource
	if app.Spec.VolumeClaimTemplate != nil {
		wolfDataVolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: c.pvcName(session),
			},
		}
	} else {
		wolfDataVolumeSource = corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}
	}

	podToCreate.Spec.Volumes = append(podToCreate.Spec.Volumes,
		// corev1.Volume{
		// 	Name: "config",
		// 	VolumeSource: corev1.VolumeSource{
		// 		ConfigMap: &corev1.ConfigMapVolumeSource{
		// 			LocalObjectReference: corev1.LocalObjectReference{
		// 				Name: c.deploymentName(session),
		// 			},
		// 		},
		// 	},
		// },
		// corev1.Volume{
		// 	Name: "wolf-tls-secret",
		// 	VolumeSource: corev1.VolumeSource{
		// 		Secret: &corev1.SecretVolumeSource{
		// 			SecretName: "wolf-tls-secret",
		// 			Optional:   ptr.To(true), // Optional so it doesn't crash if you forgot to create it
		// 		},
		// 	},
		// },
		corev1.Volume{
			Name: "wolf-cfg",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name: "wolf-runtime",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name:         "wolf-data",
			VolumeSource: wolfDataVolumeSource,
		},
		corev1.Volume{
			Name:         hotplugDevVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		corev1.Volume{
			Name:         hotplugUdevVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		// Created by reconcileAgentToken before the pod.
		corev1.Volume{
			Name: "wolf-agent-token",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: agentTokenSecretName(session.Name),
				},
			},
		},
		// corev1.Volume{ //Needs to be changed into something more secure, without host path
		// 	Name: "dev-input",
		// 	VolumeSource: corev1.VolumeSource{
		// 		HostPath: &corev1.HostPathVolumeSource{
		// 			Path: "/dev/input",
		// 			Type: ptr.To(corev1.HostPathDirectory),
		// 		},
		// 	},
		// },
		// I'm moving this to volumeConfig
		// corev1.Volume{
		// 	Name: "dev-uinput",
		// 	VolumeSource: corev1.VolumeSource{
		// 		HostPath: &corev1.HostPathVolumeSource{
		// 			Path: "/dev/uinput",
		// 			Type: ptr.To(corev1.HostPathFile),
		// 		},
		// 	},
		// },
		// corev1.Volume{ //Needs to be changed into something more secure, without host path
		// 	Name: "host-udev",
		// 	VolumeSource: corev1.VolumeSource{
		// 		HostPath: &corev1.HostPathVolumeSource{
		// 			Path: "/run/udev",
		// 			Type: ptr.To(corev1.HostPathDirectory),
		// 		},
		// 	},
		// },
	)

	// Add volumes from the user spec
	if len(user.Spec.Volumes) > 0 {
		podToCreate.Spec.Volumes = append(podToCreate.Spec.Volumes, user.Spec.Volumes...)
	}

	// A bare pod, not a Deployment: it runs once and is never restarted, so
	// the session ends with the game (see checkPod).
	podToCreate.Spec.RestartPolicy = corev1.RestartPolicyNever
	annotations := maps.Clone(podToCreate.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[portBlockAnnotation] = strconv.Itoa(int(session.Status.Ports.HTTP))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        c.podName(session),
			Namespace:   session.Namespace,
			Labels:      podToCreate.Labels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "Session",
				Name:       session.Name,
				UID:        session.UID,
				Controller: new(true),
			}},
		},
		Spec: podToCreate.Spec,
	}, nil
}

// func (c *SessionController) reconcileConfigMap(
// 	ctx context.Context,
// 	session *v1alpha1types.Session,
// ) error {
// 	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
// 	if err != nil {
// 		return fmt.Errorf("failed to get app: %s", err)
// 	}

// 	user, err := c.UserInformer.Namespaced(session.Namespace).Get(session.Spec.UserReference.Name)
// 	if err != nil {
// 		return fmt.Errorf("failed to get user: %s", err)
// 	}

// 	wolfConfig, err := GenerateWolfConfig(app)
// 	if err != nil {
// 		return fmt.Errorf("failed to generate wolf config: %s", err)
// 	}
// 	deploymentName := c.deploymentName(session)

// 	_, err = c.K8sClient.CoreV1().
// 		ConfigMaps(session.Namespace).
// 		Apply(
// 			context.Background(),
// 			v1ac.ConfigMap(deploymentName, session.Namespace).
// 				WithLabels(
// 					map[string]string{
// 						"app":           "direwolf-worker",
// 						"direwolf/app":  session.Spec.GameReference.Name,
// 						"direwolf/user": session.Spec.UserReference.Name,
// 					}).
// 				WithOwnerReferences(
// 					metav1ac.OwnerReference().
// 						WithName(app.Name).
// 						WithAPIVersion(v1alpha1.GroupVersion.String()).
// 						WithKind("App").
// 						WithUID(app.UID).
// 						WithController(true),
// 					metav1ac.OwnerReference().
// 						WithName(user.Name).
// 						WithAPIVersion(v1alpha1.GroupVersion.String()).
// 						WithKind("User").
// 						WithUID(user.UID),
// 				).
// 				WithData(map[string]string{
// 					"config.toml": wolfConfig,
// 				}),
// 			metav1.ApplyOptions{
// 				FieldManager: "direwolf-session-controller",
// 			})
// 	if err != nil {
// 		return fmt.Errorf("failed to apply configmap: %s", err)
// 	}
// 	return nil
// }

func (c *SessionController) reconcilePVC(ctx context.Context, session *v1alpha1types.Session) error {
	user, err := c.UserInformer.Namespaced(session.Namespace).Get(session.Spec.UserReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get user: %s", err)
	}
	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get user: %s", err)
	}

	// Check if the user defined a volume claim template. If not, return nil.
	if app.Spec.VolumeClaimTemplate == nil {
		klog.Infof("App %s does not define a VolumeClaimTemplate, skipping PVC creation.", app.Name)
		return nil
	}

	pvcName := c.pvcName(session)
	templateSpec := app.Spec.VolumeClaimTemplate.Spec.DeepCopy()

	// Default Access Mode: RWO
	if len(templateSpec.AccessModes) == 0 {
		templateSpec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}

	// Default Storage: 5Gi
	if templateSpec.Resources.Requests == nil {
		templateSpec.Resources.Requests = make(corev1.ResourceList)
	}
	if _, ok := templateSpec.Resources.Requests[corev1.ResourceStorage]; !ok {
		templateSpec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("5Gi")
	}
	// Note: Default storage class is handled by Kubernetes if StorageClassName is nil.

	// Build the PVC spec apply configuration from the defaulted spec
	pvcSpec := v1ac.PersistentVolumeClaimSpec().
		WithAccessModes(templateSpec.AccessModes...).
		WithResources(v1ac.VolumeResourceRequirements().
			WithLimits(templateSpec.Resources.Limits).
			WithRequests(templateSpec.Resources.Requests))

	if templateSpec.Selector != nil {
		selectorConfig := metav1ac.LabelSelector()
		if len(templateSpec.Selector.MatchLabels) > 0 {
			selectorConfig.WithMatchLabels(templateSpec.Selector.MatchLabels)
		}
		if len(templateSpec.Selector.MatchExpressions) > 0 {
			var expressions []*metav1ac.LabelSelectorRequirementApplyConfiguration
			for _, req := range templateSpec.Selector.MatchExpressions {
				expressions = append(expressions, metav1ac.LabelSelectorRequirement().
					WithKey(req.Key).
					WithOperator(req.Operator).
					WithValues(req.Values...))
			}
			selectorConfig.WithMatchExpressions(expressions...)
		}
		pvcSpec.WithSelector(selectorConfig)
	}
	if templateSpec.StorageClassName != nil {
		pvcSpec.WithStorageClassName(*templateSpec.StorageClassName)
	}
	if templateSpec.VolumeMode != nil {
		pvcSpec.WithVolumeMode(*templateSpec.VolumeMode)
	}
	if templateSpec.DataSource != nil {
		dsConfig := v1ac.TypedLocalObjectReference().
			WithKind(templateSpec.DataSource.Kind).
			WithName(templateSpec.DataSource.Name)
		if templateSpec.DataSource.APIGroup != nil {
			dsConfig.WithAPIGroup(*templateSpec.DataSource.APIGroup)
		}
		pvcSpec.WithDataSource(dsConfig)
	}
	if templateSpec.DataSourceRef != nil {
		dsrConfig := v1ac.TypedObjectReference().
			WithKind(templateSpec.DataSourceRef.Kind).
			WithName(templateSpec.DataSourceRef.Name)
		if templateSpec.DataSourceRef.APIGroup != nil {
			dsrConfig.WithAPIGroup(*templateSpec.DataSourceRef.APIGroup)
		}
		if templateSpec.DataSourceRef.Namespace != nil {
			dsrConfig.WithNamespace(*templateSpec.DataSourceRef.Namespace)
		}
		pvcSpec.WithDataSourceRef(dsrConfig)
	}

	_, err = c.K8sClient.CoreV1().PersistentVolumeClaims(session.Namespace).Apply(
		ctx,
		v1ac.PersistentVolumeClaim(pvcName, session.Namespace).
			WithLabels(map[string]string{
				"app":           "direwolf-worker",
				"direwolf/app":  session.Spec.GameReference.Name,
				"direwolf/user": session.Spec.UserReference.Name,
			}).
			WithOwnerReferences(metav1ac.OwnerReference().
				WithName(user.Name).
				WithAPIVersion(v1alpha1.GroupVersion.String()).
				WithKind("User").
				WithUID(user.UID).
				WithController(true)).
			WithSpec(pvcSpec),
		metav1.ApplyOptions{
			FieldManager: "direwolf-session-controller-pvc",
		},
	)

	if err != nil {
		return fmt.Errorf("failed to apply PVC %s: %w", pvcName, err)
	}

	return nil
}

// pvcName is per user and App, not per Session: the game's state outlives
// its sessions.
func (c *SessionController) pvcName(session *v1alpha1types.Session) string {
	return fmt.Sprintf("%s-%s", session.Spec.UserReference.Name, session.Spec.GameReference.Name)
}

// podName is the session's own: each Session runs exactly one pod.
// Volumes through which wolf-agent hands hotplugged controllers to the App's
// containers: it mknods their /dev/input nodes into one and writes their udev
// database entries into the other (pkg/fakeudev). A device node in an
// emptyDir opens like one in /dev: the device cgroup, granted by
// cmd/nri-input, is what decides.
const (
	hotplugDevVolume  = "direwolf-dev-input"
	hotplugUdevVolume = "direwolf-udev"
)

// validateNoHotplugOverride rejects wolf-agent mounts at or under the hotplug
// paths: wolf-agent would then mknod into (and clear) something other than
// what the App's containers see, such as the host's /dev/input.
//
// It catches misconfiguration, not a hostile User: a User can already mount
// hostPath volumes into sidecars. Paths are compared as strings, so only the
// image's known alias of /run (Alpine's /var/run symlink) is covered too.
func validateNoHotplugOverride(mounts []corev1.VolumeMount) error {
	udevDir := path.Dir(UdevDataPath)
	for _, m := range mounts {
		p := mountTarget(&m)
		for _, reserved := range []string{InputDevPath, udevDir, "/var" + udevDir} {
			if p == reserved || strings.HasPrefix(p, reserved+"/") {
				return fmt.Errorf("validation failed: volumeMount %q in wolfAgent sidecar policy mounts over %s, which the operator reserves for hotplugged devices", m.Name, reserved)
			}
		}
	}
	return nil
}

// mountTarget is where m lands in the container. The apiserver accepts a
// relative mountPath, which the runtime resolves against the container root.
func mountTarget(m *corev1.VolumeMount) string {
	return path.Join("/", m.MountPath)
}

// withHotplugMounts adds the hotplug volume mounts to mounts, except where
// mounts already has something at that path (e.g. an App mounting the host's
// /dev/input itself), which a second mount would collide with.
func withHotplugMounts(mounts []corev1.VolumeMount) []corev1.VolumeMount {
	return withMountUnlessTaken(mounts,
		corev1.VolumeMount{Name: hotplugDevVolume, MountPath: InputDevPath},
		corev1.VolumeMount{Name: hotplugUdevVolume, MountPath: path.Dir(UdevDataPath)},
	)
}

// wolfCommand starts Wolf through the GOW image's own /entrypoint.sh, first
// pointing WOLF_RENDER_NODE at the render node the session's GPU claim put in
// the container. Wolf otherwise defaults to /dev/dri/renderD128, but a DRA
// claim exposes only the allocated card's node, whose minor depends on the
// card (renderD129 for the second one): the encoder then fails with "Failed to
// open drm node /dev/dri/renderD128" and Moonlight gets no video. The Wolf
// image itself sets ENV WOLF_RENDER_NODE=/dev/dri/renderD128, so the test is
// "not a device here", not "unset". A WOLF_RENDER_NODE from the App
// (wolfConfig.runtimeVariables.renderNode) or a User's wolf policy that does
// exist in the container is kept.
var wolfCommand = []string{"/bin/sh", "-c", `if [ ! -c "${WOLF_RENDER_NODE:-}" ]; then
  for n in /dev/dri/renderD*; do
    if [ -c "$n" ]; then export WOLF_RENDER_NODE="$n"; break; fi
  done
fi
exec /entrypoint.sh`}

// AppHomePath is HOME in the GOW app images. The operator mounts the App's own
// state there (wolf-data, state/<app>) unless the App mounts something itself.
const AppHomePath = "/home/retro"

// withMountUnlessTaken appends each of add to mounts unless mounts already
// has something at its path.
func withMountUnlessTaken(mounts []corev1.VolumeMount, add ...corev1.VolumeMount) []corev1.VolumeMount {
	for _, m := range add {
		if !slices.ContainsFunc(mounts, func(o corev1.VolumeMount) bool { return mountTarget(&o) == m.MountPath }) {
			mounts = append(mounts, m)
		}
	}
	return mounts
}

func (c *SessionController) podName(session *v1alpha1types.Session) string {
	return session.Name
}

// allocatePorts records the session's host port block in its status.
func (c *SessionController) allocatePorts(
	ctx context.Context,
	session *v1alpha1types.Session,
) error {
	owner := c.portOwner(session)
	if session.Status.Ports != (v1alpha1types.SessionPorts{}) {
		err := c.ports.Claim(owner, session.Status.Ports)
		if err == nil {
			return nil
		}
		klog.Warningf("Session %s/%s: re-allocating ports: %v", session.Namespace, session.Name, err)
	}

	ports, err := c.ports.Allocate(owner)
	if err != nil {
		return err
	}
	session.Status.Ports = ports
	return nil
}

// claimRecordedPorts re-registers the port blocks recorded in sessions'
// statuses, e.g. after an operator restart.
func (c *SessionController) claimRecordedPorts(sessions []*v1alpha1types.Session) {
	for _, session := range sessions {
		if session.Status.Ports == (v1alpha1types.SessionPorts{}) {
			continue
		}
		if err := c.ports.Claim(c.portOwner(session), session.Status.Ports); err != nil {
			klog.Warningf("Session %s/%s: not restoring port allocation: %v", session.Namespace, session.Name, err)
		}
	}
}

// portOwner keys a session's port block: its pod, namespaced because the
// node's ports are shared by every namespace the operator watches.
func (c *SessionController) portOwner(session *v1alpha1types.Session) string {
	return session.Namespace + "/" + c.podName(session)
}

// releaseUnusedPorts frees the port block of every pod no remaining Session
// refers to. A terminating pod may still bind it: the next pod given the block
// declares the same hostPorts, so the scheduler holds it until the old one is
// gone.
func (c *SessionController) releaseUnusedPorts() error {
	sessions, err := c.SessionInformer.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}
	live := sets.New[string]()
	for _, s := range sessions {
		live.Insert(c.portOwner(s))
	}
	c.ports.Retain(live)
	return nil
}

// reconcileActiveStreams calls out to wolf-agent on the session's pod to keep
// Wolf's session in step with the Session:
//   - attach: add a Wolf session for spec.config (first launch, or /resume
//     after it put the client's new keys there);
//   - detect the client going away (wolf-agent stops Wolf's session when
//     Moonlight disconnects), and record it in status.disconnectedAt, starting
//     the grace period Reconcile ends the session after.
func (c *SessionController) reconcileActiveStreams(
	ctx context.Context,
	session *v1alpha1types.Session,
	pod *corev1.Pod,
) error {
	if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || !podReady(pod) {
		return fmt.Errorf("pod %s/%s not ready (phase %s)", pod.Namespace, pod.Name, pod.Status.Phase)
	}
	// The pod is on the host network, so this is also the node IP Moonlight
	// streams from.
	podIP := pod.Status.PodIP

	token, err := c.agentToken(ctx, session)
	if err != nil {
		return err
	}

	// List all the "sessions".
	// Ensure they match each of our k8s sessions. Hash on AESKey/IV
	// In the future it might make sense to just match on ClientID/ClientCertFingerprint
	// but that is hardcoded for now :)
	wolfclient := wolfapi.NewClient("https://"+net.JoinHostPort(podIP, strconv.Itoa(int(session.Status.Ports.WolfAgent))), &http.Client{
		Timeout: wolfAgentTimeout,
		Transport: &wolfapi.BearerTokenTransport{
			Token: token,
			Base: &http.Transport{
				// wolf-agent serves a self-signed cert. XERK-1320 tracks
				// pinning it; until then the token goes to an unverified peer.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see above
			},
		},
	})
	sessions, err := wolfclient.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %s", err)
	}

	keyIVHash := util.Hash([]byte(session.Spec.Config.AESKey), []byte(session.Spec.Config.AESIV))
	var found bool
	for _, s := range sessions {
		sHash := util.Hash([]byte(s.AESKey), []byte(s.AESIV))
		if bytes.Equal(sHash, keyIVHash) {
			found = true
			break
		}
	}

	status := &session.Status
	// spec.config changed since the last attach: /resume brought new keys.
	resumed := session.Generation > status.AttachedGeneration

	if status.WolfSessionID != "" {
		if found && !resumed {
			// Streaming.
			status.StreamURL = "rtsp://" + net.JoinHostPort(podIP, strconv.Itoa(int(status.Ports.RTSP)))
			return nil
		}
		// The stream we attached is gone: the client disconnected, or
		// resumed before Wolf noticed, in which case its old stream must go.
		if resumed {
			if freshErr := c.confirmFresh(ctx, session); freshErr != nil {
				return freshErr
			}
			if stopErr := wolfclient.StopSession(ctx, status.WolfSessionID); stopErr != nil {
				klog.Warningf("Session %s/%s: stopping superseded Wolf session %s: %v", session.Namespace, session.Name, status.WolfSessionID, stopErr)
			}
		}
		klog.Infof("Session %s/%s: stream %s ended, keeping the pod for %s", session.Namespace, session.Name, status.WolfSessionID, c.DisconnectGracePeriod)
		status.WolfSessionID = ""
		status.StreamURL = ""
		if status.DisconnectedAt == nil {
			status.DisconnectedAt = new(metav1.Now())
		}
	}

	if found {
		// Wolf has a stream for our keys that we never recorded. First rule
		// out a stale cached Session from before our own status write.
		if freshErr := c.confirmFresh(ctx, session); freshErr != nil {
			return freshErr
		}
		// The status update after AddSession was lost. The stream's ID is
		// unknown, so it cannot be stopped or adopted: end the session.
		return c.endSessionErr(ctx, session, "Wolf has an unrecorded stream for it")
	}

	if status.DisconnectedAt != nil && !resumed {
		// Waiting for /resume; Reconcile ends the session after the grace
		// period.
		return nil
	}
	if freshErr := c.confirmFresh(ctx, session); freshErr != nil {
		return freshErr
	}

	// Will need this for later
	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %s", err)
	}

	if !found && app != nil {
		// apps, err := wolfclient.ListApps(ctx)
		// if err != nil {
		// 	return fmt.Errorf("failed to list apps from wolf: %w", err)
		// }

		// Determine the title we expect the app to have
		// expectedTitle := app.Spec.Title
		// if app.Spec.WolfConfig.Title != "" {
		// 	expectedTitle = app.Spec.WolfConfig.Title
		// }

		// var appID string

		// 1. Check if the app already exists
		// for _, loadedApp := range apps {
		// 	if loadedApp.Title == expectedTitle {
		// 		appID = loadedApp.ID
		// 		klog.Infof("App '%s' already exists in Wolf with ID: %s. Skipping creation.", expectedTitle, appID)
		// 		break
		// 	}
		// }

		// 2. If app doesn't exist, create it
		// if appID == "" {
		// 	klog.Infof("App '%s' not found in Wolf. Creating it...", expectedTitle)

		// 	if len(apps) == 0 {
		// 		return fmt.Errorf("no apps found in wolf container to use as template")
		// 	}

		// 	// get the GST pipelines from the first app (supposedly wolf-ui)
		// 	templateApp := apps[0]
		// 	klog.Infof("Using app[0] (%s) as template for pipelines", templateApp.Title)

		// 	// Defaults for booleans if nil
		// 	startAudio := true
		// 	if app.Spec.WolfConfig.StartAudioServer != nil {
		// 		startAudio = *app.Spec.WolfConfig.StartAudioServer
		// 	}
		// 	startCompositor := true
		// 	if app.Spec.WolfConfig.StartVirtualCompositor != nil {
		// 		startCompositor = *app.Spec.WolfConfig.StartVirtualCompositor
		// 	}

		// 	runner := wolfapi.Runner{
		// 		Type:   "process",
		// 		RunCmd: "sh -c \"while :; do echo 'running...'; sleep 10; done\"",
		// 	}
		// 	if app.Spec.WolfConfig.Runner != nil {
		// 		runner.Type = app.Spec.WolfConfig.Runner.Type
		// 		runner.RunCmd = app.Spec.WolfConfig.Runner.RunCommand
		// 	}
		// 	var renderNode string
		// 	if app.Spec.WolfConfig.RuntimeVariables != nil {
		// 		renderNode = app.Spec.WolfConfig.RuntimeVariables.RenderNode
		// 	}

		// 	// Create the App in Wolf
		// 	newApp := wolfapi.App{
		// 		ID:                     app.Name, // Use K8s App Name as ID
		// 		Title:                  expectedTitle,
		// 		SupportHDR:             false, // Default
		// 		StartVirtualCompositor: startCompositor,
		// 		StartAudioServer:       startAudio,
		// 		RenderNode:             renderNode,
		// 		Runner:                 runner,

		// 		// Injected Pipelines
		// 		H264GSTPipeline: templateApp.H264GSTPipeline,
		// 		HEVCGSTPipeline: templateApp.HEVCGSTPipeline,
		// 		AV1GSTPipeline:  templateApp.AV1GSTPipeline,
		// 		OpusGSTPipeline: templateApp.OpusGSTPipeline,
		// 	}
		// 	// no need to create an app
		// 	// klog.Infof("Creating App in Wolf: %+v", newApp)
		// 	// if err := wolfclient.AddApp(ctx, newApp); err != nil {
		// 	// 	return fmt.Errorf("failed to add app to wolf: %w", err)
		// 	// }
		// 	appID = newApp.ID
		// }

		//!TODO: Add the ports into the request for this to support multiple
		// sessions per Gateway.
		//
		// Create the session
		wolfSession, err := wolfSessionFor(session, podIP)
		if err != nil {
			return err
		}
		sessionID, err := wolfclient.AddSession(ctx, wolfSession)

		if err != nil {
			return fmt.Errorf("failed to create session: %s", err)
		}
		status.WolfSessionID = sessionID
		status.AttachedGeneration = session.Generation
		status.DisconnectedAt = nil
	}

	status.StreamURL = "rtsp://" + net.JoinHostPort(podIP, strconv.Itoa(int(status.Ports.RTSP)))
	return nil
}

// wolfSessionFor builds the Wolf AddSession request for session. Wolf streams
// to ClientIP, so it must be the Moonlight client's real address (recorded by
// moonlight-proxy at /launch); there is no usable default. Wolf's stream
// sockets are IPv4-only and it matches peers by exact IP string, so an IPv6
// client could never stream: refuse it here with a clear error instead.
func wolfSessionFor(session *v1alpha1types.Session, podIP string) (wolfapi.Session, error) {
	clientIP, err := netip.ParseAddr(session.Spec.Config.ClientIP)
	if err != nil {
		return wolfapi.Session{}, fmt.Errorf("session %s/%s has no valid spec.config.clientIP %q: %w",
			session.Namespace, session.Name, session.Spec.Config.ClientIP, err)
	}
	clientIP = clientIP.Unmap()
	if !clientIP.Is4() || clientIP.IsUnspecified() {
		return wolfapi.Session{}, fmt.Errorf("session %s/%s: spec.config.clientIP %s is not an IPv4 address; Wolf streams over IPv4 only",
			session.Namespace, session.Name, clientIP)
	}

	return wolfapi.Session{
		VideoWidth:       session.Spec.Config.VideoWidth,
		VideoHeight:      session.Spec.Config.VideoHeight,
		VideoRefreshRate: session.Spec.Config.VideoRefreshRate,
		// AppID:             appID,
		AudioChannelCount: 2, // !TODO: parse from audio info

		ClientIP: clientIP.String(),
		// If this isn't present it crashes
		// so, I'll keep it here until I figure out a way to pass off from moonlight client
		ClientSettings: wolfapi.ClientSettings{
			RunGID:              1000,
			RunUID:              1000,
			ControllersOverride: []string{"XBOX"},
			// XBOX, not Wolf's AUTO: AUTO promotes a gyro-capable client to a
			// PlayStation pad, which needs /dev/uhid, and Talos has no uhid.
			MotionControllerOverride: "XBOX",
			MouseAcceleration:        1.0,
			VScrollAcceleration:      1.0,
			HScrollAcceleration:      1.0,
		},
		AESKey: session.Spec.Config.AESKey,
		AESIV:  session.Spec.Config.AESIV,
		//!TODO: not this. This is the hash of the client cert we are
		// hardcoding into wolf config. Should call pair endpoint to genuinely
		// add it. Though not really needed since user doesnt connect via HTTPS
		// to wolf, we just need a client ID wolf accepts for this specific
		// pairing/client...
		// ClientID:   "4193251087262667199",
		RTSPFakeIP: podIP,
	}, nil
}
