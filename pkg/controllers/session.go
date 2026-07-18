package controllers

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"time"

	// "github.com/pelletier/go-toml/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	v1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/typed/apis/v1alpha2"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// TODO: move them somewhere else
const (
	AppLabelKey       string = "direwolf/app"
	ProfileLabelKey   string = "direwolf/profile"
	UserLabelKey      string = "direwolf/user"
	LobbyLabelKey     string = "direwolf/lobby"
	NodeLabelKey      string = "direwolf/node"
	SessionLabelKey   string = "direwolf/session"
	LobbyTypeLabelKey string = "direwolf/lobby-type"
)

var (
	wolfImage = func() string {
		if im := os.Getenv("WOLF_IMAGE"); im != "" {
			return im
		}

		return "ghcr.io/games-on-whales/wolf:stable"
	}()
)

type profileGame struct {
	Profile string
	Game    string
}

type SessionControllerOptions struct {
	WolfAgentImage           string
	WolfAgentImagePullPolicy string // for debug / local testing / slow internet connections
	LBSharingKey             string
}

// SessionController manages the lifecycle of a streaming session for
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
	SessionInformer generic.Informer[*direwolfv1alpha1.Session]

	AppInformer     generic.Informer[*direwolfv1alpha1.App]
	ProfileInformer generic.Informer[*direwolfv1alpha1.Profile]

	TCPRouteClient gatewayv1alpha2.TCPRouteInterface
	UDPRouteClient gatewayv1alpha2.UDPRouteInterface

	K8sClient kubernetes.Interface

	trackedSessions map[profileGame]sets.Set[string]
	trackedGames    map[string]profileGame

	controller           generic.Controller[*direwolfv1alpha1.Session]
	deploymentController generic.Controller[*appsv1.Deployment]
	SessionControllerOptions
}

