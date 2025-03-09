package moonlight

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image/png"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
	"golang.org/x/image/webp"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"

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

	SessionClient v1alpha1client.SessionInterface

	RESTServerOptions
}

func NewRESTServer(
	manager *PairingManager,
	pairingsLister generic.NamespacedLister[*v1alpha1types.Pairing],
	userLister generic.NamespacedLister[*v1alpha1types.User],
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
		UserLister:        userLister,
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

func (s *RESTServer) Run(ctx context.Context) {
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

	go func() {
		log.Printf("HTTP server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP Server failed: %s", err)
		}
	}()

	go func() {
		log.Printf("HTTPS server listening on %s", secureServer.Addr)
		if err := secureServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTPS Server failed: %s", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutting down server...")
	server.Shutdown(context.Background())
	secureServer.Shutdown(context.Background())
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

// Multiplex the multiple phases of pairing into a single handler
func (s *RESTServer) pairHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("Handling pair request from %s", r.RemoteAddr)

	clientID := r.URL.Query().Get("uniqueid")
	clientIP := strings.Split(r.RemoteAddr, ":")[0]
	cacheKey := fmt.Sprintf("%s@%s", clientID, clientIP)

	if clientID == "" {
		sendXML(w, failPair("uniqueid required"))
		return
	}

	if r.URL.Query().Has("salt") {
		log.Printf("Pairing phase 1 with %s\n", cacheKey)
		salt := r.URL.Query().Get("salt")
		clientCertStr := r.URL.Query().Get("clientcert")

		sendXML(w, s.manager.pairPhase1(cacheKey, salt, clientCertStr))
		return
	} else if r.URL.Query().Has("clientchallenge") {
		log.Printf("Pairing phase 2 with %s\n", cacheKey)
		clientChallenge := r.URL.Query().Get("clientchallenge")

		sendXML(w, s.manager.pairPhase2(cacheKey, clientChallenge))
		return
	} else if r.URL.Query().Has("serverchallengeresp") {
		log.Printf("Pairing phase 3 with %s\n", cacheKey)
		serverChallengeResp := r.URL.Query().Get("serverchallengeresp")

		sendXML(w, s.manager.pairPhase3(cacheKey, serverChallengeResp))
		return
	} else if r.URL.Query().Has("clientpairingsecret") {
		log.Printf("Pairing phase 4 with %s\n", cacheKey)
		clientPairingSecret := r.URL.Query().Get("clientpairingsecret")

		sendXML(w, s.manager.pairPhase4(cacheKey, clientPairingSecret))
		return
	} else if phrase := r.URL.Query().Get("phrase"); phrase == "pairchallenge" {
		log.Printf("Pairing phase 5 with %s\n", cacheKey)
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
	log.Printf("Handling unpair request from %s", r.RemoteAddr)
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
	log.Printf("Handling %v pin request from %s", r.Method, r.RemoteAddr)
	// Handle GET /pin/<secret>
	if r.Method == "GET" {
		// Just post the static pin page
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(pinHTML))
		return
	}

	// Handle POST /pin
	if r.Method == "POST" {
		type PinRequest struct {
			Pin    string `json:"pin"`
			Secret string `secret:"secret"`
		}

		var req PinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Invalid request"))
			return
		}

		if req.Pin == "" || req.Secret == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Invalid request"))
			return
		}

		// Provide the pin to the pair manager
		log.Printf("Received pin %s for secret %s", req.Pin, req.Secret)
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

