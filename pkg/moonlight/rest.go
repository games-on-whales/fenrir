package moonlight

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
	"golang.org/x/image/webp"

	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	_ "embed"
)

//go:embed pin.html
var pinHTML string

type RESTServerOptions struct {
	Port       int
	SecurePort int
	Cert       tls.Certificate
}

type RESTServer struct {
	router       *http.ServeMux
	secureRouter *http.ServeMux

	manager *PairingManager

	PairingsLister generic.NamespacedLister[*v1alpha1types.Pairing]
	UserLister     generic.NamespacedLister[*v1alpha1types.User]
	AppLister      generic.NamespacedLister[*v1alpha1types.App]
	PodLister      generic.NamespacedLister[*v1.Pod]
	SessionLister  generic.NamespacedLister[*v1alpha1types.Session]
	LobbyLister    generic.NamespacedLister[*v1alpha1types.Lobby]

	SessionClient v1alpha1client.SessionInterface
	LobbyClient   v1alpha1client.LobbyInterface

	RESTServerOptions
}

func NewRESTServer(
	manager *PairingManager,
	pairingsLister generic.NamespacedLister[*v1alpha1types.Pairing],
	userLister generic.NamespacedLister[*v1alpha1types.User],
	appLister generic.NamespacedLister[*v1alpha1types.App],
	sessionLister generic.NamespacedLister[*v1alpha1types.Session],
	lobbyLister generic.NamespacedLister[*v1alpha1types.Lobby],
	podsLister generic.NamespacedLister[*v1.Pod],
	sessionClient v1alpha1client.SessionInterface,
	lobbyClient v1alpha1client.LobbyInterface,
	opts RESTServerOptions,
) *RESTServer {
	ps := &RESTServer{
		router:            http.NewServeMux(),
		secureRouter:      http.NewServeMux(),
		manager:           manager,
		PairingsLister:    pairingsLister,
		UserLister:        userLister,
		AppLister:         appLister,
		SessionLister:     sessionLister,
		LobbyLister:       lobbyLister,
		PodLister:         podsLister,
		SessionClient:     sessionClient,
		LobbyClient:       lobbyClient,
		RESTServerOptions: opts,
	}

	// Register routes
	ps.router.HandleFunc("/serverinfo", ps.serverInfoHandler)
	ps.router.HandleFunc("/pair", ps.pairHandler)
	ps.router.HandleFunc("/unpair", ps.unpairHandler)
	ps.router.HandleFunc("/pin/", ps.pinHandler)

	ps.router.HandleFunc("/readyz", ps.readyzHandler)
	ps.router.HandleFunc("/livez", ps.livezHandler)

	ps.secureRouter.HandleFunc("/serverinfo", ps.serverInfoHandler)
	ps.secureRouter.HandleFunc("/pair", ps.pairHandler)

	ps.secureRouter.HandleFunc("/applist", ps.appListHandler)
	ps.secureRouter.HandleFunc("/launch", ps.launchHandler)
	ps.secureRouter.HandleFunc("/resume", ps.resumeHandler)
	ps.secureRouter.HandleFunc("/cancel", ps.cancelHandler)
	ps.secureRouter.HandleFunc("/appasset", ps.appAssetHandler)

	return ps
}

func (s *RESTServer) Run(ctx context.Context) error {
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.Port),
		Handler: loggingMiddleware(s.router),
	}

	secureServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.SecurePort),
		Handler: loggingMiddleware(s.authenticatedMiddleware(s.secureRouter)),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{s.Cert},
			ClientAuth:   tls.RequestClientCert,
			VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
				// Accept any certificate for now during connection negotiation
				// we have a middleware that will verify the actual ceritificate
				// used and find a user for later user.
				return nil
			},
		},
	}

	ctx, cancel := context.WithCancel(ctx)
	var error atomic.Pointer[error]
	go func() {
		defer cancel()
		klog.Infof("HTTP server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) {
			klog.Errorf("HTTP Server failed: %s", err)
			error.CompareAndSwap(nil, &err)
		}
	}()

	go func() {
		defer cancel()
		klog.Infof("HTTPS server listening on %s", secureServer.Addr)
		if err := secureServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) {
			klog.Errorf("HTTPS Server failed: %s", err)
			error.CompareAndSwap(nil, &err)
		}
	}()

	<-ctx.Done()
	klog.Info("Shutting down server...")
	server.Shutdown(context.Background())
	secureServer.Shutdown(context.Background())

	if err := error.Load(); err != nil {
		klog.Errorf("Server failed: %s", *err)
		return *err
	}

	return nil
}