// NewSessionController creates a new session controller.
func NewSessionController(
	k8sClient kubernetes.Interface,
	tcpRouteClient gatewayv1alpha2.TCPRouteInterface,
	udpRouteClient gatewayv1alpha2.UDPRouteInterface,
	sessionClient v1alpha1client.SessionInterface,
	sessionInformer generic.Informer[*direwolfv1alpha1.Session],
	appInformer generic.Informer[*direwolfv1alpha1.App],
	profileInformer generic.Informer[*direwolfv1alpha1.Profile],
	deploymentInformer generic.Informer[*appsv1.Deployment],
	options SessionControllerOptions,
) *SessionController {
	res := &SessionController{
		K8sClient:                k8sClient,
		TCPRouteClient:           tcpRouteClient,
		UDPRouteClient:           udpRouteClient,
		SessionClient:            sessionClient,
		SessionInformer:          sessionInformer,
		AppInformer:              appInformer,
		ProfileInformer:          profileInformer,
		trackedSessions:          make(map[profileGame]sets.Set[string]),
		trackedGames:             make(map[string]profileGame),
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

	//!TODO: Also watch any udproutes, services, deployments, etc. that we create
	// and re-reconcile their sessions when they change.
	res.deploymentController = generic.NewController(
		deploymentInformer,
		func(_, _ string, newObj *appsv1.Deployment) error {
			// Load bearing. If we pass nil it will be casted to interface and
			// not be comparable with nil :)
			if newObj == nil {
				return nil
			}
			return res.reconcileDependant(newObj)
		},
		generic.ControllerOptions{
			Name:    "session-controller-deployment",
			Workers: 1,
		},
	)

	return res
}

func (c *SessionController) Run(ctx context.Context) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !cache.WaitForCacheSync(sessionCtx.Done(), c.SessionInformer.HasSynced) {
		return errors.New("failed to sync session informer")
	}

	// Build initial listing of sessions
	sessions, err := c.SessionInformer.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	for _, session := range sessions {
		pg := profileGame{
			Game:    session.Spec.GameReference.Name,
			Profile: session.Spec.ProfileReference.Name,
		}
		if existing, ok := c.trackedSessions[pg]; ok {
			existing.Insert(session.Name)
		} else {
			c.trackedSessions[pg] = sets.New(session.Name)
		}

		c.trackedGames[session.Name] = pg
	}

	go func() {
		defer cancel()
		err := c.deploymentController.Run(sessionCtx)
		if err != nil {
			klog.Errorf("Failed to run deployment controller: %v", err)
		}
	}()

	// Wrap the error from the interface method
	if err := c.controller.Run(sessionCtx); err != nil {
		return fmt.Errorf("failed to run session controller: %w", err)
	}
	return nil
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

	if _, ok := obj.GetLabels()[ProfileLabelKey]; !ok {
		return nil
	}

	if _, ok := obj.GetLabels()[AppLabelKey]; !ok {
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

func (c *SessionController) Reconcile(namespace, name string, newObj *direwolfv1alpha1.Session) error {
	klog.Infof("Reconciling session %s/%s", namespace, name)
	defer klog.Infof("Finished Reconciling session %s/%s", namespace, name)

	if newObj == nil {
		// Session was deleted. Stuff will be garbage collected by Kubernetes
		// due to owner references. Nothing to do.
		if gam, ok := c.trackedGames[name]; ok {
			if existing, ok := c.trackedSessions[gam]; ok {
				existing.Delete(name)
				if existing.Len() == 0 {
					delete(c.trackedSessions, gam)
				}
			}

			delete(c.trackedGames, name)
		}
		return nil
	} else if newObj.Status.WolfSessionID == "" && newObj.CreationTimestamp.Add(1*time.Minute).Before(time.Now()) {
		klog.Infof("Session %s/%s is older than 1 minute and has no wolf session ID, deleting", newObj.Namespace, newObj.Name)
		err := c.SessionClient.Delete(context.TODO(), newObj.Name, metav1.DeleteOptions{})
		if err != nil && !kerrors.IsNotFound(err) {
			klog.Errorf("Failed to delete session %s/%s: %v", newObj.Namespace, newObj.Name, err)
			return fmt.Errorf("session wasn't deleted: %w", err)
		}
		return nil
	}
	pg := profileGame{
		Game:    newObj.Spec.GameReference.Name,
		Profile: newObj.Spec.ProfileReference.Name,
	}

	if existing, ok := c.trackedSessions[pg]; ok {
		existing.Insert(newObj.Name)
	} else {
		c.trackedSessions[pg] = sets.New(newObj.Name)
	}
	c.trackedGames[newObj.Name] = pg
	oldStatus := newObj.Status.DeepCopy()
	//TODO: this needs a rewrite
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

	if podError := c.reconcilePod(context.TODO(), newObj); podError != nil {
		klog.Errorf("Failed to reconcile pod: %s", podError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "DeploymentCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "PodCreationFailed",
			Message: podError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "DeploymentCreated",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	if serviceError := c.reconcileService(context.TODO(), newObj); serviceError != nil {
		klog.Errorf("Failed to reconcile service: %s", serviceError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "ServiceCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "ServiceCreationFailed",
			Message: serviceError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "ServiceCreated",
			Status: metav1.ConditionTrue,
			Reason: "ServiceCreated",
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

	if streamError := c.reconcileActiveStreams(context.TODO(), newObj); streamError != nil {
		klog.Errorf("Failed to reconcile active streams: %s", streamError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "StreamStarted",
			Status:  metav1.ConditionFalse,
			Reason:  "StreamStartFailed",
			Message: streamError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "StreamStarted",
			Status: metav1.ConditionTrue,
			Reason: "WaitForPing", //!TOOD: use actual current stream status?
		})
	}

	// Set the new status, if it is changed
	if !reflect.DeepEqual(newObj.Status, oldStatus) {
		_, err := c.SessionClient.UpdateStatus(
			context.TODO(),
			newObj,
			metav1.UpdateOptions{
				FieldManager: "session-controller-status",
			},
		)

		// Failed to update status....nothing to do but try again with
		// exponential backoff. Could be API server issue. Depends on response
		// code?
		if err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("failed to Update status: %w", err)
		}
	}

	//!TODO: figure our retry logic. Some of these errors surely are retriable
	return nil
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
// 					AppLabelKey:  session.Spec.GameReference.Name,
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
// 					AppLabelKey:  session.Spec.GameReference.Name,
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

func (c *SessionController) reconcileService(ctx context.Context, session *direwolfv1alpha1.Session) error {
	if !meta.IsStatusConditionPresentAndEqual(session.Status.Conditions, "PortsAllocated", metav1.ConditionTrue) {
		return errors.New("waiting for PortsAllocated")
	}

	clampString := func(s string, maxLength int) string {
		if len(s) > maxLength {
			return s[:maxLength]
		}
		return s
	}

	session.Status.ServiceName = clampString(session.Name, 56) + "-rtp"

	// HACK: Delete all direwolf-worker services that dont match the service name
	// This is until we can control the ports in wolf
	allServices, err := c.K8sClient.CoreV1().Services(session.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=direwolf-worker", //nolint
	})

	if err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	for _, svc := range allServices.Items {
		if svc.Name != session.Status.ServiceName {
			klog.Infof("Deleting service %s/%s", svc.Namespace, svc.Name)
			err := c.K8sClient.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("Failed to delete service %s/%s: %s", svc.Namespace, svc.Name, err)
				return fmt.Errorf("failed to delete service %s/%s: %w", svc.Namespace, svc.Name, err)
			}
		}
	}

	// 1. Use the set up a service with correct ports pointing to the pods
	_, err = c.K8sClient.CoreV1().
		Services(session.Namespace).
		Apply(
			ctx,
			v1ac.Service(session.Status.ServiceName, session.Namespace).
				WithAnnotations(map[string]string{
					// Try to support popular service LoadBalancer implementation
					// sharing key annotations.
					"lbipam.cilium.io/sharing-key":        c.LBSharingKey,
					"metallb.universe.tf/allow-shared-ip": c.LBSharingKey,
				}).
				WithLabels(
					map[string]string{
						"app":           "direwolf-worker", //nolint
						AppLabelKey:     session.Spec.GameReference.Name,
						ProfileLabelKey: session.Spec.ProfileReference.Name,
					},
				).
				WithOwnerReferences(metav1ac.OwnerReference().
					WithName(session.Name).
					WithAPIVersion(direwolfv1alpha1.GroupVersion.String()).
					WithKind("Session").
					WithUID(session.UID).
					WithController(true)).
				WithSpec(
					v1ac.ServiceSpec().
						WithType(corev1.ServiceTypeLoadBalancer).
						WithSelector(
							map[string]string{
								AppLabelKey:     session.Spec.GameReference.Name,
								ProfileLabelKey: session.Spec.ProfileReference.Name,
							}).
						WithPorts(
							v1ac.ServicePort().
								WithName("wa"). // wolf-agent
								WithPort(8443),
							v1ac.ServicePort().
								WithName("rtsp"). // moonlight-rtsp
								WithPort(session.Status.Ports.RTSP),
							v1ac.ServicePort().
								WithName("enet"). // moonlight-enet
								WithProtocol(corev1.ProtocolUDP).
								WithPort(session.Status.Ports.Control),
							v1ac.ServicePort().
								WithName("video"). // moonlight-video
								WithProtocol(corev1.ProtocolUDP).
								WithPort(session.Status.Ports.VideoRTP),
							v1ac.ServicePort().
								WithName("audio"). // moonlight-audio
								WithProtocol(corev1.ProtocolUDP).
								WithPort(session.Status.Ports.AudioRTP),
						),
				),
			metav1.ApplyOptions{
				FieldManager: "direwolf-session-controller-svc",
			})
	if err != nil {
		return fmt.Errorf("failed to apply service: %w", err)
	}
	return nil
}

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
	maps.Copy(merged.Limits, overrides.Limits)

	// Override requests
	maps.Copy(merged.Requests, overrides.Requests)

	return *merged
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

func (c *SessionController) reconcilePod(ctx context.Context, session *direwolfv1alpha1.Session) error {
	//!TODO: Just allocate a ton of ports on the container, we wont be able to
	// change them while its running if another user connects
	if !meta.IsStatusConditionPresentAndEqual(session.Status.Conditions, "PortsAllocated", metav1.ConditionTrue) {
		return errors.New("waiting for PortsAllocated")
	}

	// Get the profile object to access resource policies
	profile, err := c.ProfileInformer.Namespaced(session.Namespace).Get(session.Spec.ProfileReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get profile %s: %w", session.Spec.ProfileReference.Name, err)
	}

	pg := profileGame{
		Game:    session.Spec.GameReference.Name,
		Profile: session.Spec.ProfileReference.Name,
	}

	var owners []metav1.OwnerReference
	var ownerApply []*metav1ac.OwnerReferenceApplyConfiguration
	if sessions, ok := c.trackedSessions[pg]; ok {
		for name := range sessions {
			sess, err := c.SessionClient.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if kerrors.IsNotFound(err) {
					// Session is gone, clean it from tracking
					klog.Warningf("Session %s/%s not found, skipping owner ref", session.Namespace, name)
					continue
				}
				klog.Errorf("Failed to get session %s/%s: %s", session.Namespace, name, err)
				continue
			}

			owner := metav1.OwnerReference{
				APIVersion: direwolfv1alpha1.GroupVersion.String(),
				Kind:       "Session",
				Name:       name,
				UID:        sess.UID,
				Controller: ptr.To(true),
			}
			owners = append(owners, owner)
			ownerApply = append(ownerApply, metav1ac.OwnerReference().
				WithName(name).
				WithAPIVersion(direwolfv1alpha1.GroupVersion.String()).
				WithKind("Session").
				WithUID(sess.UID).
				WithController(true))
		}
	}

	// If deployment already exists, just skip
	deploymentName := c.deploymentName(session)
	if _, err := c.deploymentController.Informer().Namespaced(session.Namespace).Get(deploymentName); err == nil {
		klog.Infof("Deployment %s/%s already exists, just updating metadata", session.Namespace, deploymentName)
		if _, err := c.K8sClient.AppsV1().Deployments(session.Namespace).Apply(
			ctx,
			appsv1ac.Deployment(deploymentName, session.Namespace).
				WithOwnerReferences(ownerApply...),
			metav1.ApplyOptions{
				FieldManager: "direwolf-session-controller-deployment-owners",
			},
		); err != nil {
			return fmt.Errorf("failed to apply owner references to deployment %s/%s: %w", session.Namespace, deploymentName, err)
		}

		return nil
	}

	// Create pod from pod template
	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}
	// Prepare environment variables for the wolf container
	// commenting these out was the main reason nvidia stuff wasn't working.
	// specifically the NVIDIA_VISIBLE_DEVICES
	// I need a better method of injecting env vars / configs to the pod
	// TODO better env var handling in DRA
	wolfEnvVars := map[string]string{
		"PUID":                   "1000",
		"PGID":                   "1000",
		"UNAME":                  "ubuntu",
		"XDG_RUNTIME_DIR":        "/tmp/.X11-unix", //nolint
		"PULSE_SERVER":           "unix:/tmp/.X11-unix/pulse-socket",
		"HOST_APPS_STATE_FOLDER": "/mnt/data/wolf",
		"WOLF_SOCKET_PATH":       "/etc/wolf/wolf.sock",
		// Keeping those for later
		// "GST_VAAPI_ALL_DRIVERS":      "1",
		// "GST_DEBUG":                  "2",
		// "__GL_SYNC_TO_VBLANK":        "0",
		// "NVIDIA_VISIBLE_DEVICES":     "all",
		// "NVIDIA_DRIVER_CAPABILITIES": "all",
		// "LIBVA_DRIVER_NAME":          "nvidia",
		// "LD_LIBRARY_PATH":            "/usr/local/nvidia/lib:/usr/local/nvidia/lib64:/usr/local/lib",
	}

	// Check if runtime variables are defined in the App spec and override defaults
	// TODO: Remove all of these
	if app.Spec.WolfConfig.RuntimeVariables != nil {
		runtimeVars := app.Spec.WolfConfig.RuntimeVariables
		if runtimeVars.TimeZone != "" {
			wolfEnvVars["TZ"] = runtimeVars.TimeZone
		}
		if runtimeVars.RenderNode != "" {
			wolfEnvVars["WOLF_RENDER_NODE"] = runtimeVars.RenderNode
		}
	}

	if session.Spec.Config.ClientIP != "" {
		wolfEnvVars["WOLF_STREAM_CLIENT_IP"] = session.Spec.Config.ClientIP
	}
	var podToCreate corev1.PodTemplateSpec
	if app.Spec.Template != nil {
		podToCreate.ObjectMeta = app.Spec.Template.ObjectMeta
		podToCreate.Spec = *app.Spec.Template.Spec.DeepCopy()
	}

	if podToCreate.Labels == nil {
		podToCreate.Labels = map[string]string{}
	}

	podToCreate.Labels["app"] = "direwolf-worker" //nolint
	podToCreate.Labels[AppLabelKey] = session.Spec.GameReference.Name
	podToCreate.Labels[ProfileLabelKey] = session.Spec.ProfileReference.Name

	wolfEnvVarsSlice := make([]corev1.EnvVar, 0, len(wolfEnvVars))
	for k, v := range wolfEnvVars {
		wolfEnvVarsSlice = append(wolfEnvVarsSlice, corev1.EnvVar{Name: k, Value: v})
	}

	// Inject volume mounts into existing containers
	for i := range podToCreate.Spec.Containers {
		podToCreate.Spec.Containers[i].VolumeMounts = append(podToCreate.Spec.Containers[i].VolumeMounts,
			corev1.VolumeMount{
				Name:      "wolf-runtime", //nolint
				MountPath: "/tmp/.X11-unix",
			},
			corev1.VolumeMount{
				Name:      "wolf-data",
				MountPath: "/home/retro",
				SubPath:   "state/" + app.Name,
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
			{Name: "XDG_RUNTIME_DIR", Value: "/tmp/.X11-unix"}, //nolint
			// "UID":             "1000",
			// "GID":             "1000",
			{Name: "PULSE_SERVER", Value: "unix:/tmp/.X11-unix/pulse-socket"},
			// PULSE_SINK & PULSE_SOURCE set at runtime calculated based off session ID.
			// But would be nice if unnecessary

			// Assorted NVIDIA env vars, definitely required for Nvidia devices.
			{Name: "LIBVA_DRIVER_NAME", Value: "nvidia"},
			{Name: "LD_LIBRARY_PATH", Value: "/usr/local/nvidia/lib:/usr/local/nvidia/lib64:/usr/local/lib"},
			{Name: "NVIDIA_DRIVER_CAPABILITIES", Value: "all"},
			{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"},
			{Name: "GST_VAAPI_ALL_DRIVERS", Value: "1"},
			{Name: "GST_DEBUG", Value: "2"},

			// Gamescape envar injection. Ham-handed. Why not.
			{Name: "GAMESCOPE_WIDTH", Value: strconv.Itoa(session.Spec.Config.VideoWidth)},
			{Name: "GAMESCOPE_HEIGHT", Value: strconv.Itoa(session.Spec.Config.VideoHeight)},
			{Name: "GAMESCOPE_REFRESH", Value: strconv.Itoa(session.Spec.Config.VideoRefreshRate)},
		}...)

		// Validate the main app container's resources against the profile's policy.
		validatedResources, err := validateAppResources(podToCreate.Spec.Containers[i].Resources, profile.Spec.Resources)
		if err != nil {
			// The error will be handled by the main Reconcile loop to update the session status.
			return fmt.Errorf("resource validation for main app container failed: %w", err)
		}
		podToCreate.Spec.Containers[i].Resources = validatedResources
	}

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
	for _, volume := range profile.Spec.Volumes {
		validVolumes[volume.Name] = struct{}{}
	}

	if profile.Spec.SidecarPolicies != nil {
		policies := profile.Spec.SidecarPolicies
		if policies.WolfAgent != nil {
			if err := validateVolumeMounts(policies.WolfAgent.VolumeMounts, validVolumes, "wolfAgent"); err != nil {
				return err
			}
			wolfAgentEnv = policies.WolfAgent.Env
			wolfAgentResources = mergeResourceRequirements(wolfAgentDefaultResources, policies.WolfAgent.Resources)
			wolfAgentVolumeMounts = policies.WolfAgent.VolumeMounts
			wolfAgentSecurityContext = policies.WolfAgent.SecurityContext
			if policies.WolfAgent.HostIPC != nil && *policies.WolfAgent.HostIPC {
				podHostIPC = true
			}
		}
		if policies.PulseAudio != nil {
			if err := validateVolumeMounts(policies.PulseAudio.VolumeMounts, validVolumes, "pulseAudio"); err != nil {
				return err
			}
			pulseAudioEnv = policies.PulseAudio.Env
			pulseAudioResources = mergeResourceRequirements(pulseAudioDefaultResources, policies.PulseAudio.Resources)
			pulseAudioVolumeMounts = policies.PulseAudio.VolumeMounts
			pulseAudioSecurityContext = policies.PulseAudio.SecurityContext
			if policies.PulseAudio.HostIPC != nil && *policies.PulseAudio.HostIPC {
				podHostIPC = true
			}
		}
		if policies.Wolf != nil {
			if err := validateVolumeMounts(policies.Wolf.VolumeMounts, validVolumes, "wolf"); err != nil {
				return err
			}
			wolfEnv = policies.Wolf.Env
			wolfResources = mergeResourceRequirements(wolfDefaultResources, policies.Wolf.Resources)
			wolfVolumeMounts = policies.Wolf.VolumeMounts
			wolfSecurityContext = policies.Wolf.SecurityContext
			if policies.Wolf.HostIPC != nil && *policies.Wolf.HostIPC {
				podHostIPC = true
			}
		}
	}

	// Apply HostIPC setting to the pod spec if requested by any sidecar policy
	podToCreate.Spec.HostIPC = podHostIPC

	podToCreate.Spec.Containers = append(podToCreate.Spec.Containers,
		corev1.Container{
			Name:            "wolf-agent",
			Image:           c.WolfAgentImage,
			ImagePullPolicy: corev1.PullPolicy(c.WolfAgentImagePullPolicy),
			// ImagePullPolicy: corev1.PullIfNotPresent,
			Args: []string{
				"--socket=/etc/wolf/wolf.sock",
				"--port=8443",
			},
			Ports: []corev1.ContainerPort{
				{
					Name:          "wa",
					ContainerPort: 8443,
				},
			},
			Env: append([]corev1.EnvVar{
				{
					Name:  "XDG_RUNTIME_DIR", //nolint
					Value: "/tmp/.X11-unix",  //nolint
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
					// I don't know what purpose does this serve
					// but since this is in the wolf-agent image, it'll eventually go alongside most of these env vars.
					Name:  "DIREWOLF_PROFILE",
					Value: session.Spec.ProfileReference.Name,
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
						Port:   intstr.FromInt(8443),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/livez",
						Port:   intstr.FromInt(8443),
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
					Name:      "wolf-runtime", //nolint
					MountPath: "/tmp/.X11-unix",
				},
			}, wolfAgentVolumeMounts...),
		},
		corev1.Container{
			Name:  "pulseaudio",
			Image: "ghcr.io/games-on-whales/pulseaudio:edge",
			Env: append([]corev1.EnvVar{
				{Name: "TZ", Value: wolfEnvVars["TZ"]},
				{Name: "UNAME", Value: "retro"},
				{Name: "XDG_RUNTIME_DIR", Value: "/tmp/pulse"}, //nolint
				// "UID":             "1000",
				// "GID":             "1000",
			}, pulseAudioEnv...),

			Resources:       pulseAudioResources,
			SecurityContext: pulseAudioSecurityContext,
			VolumeMounts: append([]corev1.VolumeMount{
				{
					Name:      "wolf-runtime", //nolint
					MountPath: "/tmp/pulse",
				},
			}, pulseAudioVolumeMounts...),
		},
		corev1.Container{
			Name:  "wolf",
			Image: wolfImage,
			Env:   append(wolfEnvVarsSlice, wolfEnv...),
			// Note: Container Ports list is strictly informational. As long
			// as process is listening on 0.0.0.0 it can be bound by a service.
			Ports: []corev1.ContainerPort{
				{
					Name:          "http",
					ContainerPort: 48989,
				},
				{
					Name:          "https",
					ContainerPort: 48984,
				},
				{
					Name:          "rtsp",
					ContainerPort: session.Status.Ports.RTSP,
				},
				{
					Name:          "enet",
					ContainerPort: session.Status.Ports.Control,
				},
				{
					Name:          "video",
					ContainerPort: session.Status.Ports.VideoRTP,
				},
				{
					Name:          "audio",
					ContainerPort: session.Status.Ports.AudioRTP,
				},
			},
			Resources:       wolfResources,
			SecurityContext: wolfSecurityContext,
			VolumeMounts: append([]corev1.VolumeMount{
				{
					Name:      "wolf-cfg",
					MountPath: "/etc/wolf",
				},
				{
					Name:      "wolf-runtime", //nolint
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
				ClaimName: c.deploymentName(session),
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
			Name: "wolf-runtime", //nolint
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name:         "wolf-data",
			VolumeSource: wolfDataVolumeSource,
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

	// Add volumes from the profile spec
	if len(profile.Spec.Volumes) > 0 {
		podToCreate.Spec.Volumes = append(podToCreate.Spec.Volumes, profile.Spec.Volumes...)
	}

	// Create deployment scaled to 1 for this pod
	// Should use deployment so that changes in spec aren't rejected.
	deployment := appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.deploymentName(session),
			Namespace: session.Namespace,
			Labels: map[string]string{
				"app":           "direwolf-worker", //nolint
				AppLabelKey:     session.Spec.GameReference.Name,
				ProfileLabelKey: session.Spec.ProfileReference.Name,
			},
			OwnerReferences: owners,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					AppLabelKey:     session.Spec.GameReference.Name,
					ProfileLabelKey: session.Spec.ProfileReference.Name,
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			RevisionHistoryLimit:    ptr.To[int32](1),
			ProgressDeadlineSeconds: ptr.To[int32](10),
			Template:                podToCreate,
		},
	}

	unstructuredDeployment, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
	if err != nil {
		return fmt.Errorf("failed to convert deployment to unstructured: %w", err)
	}

	// NOTE: Kinda dumb cuz its just gona get serialized again....
	// could just use dynamic client
	var deploymentApplyConfig appsv1ac.DeploymentApplyConfiguration
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredDeployment, &deploymentApplyConfig)
	if err != nil {
		return fmt.Errorf("failed to convert unstructured to deployment: %w", err)
	}

	_, err = c.K8sClient.AppsV1().Deployments(session.Namespace).Apply(
		ctx,
		&deploymentApplyConfig,
		metav1.ApplyOptions{
			FieldManager: "direwolf-session-controller-deployment",
		})

	if err != nil {
		return fmt.Errorf("failed to apply deployment: %w", err)
	}

	return nil
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
// 						AppLabelKey:  session.Spec.GameReference.Name,
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

func (c *SessionController) reconcilePVC(ctx context.Context, session *direwolfv1alpha1.Session) error {
	profile, err := c.ProfileInformer.Namespaced(session.Namespace).Get(session.Spec.ProfileReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get profile: %w", err)
	}
	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get profile: %w", err)
	}

	// Check if the user defined a volume claim template. If not, return nil.
	if app.Spec.VolumeClaimTemplate == nil {
		klog.Infof("App %s does not define a VolumeClaimTemplate, skipping PVC creation.", app.Name)
		return nil
	}

	pvcName := c.deploymentName(session)
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
				"app":           "direwolf-worker", //nolint
				AppLabelKey:     session.Spec.GameReference.Name,
				ProfileLabelKey: session.Spec.ProfileReference.Name,
			}).
			WithOwnerReferences(metav1ac.OwnerReference().
				WithName(profile.Name).
				WithAPIVersion(direwolfv1alpha1.GroupVersion.String()).
				WithKind("Profile").
				WithUID(profile.UID).
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
func (c *SessionController) deploymentName(session *direwolfv1alpha1.Session) string {
	return fmt.Sprintf("%s-%s", session.Spec.ProfileReference.Name, session.Spec.GameReference.Name)
}

func (c *SessionController) allocatePorts(
	_ context.Context,
	session *direwolfv1alpha1.Session,

) error { //nolint
	// This will be reimplemented during DRA
	//!TODO: Take lock if multiple workers are running

	// 0. Allocate ports for this streaming session to use
	// 1. List all listeners for the gateway
	// 2. List all routes attached to the gateway
	// 3. Subtract used ports
	// 4. Choose a port for RTSP, Enet, Video RTP, Audio RTP

	//!TODO: Implement this properly once wolf lets us assign ports. For now, just
	// hardcode some ports.
	session.Status.Ports = direwolfv1alpha1.SessionPorts{
		RTSP:     48010,
		Control:  47999,
		VideoRTP: 48100,
		AudioRTP: 48200,
	}
	return nil
}

// reconcileActiveStreams calls out to wolf-agent on the running pod to ensure
// that wolf is configured in the correct state and listening for streams on the
// correct ports for each session trying to connect to the Pod.
func (c *SessionController) reconcileActiveStreams(
	ctx context.Context,
	session *direwolfv1alpha1.Session,
) error {
	deploymentName := c.deploymentName(session)

	// !TODO: Use informer for cache reads instead?
	deployment, err := c.K8sClient.AppsV1().Deployments(session.Namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	if deployment.Status.ObservedGeneration != deployment.Generation ||
		deployment.Status.ReadyReplicas != deployment.Status.Replicas {
		return fmt.Errorf("deployment %s/%s not ready (Observed %d, Latest %d) (%d/%d)",
			session.Namespace, deploymentName,
			deployment.Status.ObservedGeneration, deployment.Generation,
			deployment.Status.ReadyReplicas, deployment.Status.Replicas)
	}

	// Get service for the deployment
	service, err := c.K8sClient.CoreV1().Services(session.Namespace).Get(ctx, session.Status.ServiceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get service: %w", err)
	}

	// List all the "sessions".
	// Ensure they match each of our k8s sessions. Hash on AESKey/IV
	// In the future it might make sense to just match on ClientID/ClientCertFingerprint
	// but that is hardcoded for now :)
	wolfclient := wolfapi.NewClient("https://"+net.JoinHostPort(service.Spec.ClusterIP, "8443"), &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint G402
		},
	})

	// Retry wolf-agent calls
	var sessions []wolfapi.Session
	for i := 0; i < 5; i++ {
		sessions, err = wolfclient.ListSessions(ctx)
		if err == nil {
			break
		}
		klog.Warningf("wolf-agent list sessions failed (attempt %d/5) for %s/%s: %v", i+1, session.Namespace, session.Name, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
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

	if found != (session.Status.WolfSessionID != "") {
		klog.Infof("Session %s/%s found: %v, status: %v", session.Namespace, session.Name, found, session.Status.WolfSessionID)
		// Delete the session
		if err := c.SessionClient.Delete(ctx, session.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("failed to delete invalid session %s/%s: %w", session.Namespace, session.Name, err)
		}
		return nil
	}

	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}

	if !found && app != nil {
		clientIP := "10.128.1.0"
		if session.Spec.Config.ClientIP != "" {
			clientIP = session.Spec.Config.ClientIP
		}
		//This is temporary, since sometimes session creation fails on kind cluster
		var sessionID string
		for i := 0; i < 5; i++ {
			sessionID, err = wolfclient.AddSession(ctx, wolfapi.Session{
				VideoWidth:       session.Spec.Config.VideoWidth,
				VideoHeight:      session.Spec.Config.VideoHeight,
				VideoRefreshRate: session.Spec.Config.VideoRefreshRate,
				// AppID:             appID,
				AudioChannelCount: 2,        // !TODO: parse from audio info
				ClientIP:          clientIP, // In the future, this will be acquired dynamically
				// If this isn't present it crashes
				// so, I'll keep it here until I figure out a way to pass off from moonlight client
				ClientSettings: wolfapi.ClientSettings{
					RunGID:              1000,
					RunUID:              1000,
					ControllersOverride: []string{"XBOX"},
					MouseAcceleration:   1.0,
					VScrollAcceleration: 1.0,
					HScrollAcceleration: 1.0,
				},
				AESKey: session.Spec.Config.AESKey,
				AESIV:  session.Spec.Config.AESIV,
				//!TODO: not this. This is the hash of the client cert we are
				// hardcoding into wolf config. Should call pair endpoint to genuinely
				// add it. Though not really needed since user doesnt connect via HTTPS
				// to wolf, we just need a client ID wolf accepts for this specific
				// pairing/client...
				// ClientID:   "4193251087262667199",
				RTSPFakeIP: service.Spec.ClusterIP,
			})
			if err == nil {
				break
			}
			klog.Warningf("wolf-agent add session failed (attempt %d/5) for %s/%s: %v", i+1, session.Namespace, session.Name, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(i+1) * time.Second):
			}
		}
		if err != nil {
			return fmt.Errorf("failed to create session: %w", err)
		}
		session.Status.WolfSessionID = sessionID
	}
	// else {
	//!TODO: Update wolf API to include session ID in list so we can update
	// these details/validate discrepencies
	// assert wolf session ID non-empty and matches what we expect
	// }

	session.Status.StreamURL = "rtsp://" + net.JoinHostPort(
		service.Spec.ClusterIP,
		strconv.FormatInt(int64(session.Status.Ports.RTSP), 10),
	)

	return nil
}
