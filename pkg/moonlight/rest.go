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
	Port         int
	SecurePort   int
	Cert         tls.Certificate
	AllowedHosts []string
}

type RESTServer struct {
	router       *http.ServeMux
	secureRouter *http.ServeMux

	manager *PairingManager

	PairingsLister generic.NamespacedLister[*v1alpha1types.Pairing]
	ProfileLister  generic.NamespacedLister[*v1alpha1types.Profile]
	AppLister      generic.NamespacedLister[*v1alpha1types.App]
	PodLister      generic.NamespacedLister[*v1.Pod]
	SessionLister  generic.NamespacedLister[*v1alpha1types.Session]

	SessionClient v1alpha1client.SessionInterface

	RESTServerOptions
}

func NewRESTServer(
	manager *PairingManager,
	pairingsLister generic.NamespacedLister[*v1alpha1types.Pairing],
	profileLister generic.NamespacedLister[*v1alpha1types.Profile],
	appLister generic.NamespacedLister[*v1alpha1types.App],
	sessionLister generic.NamespacedLister[*v1alpha1types.Session],
	podsLister generic.NamespacedLister[*v1.Pod],
	sessionClient v1alpha1client.SessionInterface,
	opts RESTServerOptions,
) *RESTServer {
	ps := &RESTServer{
		router:            http.NewServeMux(),
		secureRouter:      http.NewServeMux(),
		manager:           manager,
		PairingsLister:    pairingsLister,
		ProfileLister:     profileLister,
		AppLister:         appLister,
		SessionLister:     sessionLister,
		PodLister:         podsLister,
		SessionClient:     sessionClient,
		RESTServerOptions: opts,
	}

	// Register routes
	ps.router.HandleFunc("/serverinfo", ps.serverInfoHandler)
	ps.router.HandleFunc("/pair", ps.pairHandler)
	ps.router.HandleFunc("/unpair", ps.unpairHandler)
	// I don't know why I'm keeping this
	// ps.router.HandleFunc("/{username}/pin/", ps.pinHandler)
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
		Addr:              fmt.Sprintf(":%d", s.Port),
		Handler:           loggingMiddleware(s.router),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	secureServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.SecurePort),
		Handler:           loggingMiddleware(s.authenticatedMiddleware(s.secureRouter)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout needs to be generous to accommodate the time an admin takes
		// to input the PIN, but bounded to prevent indefinite leaks.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  1 * time.Minute,
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

	// If the pairing is in the context, the client is successfully paired.
	// We set pairStatus = 1 even if they have no profiles assigned yet.
	if pairing := r.Context().Value(pairingContextKey{}); pairing != nil {
		pairStatus = 1

		if profiles := r.Context().Value(profilesContextKey{}); profiles != nil {
			profiles := profiles.([]*v1alpha1types.Profile)

			// Check pods for any of the available profiles
			for _, profile := range profiles {
				pods, err := s.PodLister.List(labels.SelectorFromSet(labels.Set{
					"direwolf/profile": profile.Name,
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
					break
				}
			}
		}
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

// getBaseURL extracts the true host and protocol from the incoming HTTP request.
// This will most likely be abandoned once we get a frontend ready, users will likely just authenticate from the frontened
// It sanitizes the host string to prevent log injection (CRLF characters).
// If an allow-list is configured, it only trusts X-Forwarded-Host if it matches.
// however, if it's an IP Address, it'll forward it directly
func (s *RESTServer) getBaseURL(r *http.Request) string {
	host := r.Host
	forwardedHost := r.Header.Get("X-Forwarded-Host")

	if forwardedHost != "" {
		// If no allow-list is configured, fallback to forwarded host (local dev).
		// Otherwise, strictly enforce the allow-list.
		if len(s.AllowedHosts) == 0 {
			host = forwardedHost
		} else {
			for _, allowed := range s.AllowedHosts {
				if strings.EqualFold(allowed, forwardedHost) {
					host = forwardedHost
					break
				}
			}
		}
	}

	// Strip CR/LF to prevent log injection
	host = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, host)

	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
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

		reqHost := s.getBaseURL(r)

		sendXML(w, s.manager.pairPhase1(r.Context(), cacheKey, salt, clientCertStr, reqHost))
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

	// Handle POST /pin
	if r.Method == "POST" {
		type PinRequest struct {
			Pin    string `json:"pin"`
			Secret string `json:"secret"`
		}

		var req PinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Invalid request"))
			return
		}

		if req.Pin == "" || req.Secret == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Invalid request: missing pin or secret"))
			return
		}

		// Provide the pin to the pair manager
		klog.V(5).Infof("Received pin %s for secret %s", req.Pin, req.Secret)
		err := s.manager.PostPin(req.Secret, req.Pin)
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
	profiles := r.Context().Value(profilesContextKey{}).([]*v1alpha1types.Profile)

	appsList := make([]App, 0)
	for _, profile := range profiles {
		for _, appRef := range profile.Spec.Apps {
			app, err := s.AppLister.Get(appRef.Name)
			if err != nil {
				klog.Errorf("Failed to get app %s for profile %s: %s", appRef.Name, profile.Name, err)
				continue
			}
			appsList = append(appsList, App{
				AppSpec: app.Spec,
			})
		}
	}

	sendXML(w, AppListResponse{
		Response: Response{
			StatusCode: 200,
		},
		Apps: appsList,
	})
}

func (s *RESTServer) launchHandler(w http.ResponseWriter, r *http.Request) {
	clientIP := strings.Split(r.RemoteAddr, ":")[0]

	// 2025/03/03 11:34:48 HTTP/2.0 GET /launch map[additionalStates:[1] appid:[firefox] localAudioPlayMode:[0] mode:[1920x1080x60] rikey:[773448F67992470C5C62848D361E1025] rikeyid:[1311662065] sops:[0] surroundAudioInfo:[196610] uniqueid:[0123456789ABCDEF]] 127.0.0.1:65314
	// 2025/03/03 11:34:48 &{GET /launch?uniqueid=0123456789ABCDEF&appid=firefox&mode=1920x1080x60&additionalStates=1&sops=0&rikey=773448F67992470C5C62848D361E1025&rikeyid=1311662065&localAudioPlayMode=0&surroundAudioInfo=196610 HTTP/2.0 2 0 map[Accept:[*/*] Accept-Encoding:[gzip, deflate, br] Accept-Language:[en-US,en;q=0.9] User-Agent:[Moonlight/1243 CFNetwork/1568.100.1 Darwin/24.0.0]] 0x14000296570 <nil> 0 [] false 127.0.0.1:47984 map[] map[] <nil> map[] 127.0.0.1:65314 /launch?uniqueid=0123456789ABCDEF&appid=firefox&mode=1920x1080x60&additionalStates=1&sops=0&rikey=773448F67992470C5C62848D361E1025&rikeyid=1311662065&localAudioPlayMode=0&surroundAudioInfo=196610 0x1400016a540 <nil> <nil> /launch 0x140001ce0f0 0x14000186540 [] map[]}
	appID := r.URL.Query().Get("appid")
	// additionalStates := r.URL.Query().Get("additionalStates") // ???
	mode := r.URL.Query().Get("mode")
	rikey := r.URL.Query().Get("rikey")
	rikeyID := r.URL.Query().Get("rikeyid")
	// sops := r.URL.Query().Get("sops") // legacy GFE auto-optimize game settings. i dont think wolf has equivalent
	surroundAudioInfo := r.URL.Query().Get("surroundAudioInfo")

	if appID == "" {
		writeErrorResponse(w, 400, fmt.Errorf("appid required"))
		return
	}

	if rikey == "" {
		writeErrorResponse(w, 400, fmt.Errorf("rikey required"))
		return
	}

	if rikeyID == "" {
		writeErrorResponse(w, 400, fmt.Errorf("rikeyid required"))
		return
	}

	if mode == "" {
		mode = "1920x1080x60"
	}

	splitMode := strings.Split(mode, "x")
	if len(splitMode) != 3 {
		writeErrorResponse(w, 400, fmt.Errorf("invalid mode: %s", mode))
		return
	}

	width, err := strconv.Atoi(splitMode[0])
	if err != nil {
		writeErrorResponse(w, 400, fmt.Errorf("invalid mode: %s", mode))
		return
	}

	height, err := strconv.Atoi(splitMode[1])
	if err != nil {
		writeErrorResponse(w, 400, fmt.Errorf("invalid mode: %s", mode))
	}

	refreshRate, err := strconv.Atoi(splitMode[2])
	if err != nil {
		writeErrorResponse(w, 400, fmt.Errorf("invalid mode: %s", mode))
	}

	if surroundAudioInfo == "" {
		surroundAudioInfo = "196610"
	}

	surroundFlags, err := strconv.Atoi(surroundAudioInfo)
	if err != nil {
		writeErrorResponse(w, 400, fmt.Errorf("invalid surroundAudioInfo: %s", surroundAudioInfo))
		return
	}

	app, err := s.getAppByID(appID)
	if err != nil {
		writeErrorResponse(w, 404, fmt.Errorf("app not found: %s", err))
		return
	}

	profiles := r.Context().Value(profilesContextKey{}).([]*v1alpha1types.Profile)
	pairing := r.Context().Value(pairingContextKey{}).(*v1alpha1types.Pairing)

	// Find a profile that contains the requested app
	var targetProfile *v1alpha1types.Profile
	for _, p := range profiles {
		for _, appRef := range p.Spec.Apps {
			if appRef.Name == app.ObjectMeta.Name {
				targetProfile = p
				break
			}
		}
		if targetProfile != nil {
			break
		}
	}

	if targetProfile == nil {
		writeErrorResponse(w, 403, fmt.Errorf("no available profile contains app %s", app.ObjectMeta.Name))
		return
	}

	//!TOOD: May want to wait here, since we need the Service to stop pointing
	// at the old pod. It is very likely to happen before operator syncs and
	// can create session, but perhaps should still check after operator returns
	// the session URL.
	if err := s.stopSessionsForProfile(targetProfile, false); err != nil && !k8serrors.IsNotFound(err) {
		writeErrorResponse(w, 500, fmt.Errorf("failed to stop existing sessions: %s", err))
		return
	}

	klog.Infof("Launching app %s for profile %s", app.ObjectMeta.Name, targetProfile.ObjectMeta.Name)
	session, err := s.SessionClient.Create(
		r.Context(),
		&v1alpha1types.Session{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: fmt.Sprintf("%s-%s-", targetProfile.Name, app.Name),
				Namespace:    pairing.Namespace,
				Labels: map[string]string{
					"direwolf":         "true",
					"direwolf/app":     app.ObjectMeta.Name,
					"direwolf/profile": targetProfile.ObjectMeta.Name,
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
				ProfileReference: v1alpha1types.ProfileReference{
					Name: targetProfile.ObjectMeta.Name,
				},
				//!TODO: Unused. v1alpha2 Gateway types are not widely supported
				GatewayReference: v1alpha1types.GatewayReference{
					Name:      "unused",
					Namespace: "unused",
				},
				Config: v1alpha1types.SessionInfo{
					ClientIP:           clientIP,
					AESIV:              rikeyID,
					AESKey:             rikey,
					SurroundAudioFlags: surroundFlags,
					VideoWidth:         width,
					VideoHeight:        height,
					VideoRefreshRate:   refreshRate,
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

	// Wait for session to be created by the direwolf controller
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

func (s *RESTServer) resumeHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: Wolf API current cannot support a "resume" to reuse the existing
	// display/controllers. So we relaunch instead :(
	s.launchHandler(w, r)
}
func (s *RESTServer) cancelHandler(w http.ResponseWriter, r *http.Request) {
	profiles := r.Context().Value(profilesContextKey{}).([]*v1alpha1types.Profile)

	anyErr := false
	for _, profile := range profiles {
		if err := s.stopSessionsForProfile(profile, true); err != nil && !k8serrors.IsNotFound(err) {
			anyErr = true
		}
	}

	if anyErr {
		writeErrorResponse(w, 500, fmt.Errorf("failed to cancel one or more sessions"))
		return
	}
	sendXML(w, Response{StatusCode: 200})
}

func (s *RESTServer) stopSessionsForProfile(profile *v1alpha1types.Profile, shouldWait bool) error {
	sessions, err := s.SessionLister.List(labels.SelectorFromSet(labels.Set{
		"direwolf/profile": profile.Name,
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
		klog.Warningf("Not stopping any sessions. No sessions found for profile %s", profile.Name)
		return k8serrors.NewNotFound(v1alpha1types.Resource("session"), profile.Name)
	}

	if shouldWait {
		// Wait for pods to be cleaned up
		err = wait.PollUntilContextTimeout(context.Background(), 250*time.Millisecond, 25*time.Second, true, func(ctx context.Context) (bool, error) {
			pods, err := s.PodLister.List(labels.SelectorFromSet(labels.Set{
				"direwolf/profile": profile.Name,
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
			if klog.V(5).Enabled() {
				klog.V(5).Infof("%s %s %s %v %s", r.Proto, r.Method, r.URL.Path, r.URL.Query(), r.RemoteAddr)
			} else {
				klog.Infof("%s %s %s %s", r.Proto, r.Method, r.URL.Path, r.RemoteAddr)
			}
			next.ServeHTTP(w, r)
			klog.V(1).Infof("Completed in %s", time.Since(start))
		} else {
			next.ServeHTTP(w, r)
		}
	})
}

type profilesContextKey struct{}
type pairingContextKey struct{}

// Grabs fingerprint of client cert, finds associated profiles in backend and attaches
// them to the request context.
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

		// List all profiles and filter those that include this pairing
		allProfiles, err := s.ProfileLister.List(labels.Everything())
		if err != nil {
			writeErrorResponse(w, 500, fmt.Errorf("failed to list profiles: %s", err))
			return
		}

		var availableProfiles []*v1alpha1types.Profile
		for _, p := range allProfiles {
			for _, pRef := range p.Spec.Pairings {
				if pRef.Name == pairing.Name {
					availableProfiles = append(availableProfiles, p)
					break
				}
			}
		}

		ctx := context.WithValue(r.Context(), profilesContextKey{}, availableProfiles)
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