func (s *RESTServer) readyzHandler(w http.ResponseWriter, r *http.Request) {
	// Server is ready to serve traffic as soon as HTTP & HTTPS server is UP.
	//!TODO: (And when all informers/caches are synced)
	w.WriteHeader(http.StatusOK)
}

func (s *RESTServer) livezHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *RESTServer) serverInfoHandler(w http.ResponseWriter, r *http.Request) {
	// If this is HTTPS request, print the client certificate
	pairStatus := 0
	serverStatus := "SUNSHINE_SERVER_FREE"
	currentGame := ""

	if user := r.Context().Value(userContextKey{}); user != nil {
		user := user.(*v1alpha1types.User)

		// Return SERVER_BUSY if there exists any pod with direwolf/user = <user>
		pods, err := s.PodLister.List(labels.SelectorFromSet(labels.Set{
			"direwolf/user": user.Name,
		}))
		if err != nil {
			writeErrorResponse(w, 500, fmt.Errorf("failed to list pods: %s", err))
			return
		}

		if len(pods) > 0 {
			serverStatus = "SUNSHINE_SERVER_BUSY"

			currentApp, err := s.AppLister.Get(pods[0].Labels["direwolf/app"])
			if err != nil {
				writeErrorResponse(w, 500, fmt.Errorf("failed to get app: %s", err))
				return
			}

			currentGame = fmt.Sprint(currentApp.Spec.ID)
		}
		pairStatus = 1
	}

	root := ServerInfoResponse{
		Response: Response{
			StatusCode: 200,
		},
		ServerInfo: ServerInfo{
			Hostname:               "Direwolf",
			AppVersion:             "7.1.431.-1",
			GfeVersion:             "3.23.0.74",
			UniqueID:               "dd7c60f6-4b88-4ef1-be07-eeec72f96080", // Does this matter?
			MaxLumaPixelsHEVC:      1869449984,
			ServerCodecModeSupport: 65793, // Bitwise OR of various codecs. Just hardcoding HEVC and AV1 for now
			HttpsPort:              s.SecurePort,
			ExternalPort:           s.Port,
			MAC:                    "00:00:00:00:00:00", // Does this matter?
			LocalIP:                "127.0.0.1",         // Does this matter?
			SupportedDisplayModes: DisplayModes{
				Modes: []DisplayMode{
					{1280, 720, 120}, {1280, 720, 60}, {1280, 720, 30},
					{1920, 1080, 120}, {1920, 1080, 60}, {1920, 1080, 30},
					{2560, 1440, 120}, {2560, 1440, 90}, {2560, 1440, 60},
					{3840, 2160, 120}, {3840, 2160, 90}, {3840, 2160, 60},
					{7680, 4320, 120}, {7680, 4320, 90}, {7680, 4320, 60},
				},
			},
			PairStatus:  pairStatus,
			CurrentGame: currentGame, // Meant to be "last played game". But we still use current game
			State:       serverStatus,
		},
	}

	sendXML(w, root)
}

// getBaseURL extracts the true host and protocol from the incoming HTTP request,
// this should be the simplest way for any future url / ip changes
// basically if your moonlight reached this point, it's safe to assume you got the right URI
func getBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}

	host := r.Host
	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		host = forwardedHost
	}

	return fmt.Sprintf("%s://%s", scheme, host)
}