func (s *RESTServer) launchHandler(w http.ResponseWriter, r *http.Request) {

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

	user := r.Context().Value(userContextKey{}).(*v1alpha1types.User)
	pairing := r.Context().Value(pairingContextKey{}).(*v1alpha1types.Pairing)

	// Delete any sessions with the same user x game x pairing
	existingSessions, err := s.SessionLister.List(labels.SelectorFromSet(labels.Set{
		"direwolf/user": user.Name,
		"direwolf/app":  app.ObjectMeta.Name,
	}))
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to list sessions: %s", err))
		return
	}

	for _, session := range existingSessions {
		// If it has same annotation for pairing, delete it. Pairings use annotation
		if pairingName, exists := session.Annotations["direwolf/pairing"]; exists && pairingName == pairing.Name {
			log.Printf("Deleting existing session %s", session.Name)
			if err := s.SessionClient.Delete(r.Context(), session.Name, metav1.DeleteOptions{}); err != nil {
				writeErrorResponse(w, 500, fmt.Errorf("failed to delete session: %s", err))
				return
			}
		}
	}

	log.Printf("Launching app %s for user %s", app.ObjectMeta.Name, user.ObjectMeta.Name)
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
				//!TODO: Unused. v1alpha2 Gateway types are not widely supported
				GatewayReference: v1alpha1types.GatewayReference{
					Name:      "unused",
					Namespace: "unused",
				},
				Config: v1alpha1types.SessionInfo{
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
	time.Sleep(3 * time.Second)
	sendXML(w, LaunchResponse{
		Response: Response{
			StatusCode: 200,
		},
		RTSPSessionURL: streamURL,
		GameSession:    1,
	})
}

func (s *RESTServer) resumeHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: How is resume different from launch?
	// Does launch imply a new session?
	s.launchHandler(w, r)
}

func (s *RESTServer) cancelHandler(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userContextKey{}).(*v1alpha1types.User)
	pairing := r.Context().Value(pairingContextKey{}).(*v1alpha1types.Pairing)

	// Delete any sessions with the same user x pairing
	// Unforuntately cancel doesnt seem to be able to cancel a specific app
	existingSessions, err := s.SessionLister.List(labels.SelectorFromSet(labels.Set{
		"direwolf/user": user.Name,
	}))
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to list sessions: %s", err))
		return
	}

	for _, session := range existingSessions {
		// If it has same annotation for pairing, delete it. Pairings use annotation
		if pairingName, exists := session.Annotations["direwolf/pairing"]; exists && pairingName == pairing.Name {
			log.Printf("Deleting existing session %s", session.Name)
			if err := s.SessionClient.Delete(r.Context(), session.Name, metav1.DeleteOptions{}); err != nil {
				writeErrorResponse(w, 500, fmt.Errorf("failed to delete session: %s", err))
				return
			}
		}
	}

	// Wait for pods to be cleaned up
	err = wait.PollUntilContextTimeout(r.Context(), 250*time.Millisecond, 25*time.Second, true, func(ctx context.Context) (bool, error) {
		pods, err := s.PodLister.List(labels.SelectorFromSet(labels.Set{
			"direwolf/user": user.Name,
		}))
		if err != nil {
			return false, err
		}
		return len(pods) == 0, nil
	})
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to wait for pods to be cleaned up: %s", err))
		return
	}

	sendXML(w, Response{StatusCode: 200})
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
		log.Printf("Failed to decode webp: %s", err)
		return
	}

	pngData := new(bytes.Buffer)
	if err := png.Encode(pngData, img); err != nil {
		log.Printf("Failed to encode png: %s", err)
		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(pngData.Len()))
	_, err = io.Copy(w, pngData)
	if err != nil {
		log.Printf("Failed to write png: %s", err)
		return
	}
	log.Printf("Sent app asset for %s", appID)
}

func writeErrorResponse(w http.ResponseWriter, status int, err error) {
	log.Printf("%s", err)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	w.Write(fmt.Appendf(nil, `<root status_code="%d"></root>`, status))

	log.Printf("Sent response: %d %v", status, err)
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
		log.Printf("Sent response: %s\n", string(bytes))
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		if r.URL.Path != "/serverinfo" {

			log.Printf("%s %s %s %v %s", r.Proto, r.Method, r.URL.Path, r.URL.Query(), r.RemoteAddr)
			next.ServeHTTP(w, r)
			log.Printf("Completed in %s", time.Since(start))
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
