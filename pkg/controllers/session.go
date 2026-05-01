package controllers

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"reflect"
	"time"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	// Preserved for future Gateway API implementation:
	// gatewayv1alpha2 "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/typed/apis/v1alpha2"
)

const sessionFinalizer = "direwolf.games-on-whales.io/session-cleanup"

// Session Controller manages the lifecycle of a streaming session for
// a given game, of a given user.
// It is responsible for:
//   - 1. Setting up port forwards via Gateway API (TODO)
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

	LobbyClient   v1alpha1client.LobbyInterface
	LobbyInformer generic.Informer[*v1alpha1types.Lobby]

	K8sClient kubernetes.Interface

	controller generic.Controller[*v1alpha1types.Session]

	// Preserved for future Gateway API implementation:
	// TCPRouteClient gatewayv1alpha2.TCPRouteInterface
	// UDPRouteClient gatewayv1alpha2.UDPRouteInterface
}

// NewSessionController creates a new session controller.
func NewSessionController(
	k8sClient kubernetes.Interface,
	// Preserved for future Gateway API implementation:
	// tcpRouteClient gatewayv1alpha2.TCPRouteInterface,
	// udpRouteClient gatewayv1alpha2.UDPRouteInterface,
	sessionClient v1alpha1client.SessionInterface,
	sessionInformer generic.Informer[*v1alpha1types.Session],
	lobbyClient v1alpha1client.LobbyInterface,
	lobbyInformer generic.Informer[*v1alpha1types.Lobby],
) *SessionController {
	res := &SessionController{
		K8sClient:       k8sClient,
		SessionClient:   sessionClient,
		SessionInformer: sessionInformer,
		LobbyClient:     lobbyClient,
		LobbyInformer:   lobbyInformer,
		// Preserved for future Gateway API implementation:
		// TCPRouteClient: tcpRouteClient,
		// UDPRouteClient: udpRouteClient,
	}

	res.controller = generic.NewController(
		sessionInformer,
		res.Reconcile,
		generic.ControllerOptions{
			Name:    "session-controller",
			Workers: 2,
		},
	)

	return res
}

func (c *SessionController) Run(ctx context.Context) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !cache.WaitForCacheSync(sessionCtx.Done(), c.SessionInformer.HasSynced, c.LobbyInformer.HasSynced) {
		return fmt.Errorf("failed to sync informers")
	}

	return c.controller.Run(sessionCtx)
}

func (c *SessionController) HasSynced() bool {
	return c.SessionInformer.HasSynced() && c.LobbyInformer.HasSynced()
}

// Reconcile handles the main reconciliation loop for a Session object.
func (c *SessionController) Reconcile(namespace, name string, newObj *v1alpha1types.Session) error {
	klog.Infof("Reconciling session %s/%s", namespace, name)
	defer klog.Infof("Finished Reconciling session %s/%s", namespace, name)

	if newObj == nil {
		return nil
	}

	// Handle Deletion (Disconnect)
	if !newObj.DeletionTimestamp.IsZero() {
		return c.handleCleanup(context.TODO(), newObj)
	}

	// Add Finalizer so we can cleanly LeaveLobby and StopSession when deleted
	if !containsString(newObj.Finalizers, sessionFinalizer) {
		newObj.Finalizers = append(newObj.Finalizers, sessionFinalizer)
		_, err := c.SessionClient.Update(context.TODO(), newObj, metav1.UpdateOptions{})
		return err
	}

	// Garbage collection for stale sessions
	if newObj.Status.WolfSessionID == "" && newObj.CreationTimestamp.Add(1*time.Minute).Before(time.Now()) {
		klog.Infof("Session %s/%s is older than 1 minute and has no wolf session ID, deleting", namespace, name)
		err := c.SessionClient.Delete(context.TODO(), name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			klog.Errorf("Failed to delete session %s/%s: %v", namespace, name, err)
			return err
		}
		return nil
	}

	oldStatus := newObj.Status.DeepCopy()

	// 1. Reconcile Lobby (Check if exists, wait if not ready)
	lobby, err := c.reconcileLobby(context.TODO(), newObj)
	if err != nil {
		klog.Errorf("Failed to reconcile lobby: %v", err)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "LobbyReady",
			Status:  metav1.ConditionFalse,
			Reason:  "LobbyReconciliationFailed",
			Message: err.Error(),
		})
		c.updateStatusIfNeeded(newObj, oldStatus)
		return err
	}

	meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
		Type:   "LobbyReady",
		Status: metav1.ConditionTrue,
		Reason: "Success",
	})

	// 2. Map Ports and Details from Lobby to Session
	newObj.Status.Ports = lobby.Status.Ports
	newObj.Status.ServiceName = lobby.Status.ServiceName
	newObj.Status.LobbyName = lobby.Name

	// 3. Connect Stream to the Lobby (joining the lobby is handled by wolf-agent)
	if err := c.reconcileActiveStream(context.TODO(), newObj, lobby); err != nil {
		klog.Errorf("Failed to reconcile active stream: %v", err)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "StreamStarted",
			Status:  metav1.ConditionFalse,
			Reason:  "StreamStartFailed",
			Message: err.Error(),
		})
		c.updateStatusIfNeeded(newObj, oldStatus)
		return err
	}

	meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
		Type:   "StreamStarted",
		Status: metav1.ConditionTrue,
		Reason: "Success",
	})

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

	return c.updateStatusIfNeeded(newObj, oldStatus)
}