// Multiplex the multiple phases of pairing into a single handler
func (s *RESTServer) pairHandler(w http.ResponseWriter, r *http.Request) {
	klog.Infof("Handling pair request from %s", r.RemoteAddr)

	clientID := r.URL.Query().Get("uniqueid")
	clientIP := strings.Split(r.RemoteAddr, ":")[0]
	cacheKey := fmt.Sprintf("%s@%s", clientID, clientIP)

	if clientID == "" {
		sendXML(w, failPair("uniqueid required"))
		return
	}

	if r.URL.Query().Has("salt") {
		klog.Infof("Pairing phase 1 with %s\n", cacheKey)
		salt := r.URL.Query().Get("salt")
		clientCertStr := r.URL.Query().Get("clientcert")

		reqHost := getBaseURL(r)

		sendXML(w, s.manager.pairPhase1(cacheKey, salt, clientCertStr, reqHost))
		return
	} else if r.URL.Query().Has("clientchallenge") {
		klog.Infof("Pairing phase 2 with %s\n", cacheKey)
		clientChallenge := r.URL.Query().Get("clientchallenge")

		sendXML(w, s.manager.pairPhase2(cacheKey, clientChallenge))
		return
	} else if r.URL.Query().Has("serverchallengeresp") {
		klog.Infof("Pairing phase 3 with %s\n", cacheKey)
		serverChallengeResp := r.URL.Query().Get("serverchallengeresp")

		sendXML(w, s.manager.pairPhase3(cacheKey, serverChallengeResp))
		return
	} else if r.URL.Query().Has("clientpairingsecret") {
		klog.Infof("Pairing phase 4 with %s\n", cacheKey)
		clientPairingSecret := r.URL.Query().Get("clientpairingsecret")

		sendXML(w, s.manager.pairPhase4(cacheKey, clientPairingSecret))
		return
	} else if phrase := r.URL.Query().Get("phrase"); phrase == "pairchallenge" {
		klog.Infof("Pairing phase 5 with %s\n", cacheKey)
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			sendXML(w, failPair("Client cert required"))
			return
		}
		sendXML(w, PairingResponse{Paired: 1, Response: Response{StatusCode: 200}})
		return
	}

	sendXML(w, failPair("Invalid pairing request"))
}

func (s *RESTServer) unpairHandler(w http.ResponseWriter, r *http.Request) {
	klog.Infof("Handling unpair request from %s", r.RemoteAddr)
	if r.Method == "GET" {
		clientIP := strings.Split(r.RemoteAddr, ":")[0]
		clientID := r.URL.Query().Get("uniqueid")
		cacheKey := fmt.Sprintf("%s@%s", clientID, clientIP)

		if err := s.manager.Unpair(cacheKey); err != nil {
			writeErrorResponse(w, 500, err)
			return
		}

		sendXML(w, Response{StatusCode: 200})
	}
}

func (s *RESTServer) pinHandler(w http.ResponseWriter, r *http.Request) {
	klog.Infof("Handling %v pin request from %s", r.Method, r.RemoteAddr)

	// Handle GET /pin
	if r.Method == "GET" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(pinHTML))
		return
	}

	if r.Method == "POST" {
		type PinRequest struct {
			Pin      string `json:"pin"`
			Secret   string `secret:"secret"` // Removed `secret:"secret"` struct tag if it was a typo
			Username string `json:"username"`
		}

		var req PinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Invalid request"))
			return
		}

		if req.Pin == "" || req.Secret == "" || req.Username == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Invalid request: missing pin, secret, or username"))
			return
		}

		// Provide the pin to the pair manager
		klog.Infof("Received pin %s for secret %s, user %s", req.Pin, req.Secret, req.Username)
		err := s.manager.PostPin(req.Secret, req.Pin, req.Username)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}

	w.WriteHeader(http.StatusBadRequest)
	w.Write([]byte("Invalid pin request"))
}

func (s *RESTServer) appListHandler(w http.ResponseWriter, r *http.Request) {
	apps, err := s.AppLister.List(nil)
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to list apps: %s", err))
		return
	}

	appsList := make([]App, 0, len(apps))
	for _, app := range apps {
		appsList = append(appsList, App{
			AppSpec: app.Spec,
		})
	}

	sendXML(w, AppListResponse{
		Response: Response{
			StatusCode: 200,
		},
		Apps: appsList,
	})
}

type launchParams struct {
	appID             string
	mode              string
	rikey             string
	rikeyID           string
	surroundAudioInfo string
	width             int
	height            int
	refreshRate       int
	surroundFlags     int
	clientIP          string
}

func parseLaunchRequest(r *http.Request) (*launchParams, error) {
	clientIP := strings.Split(r.RemoteAddr, ":")[0]
	appID := r.URL.Query().Get("appid")
	mode := r.URL.Query().Get("mode")
	rikey := r.URL.Query().Get("rikey")
	rikeyID := r.URL.Query().Get("rikeyid")
	surroundAudioInfo := r.URL.Query().Get("surroundAudioInfo")

	if appID == "" {
		return nil, fmt.Errorf("appid required")
	}
	if rikey == "" {
		return nil, fmt.Errorf("rikey required")
	}
	if rikeyID == "" {
		return nil, fmt.Errorf("rikeyid required")
	}
	if mode == "" {
		mode = "1920x1080x60"
	}

	splitMode := strings.Split(mode, "x")
	if len(splitMode) != 3 {
		return nil, fmt.Errorf("invalid mode: %s", mode)
	}

	width, err := strconv.Atoi(splitMode[0])
	if err != nil {
		return nil, fmt.Errorf("invalid mode: %s", mode)
	}
	height, err := strconv.Atoi(splitMode[1])
	if err != nil {
		return nil, fmt.Errorf("invalid mode: %s", mode)
	}
	refreshRate, err := strconv.Atoi(splitMode[2])
	if err != nil {
		return nil, fmt.Errorf("invalid mode: %s", mode)
	}

	if surroundAudioInfo == "" {
		surroundAudioInfo = "196610"
	}
	surroundFlags, err := strconv.Atoi(surroundAudioInfo)
	if err != nil {
		return nil, fmt.Errorf("invalid surroundAudioInfo: %s", surroundAudioInfo)
	}

	return &launchParams{
		appID: appID, mode: mode, rikey: rikey, rikeyID: rikeyID,
		surroundAudioInfo: surroundAudioInfo, width: width, height: height,
		refreshRate: refreshRate, surroundFlags: surroundFlags, clientIP: clientIP,
	}, nil
}

func (s *RESTServer) launchHandler(w http.ResponseWriter, r *http.Request) {
	params, err := parseLaunchRequest(r)
	if err != nil {
		writeErrorResponse(w, 400, err)
		return
	}

	app, err := s.getAppByID(params.appID)
	if err != nil {
		writeErrorResponse(w, 404, fmt.Errorf("app not found: %s", err))
		return
	}

	user := r.Context().Value(userContextKey{}).(*v1alpha1types.User)
	pairing := r.Context().Value(pairingContextKey{}).(*v1alpha1types.Pairing)

	// I should probably introduce randomization? should a user have more than one lobby for each application?
	lobbyName := fmt.Sprintf("%s-%s", user.Name, app.Name)

	// application lobby creation
	_, err = s.LobbyClient.Get(r.Context(), lobbyName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		klog.Infof("Lobby %s not found, creating it for launch", lobbyName)
		newLobby := &v1alpha1types.Lobby{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lobbyName,
				Namespace: pairing.Namespace,
			},
			Spec: v1alpha1types.LobbySpec{
				AppReference:           v1.LocalObjectReference{Name: app.Name},
				ProfileReference:       v1.LocalObjectReference{Name: user.Name},
				MultiUser:              true,             // first join attempt fails, and then the lobby is full, keeping this here for now
				StopWhenEveryoneLeaves: false,            // should make this a parameter
				RunnerOverride:         "sleep infinity", // we just need it to create the wayland socket, the application will  then attach to it, so sleep infinity works fine for now.
				VideoSettings: v1alpha1types.LobbyVideoSettings{
					Width:       params.width,
					Height:      params.height,
					RefreshRate: params.refreshRate,
				},
				AudioSettings: v1alpha1types.LobbyAudioSettings{
					ChannelCount: 2,
				},
			},
		}
		_, err = s.LobbyClient.Create(r.Context(), newLobby, metav1.CreateOptions{})
		if err != nil {
			writeErrorResponse(w, 500, fmt.Errorf("failed to create lobby: %s", err))
			return
		}
	} else if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to get lobby: %s", err))
		return
	}

	// Best-effort cleanup of any stale sessions for this user
	if err := s.stopSessionsForUser(user, false); err != nil && !k8serrors.IsNotFound(err) {
		writeErrorResponse(w, 500, fmt.Errorf("failed to stop existing sessions: %s", err))
		return
	}

	klog.Infof("Launching app %s for user %s", app.ObjectMeta.Name, user.ObjectMeta.Name)
	session, err := s.SessionClient.Create(
		r.Context(),
		&v1alpha1types.Session{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: fmt.Sprintf("%s-%s-", user.Name, app.Name),
				Namespace:    pairing.Namespace,
				Labels: map[string]string{
					"direwolf":      "true",
					"direwolf/app":  app.ObjectMeta.Name,
					"direwolf/user": user.ObjectMeta.Name,
				},
				Annotations: map[string]string{
					"direwolf/pairing": pairing.ObjectMeta.Name,
				},
			},
			Spec: v1alpha1types.SessionSpec{
				GameReference: v1alpha1types.GameReference{
					Name: app.ObjectMeta.Name,
				},
				PairingReference: v1alpha1types.PairingReference{
					Name: pairing.ObjectMeta.Name,
				},
				UserReference: v1alpha1types.UserReference{
					Name: user.ObjectMeta.Name,
				},
				LobbyReference: v1alpha1types.LobbyReference{
					Name: lobbyName,
				},
				GatewayReference: v1alpha1types.GatewayReference{
					Name:      "unused",
					Namespace: "unused",
				},
				Config: v1alpha1types.SessionInfo{
					ClientIP:           params.clientIP,
					AESIV:              params.rikeyID,
					AESKey:             params.rikey,
					SurroundAudioFlags: params.surroundFlags,
					VideoWidth:         params.width,
					VideoHeight:        params.height,
					VideoRefreshRate:   params.refreshRate,
				},
			},
		},
		metav1.CreateOptions{
			FieldManager: "direwolf-launch",
		},
	)
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to create session: %s", err))
		return
	}

	// Wait for the session controller to provision the stream then join the lobby
	var streamURL string
	err = wait.PollUntilContextTimeout(r.Context(), 250*time.Millisecond, 25*time.Second, true, func(ctx context.Context) (bool, error) {
		session, err := s.SessionClient.Get(ctx, session.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if session.Status.StreamURL == "" {
			return false, nil
		}
		streamURL = session.Status.StreamURL
		return true, nil
	})
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to launch app: %s", err))
		return
	}

	sendXML(w, LaunchResponse{
		Response: Response{
			StatusCode: 200,
		},
		RTSPSessionURL: streamURL,
		GameSession:    1,
	})
}