// reconcileLobby fetches the Lobby referenced by the session and ensures it
// is ready before allowing the session to connect.
func (c *SessionController) reconcileLobby(ctx context.Context, session *v1alpha1types.Session) (*v1alpha1types.Lobby, error) {
	lobbyName := session.Spec.LobbyReference.Name
	if lobbyName == "" {
		// Will need to account for multi-user lobbies to have something other than name and app
		lobbyName = fmt.Sprintf("%s-%s", session.Spec.UserReference.Name, session.Spec.GameReference.Name)
	}

	lobby, err := c.LobbyInformer.Namespaced(session.Namespace).Get(lobbyName)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("lobby %s not found", lobbyName)
		}
		return nil, fmt.Errorf("failed to fetch lobby: %w", err)
	}

	if lobby.Status.ServiceName == "" {
		return nil, fmt.Errorf("waiting for lobby %s service to be provisioned", lobbyName)
	}

	return lobby, nil
}

// reconcileActiveStream calls out to wolf-agent on the running pod to ensure
// that wolf is configured in the correct state and listening for streams on the
// correct ports for each session trying to connect to the Pod.
func (c *SessionController) reconcileActiveStream(ctx context.Context, session *v1alpha1types.Session, lobby *v1alpha1types.Lobby) error {
	service, err := c.K8sClient.CoreV1().Services(lobby.Namespace).Get(ctx, lobby.Status.ServiceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get service for wolf client: %w", err)
	}

	wolfclient := wolfapi.NewClient(fmt.Sprintf("https://%s:8443", service.Spec.ClusterIP), &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	})

	// Create Stream Session if it doesn't exist
	if session.Status.WolfSessionID == "" {
		clientIP := "10.128.1.0" // I'm keeping this, for now...
		if session.Spec.Config.ClientIP != "" {
			clientIP = session.Spec.Config.ClientIP
		}

		sessionReq := wolfapi.Session{
			VideoWidth:        session.Spec.Config.VideoWidth,
			VideoHeight:       session.Spec.Config.VideoHeight,
			VideoRefreshRate:  session.Spec.Config.VideoRefreshRate,
			AudioChannelCount: 2,
			ClientIP:          clientIP,
			AESKey:            session.Spec.Config.AESKey,
			AESIV:             session.Spec.Config.AESIV,
			RTSPFakeIP:        service.Spec.ClusterIP,
			ClientSettings: wolfapi.ClientSettings{
				RunGID:              1000,
				RunUID:              1000,
				ControllersOverride: []string{"XBOX"},
				MouseAcceleration:   1.0,
				VScrollAcceleration: 1.0,
				HScrollAcceleration: 1.0,
			},
		}

		sessionID, err := wolfclient.AddSession(ctx, sessionReq)
		if err != nil {
			return fmt.Errorf("failed to add session to wolf: %w", err)
		}

		session.Status.WolfSessionID = sessionID
		session.Status.StreamURL = fmt.Sprintf("rtsp://%s:%d", service.Spec.ClusterIP, lobby.Status.Ports.RTSP)
	}

	// JoinLobby is handled asynchronously by the wolf-agent inside the pod.
	// It waits for the AudioSessionEvent before joining to ensure pipelines are ready.
	return nil
}

// handleCleanup executes when a Session is being deleted. It ensures the
// session leaves the wolf lobby and stops its stream before removing the finalizer.
func (c *SessionController) handleCleanup(ctx context.Context, session *v1alpha1types.Session) error {
	if !containsString(session.Finalizers, sessionFinalizer) {
		return nil
	}

	klog.Infof("Executing cleanup for session %s/%s", session.Namespace, session.Name)

	if session.Status.WolfSessionID != "" && session.Status.LobbyName != "" {
		lobby, err := c.LobbyInformer.Namespaced(session.Namespace).Get(session.Status.LobbyName)
		if err == nil && lobby.Status.ServiceName != "" {
			service, err := c.K8sClient.CoreV1().Services(session.Namespace).Get(ctx, lobby.Status.ServiceName, metav1.GetOptions{})
			if err == nil {
				wolfclient := wolfapi.NewClient(fmt.Sprintf("https://%s:8443", service.Spec.ClusterIP), &http.Client{
					Transport: &http.Transport{
						TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
					},
				})

				if lobby.Status.LobbyID != "" {
					leaveReq := wolfapi.LeaveLobbyRequest{
						LobbyID:            lobby.Status.LobbyID,
						MoonlightSessionID: session.Status.WolfSessionID,
					}
					_ = wolfclient.LeaveLobby(ctx, leaveReq)
				}

				_ = wolfclient.StopSession(ctx, session.Status.WolfSessionID)
			}
		}
	}

	session.Finalizers = removeString(session.Finalizers, sessionFinalizer)
	_, err := c.SessionClient.Update(ctx, session, metav1.UpdateOptions{})
	return err
}

func (c *SessionController) updateStatusIfNeeded(session *v1alpha1types.Session, oldStatus *v1alpha1types.SessionStatus) error {
	if !reflect.DeepEqual(session.Status, *oldStatus) {
		_, err := c.SessionClient.UpdateStatus(
			context.TODO(),
			session,
			metav1.UpdateOptions{FieldManager: "session-controller-status"},
		)
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	result := []string{}
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return result
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