// resuming with a changed display restarts the pod
func (s *RESTServer) resumeHandler(w http.ResponseWriter, r *http.Request) {
	params, err := parseLaunchRequest(r)
	if err != nil {
		writeErrorResponse(w, 400, err)
		return
	}

	app, err := s.getAppByID(params.appID)
	if err != nil {
		writeErrorResponse(w, 404, fmt.Errorf("app not found: %s", err))
		return
	}

	user := r.Context().Value(userContextKey{}).(*v1alpha1types.User)
	pairing := r.Context().Value(pairingContextKey{}).(*v1alpha1types.Pairing)

	lobbyName := fmt.Sprintf("%s-%s", user.Name, app.Name)

	lobby, err := s.LobbyClient.Get(r.Context(), lobbyName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			klog.Errorf("Resume failed: lobby %s does not exist", lobbyName)
			writeErrorResponse(w, 404, fmt.Errorf("lobby %s not found, cannot resume without prior launch", lobbyName))
		} else {
			writeErrorResponse(w, 500, fmt.Errorf("failed to get lobby: %s", err))
		}
		return
	}

	// clean up stale sessions so the user doesn't end up with multiple session objects
	if err := s.stopSessionsForUser(user, false); err != nil && !k8serrors.IsNotFound(err) {
		writeErrorResponse(w, 500, fmt.Errorf("failed to stop existing sessions: %s", err))
		return
	}

	klog.Infof("Resuming app %s for user %s (lobby %s)", app.ObjectMeta.Name, user.ObjectMeta.Name, lobby.Name)
	session, err := s.SessionClient.Create(
		r.Context(),
		&v1alpha1types.Session{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: fmt.Sprintf("%s-%s-", user.Name, app.Name),
				Namespace:    pairing.Namespace,
				Labels: map[string]string{
					"direwolf":      "true",
					"direwolf/app":  app.ObjectMeta.Name,
					"direwolf/user": user.ObjectMeta.Name,
				},
				Annotations: map[string]string{
					"direwolf/pairing": pairing.ObjectMeta.Name,
				},
			},
			Spec: v1alpha1types.SessionSpec{
				GameReference: v1alpha1types.GameReference{
					Name: app.ObjectMeta.Name,
				},
				PairingReference: v1alpha1types.PairingReference{
					Name: pairing.ObjectMeta.Name,
				},
				UserReference: v1alpha1types.UserReference{
					Name: user.ObjectMeta.Name,
				},
				LobbyReference: v1alpha1types.LobbyReference{
					Name: lobbyName,
				},
				GatewayReference: v1alpha1types.GatewayReference{
					Name:      "unused",
					Namespace: "unused",
				},
				Config: v1alpha1types.SessionInfo{
					ClientIP:           params.clientIP,
					AESIV:              params.rikeyID,
					AESKey:             params.rikey,
					SurroundAudioFlags: params.surroundFlags,
					VideoWidth:         params.width,
					VideoHeight:        params.height,
					VideoRefreshRate:   params.refreshRate,
				},
			},
		},
		metav1.CreateOptions{
			FieldManager: "direwolf-resume",
		},
	)
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to create session: %s", err))
		return
	}

	var streamURL string
	err = wait.PollUntilContextTimeout(r.Context(), 250*time.Millisecond, 25*time.Second, true, func(ctx context.Context) (bool, error) {
		session, err := s.SessionClient.Get(ctx, session.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if session.Status.StreamURL == "" {
			return false, nil
		}
		streamURL = session.Status.StreamURL
		return true, nil
	})
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to resume app: %s", err))
		return
	}

	sendXML(w, LaunchResponse{
		Response: Response{
			StatusCode: 200,
		},
		RTSPSessionURL: streamURL,
		GameSession:    1,
	})
}

func (s *RESTServer) cancelHandler(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userContextKey{}).(*v1alpha1types.User)

	// Delete sessions first so the session controller stops reconciling
	if err := s.stopSessionsForUser(user, false); err != nil && !k8serrors.IsNotFound(err) {
		writeErrorResponse(w, 500, fmt.Errorf("failed to stop sessions: %s", err))
		return
	}

	// Delete lobbies and wait for pods to be cleaned up
	if err := s.stopLobbiesForUser(user, true); err != nil && !k8serrors.IsNotFound(err) {
		writeErrorResponse(w, 500, fmt.Errorf("failed to cancel session: %s", err))
		return
	}
	sendXML(w, Response{StatusCode: 200})
}

func (s *RESTServer) stopSessionsForUser(user *v1alpha1types.User, shouldWait bool) error {
	sessions, err := s.SessionLister.List(labels.SelectorFromSet(labels.Set{
		"direwolf/user": user.Name,
	}))
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	didDelete := false
	for _, session := range sessions {
		if err := s.SessionClient.Delete(context.Background(), session.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("failed to delete session: %w", err)
		}
		didDelete = true
	}

	if !didDelete {
		klog.Warningf("Not stopping any sessions. No sessions found for user %s", user.Name)
		return k8serrors.NewNotFound(v1alpha1types.Resource("session"), user.Name)
	}

	if shouldWait {
		err = wait.PollUntilContextTimeout(context.Background(), 250*time.Millisecond, 25*time.Second, true, func(ctx context.Context) (bool, error) {
			pods, err := s.PodLister.List(labels.SelectorFromSet(labels.Set{
				"direwolf/user": user.Name,
			}))
			if err != nil {
				return false, err
			}
			return len(pods) == 0, nil
		})
		if err != nil {
			return fmt.Errorf("failed to wait for pods to be cleaned up: %w", err)
		}
	}

	return nil
}

func (s *RESTServer) stopLobbiesForUser(user *v1alpha1types.User, shouldWait bool) error {
	lobbies, err := s.LobbyLister.List(nil)
	if err != nil {
		return fmt.Errorf("failed to list lobbies: %w", err)
	}

	didDelete := false
	for _, lobby := range lobbies {
		if lobby.Spec.ProfileReference.Name != user.Name {
			continue
		}
		if err := s.LobbyClient.Delete(context.Background(), lobby.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("failed to delete lobby: %w", err)
		}
		didDelete = true
	}

	if !didDelete {
		klog.Warningf("Not stopping any lobbies. No lobbies found for user %s", user.Name)
		return k8serrors.NewNotFound(v1alpha1types.Resource("lobby"), user.Name)
	}

	if shouldWait {
		err = wait.PollUntilContextTimeout(context.Background(), 250*time.Millisecond, 25*time.Second, true, func(ctx context.Context) (bool, error) {
			pods, err := s.PodLister.List(labels.SelectorFromSet(labels.Set{
				"direwolf/user": user.Name,
			}))
			if err != nil {
				return false, err
			}
			return len(pods) == 0, nil
		})
		if err != nil {
			return fmt.Errorf("failed to wait for pods to be cleaned up: %w", err)
		}
	}

	return nil
}

func (s *RESTServer) appAssetHandler(w http.ResponseWriter, r *http.Request) {
	appID := r.URL.Query().Get("appid")
	if appID == "" {
		writeErrorResponse(w, 400, fmt.Errorf("appid required"))
		return
	}

	app, err := s.getAppByID(appID)
	if err != nil {
		writeErrorResponse(w, 404, fmt.Errorf("app not found: %s", err))
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(200)

	img, err := webp.Decode(bytes.NewReader(app.Spec.AppAssetWebP))
	if err != nil {
		klog.Infof("Failed to decode webp: %s", err)
		return
	}

	pngData := new(bytes.Buffer)
	if err := png.Encode(pngData, img); err != nil {
		klog.Infof("Failed to encode png: %s", err)
		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(pngData.Len()))
	_, err = io.Copy(w, pngData)
	if err != nil {
		klog.Infof("Failed to write png: %s", err)
		return
	}
	klog.Infof("Sent app asset for %s", appID)
}

func writeErrorResponse(w http.ResponseWriter, status int, err error) {
	klog.Error(err)

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))

	// First attempt to serialize as XML with the message. If that fails, just
	// send unfallible statuscode
	if bytes, err := xml.Marshal(Response{
		StatusCode:    status,
		StatusMessage: err.Error(),
	}); err == nil {
		w.Write(bytes)
	} else {
		klog.ErrorS(err, "Failed to marshal XML. Just sending status code")
		w.Write(fmt.Appendf(nil, `<root status_code="%d"></root>`, status))
	}

	klog.ErrorS(err, "Sent error response", "status", status)
}

func sendXML(w http.ResponseWriter, resp Responsable) {
	bytes, err := xml.Marshal(resp)
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to marshal XML: %s", err))
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(resp.GetStatusCode())
	w.Write([]byte(xml.Header))
	w.Write(bytes)

	if resp.GetStatusCode() != 200 {
		klog.V(1).Info("Sent response", string(bytes))
	} else {
		klog.V(1).Info("Sent response", string(bytes))
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		if r.URL.Path != "/serverinfo" {

			klog.Infof("%s %s %s %v %s", r.Proto, r.Method, r.URL.Path, r.URL.Query(), r.RemoteAddr)
			next.ServeHTTP(w, r)
			klog.V(1).Infof("Completed in %s", time.Since(start))
		} else {
			next.ServeHTTP(w, r)
		}
	})
}

type userContextKey struct{}
type pairingContextKey struct{}

// Grabs fingerprint of client cert, finds associated user in backend and attaches
// it to the request context.
func (s *RESTServer) authenticatedMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeErrorResponse(w, 401, fmt.Errorf("client cert required"))
			return
		}

		fingerprint := hex.EncodeToString(util.Hash(r.TLS.PeerCertificates[0].Raw))
		pairing, err := s.PairingsLister.Get(fingerprint)
		if err != nil {
			writeErrorResponse(w, 401, fmt.Errorf("client %s not paired", fingerprint))
			return
		}

		user, err := s.UserLister.Get(pairing.Spec.UserReference.Name)
		if err != nil {
			writeErrorResponse(w, 401, fmt.Errorf("user not found"))
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey{}, user)
		ctx = context.WithValue(ctx, pairingContextKey{}, pairing)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *RESTServer) getAppByID(appID string) (*v1alpha1types.App, error) {
	intParsedAppID, err := strconv.Atoi(appID)
	if err != nil {
		return nil, fmt.Errorf("app id must be an integer")
	}

	apps, err := s.AppLister.List(nil)
	if err != nil {
		return nil, err
	}

	for _, app := range apps {
		if app.Spec.ID == intParsedAppID {
			return app, nil
		}
	}

	return nil, fmt.Errorf("app not found")
}
