package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/controllers"
	"games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
)

func main() {
	appContext, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	im := os.Getenv("AGENT_IMAGE")
	if im == "" {
		im = "ghcr.io/games-on-whales/wolf-agent:main"
	}
	wolfAgentImage := flag.String("wolf-agent-image", im, "Wolf Agent image")
	holderIdentity := flag.String("holder-identity", os.Getenv("POD_NAME"), "Holder identity")
	namespace := flag.String("namespace", os.Getenv("POD_NAMESPACE"), "Namespace to watch")
	// Below Linux's ephemeral range (32768-60999), where an outbound socket
	// on the node could hold a port a session pod needs to bind, and below
	// the NodePort range (30000-32767).
	sessionPortRange := flag.String("session-port-range", "20000-20999",
		"Host port range (MIN-MAX) session pods get their port blocks from")
	sessionNodeSelector := flag.String("session-node-selector", "",
		"Node labels session pods are pinned to, e.g. kubernetes.io/hostname=talos04.xerktech.com")
	sessionTolerations := flag.String("session-tolerations", "",
		"Comma-separated taints (key[=value]:Effect) session pods tolerate, e.g. nvidia.com/gpu=present:NoSchedule")
	disconnectGracePeriod := flag.Duration("disconnect-grace-period", 10*time.Minute,
		"How long a session's pod is kept after its Moonlight client disconnects, so /resume can re-attach")
	libraryPort := flag.Int("library-port", 0,
		"Port for the Library page, reached only through the authenticating Ingress. 0 disables the Library")
	libraryTrustedProxies := flag.String("library-trusted-proxies", "",
		"Comma-separated CIDRs of the Ingress controller, the only peers allowed to the Library page. Required with --library-port")
	libraryImage := flag.String("library-image", cmp.Or(os.Getenv("LIBRARY_IMAGE"), "ghcr.io/games-on-whales/fenrir/library:main"),
		"Library (Steam + Heroic desktop) image")
	libraryHomePVC := flag.String("library-home-pvc", "", "PVC holding the shared Steam home. Required with --library-port")
	libraryGamesPVC := flag.String("library-games-pvc", "", "PVC holding the shared game library. Required with --library-port")
	libraryGamesPath := flag.String("library-games-path", "/games", "Where the Library mounts the game library")
	libraryIdleTimeout := flag.Duration("library-idle-timeout", controllers.DefaultLibraryIdleTimeout,
		"How long the Library may go without browser traffic (and with nothing downloading) before it is stopped")
	klog.InitFlags(nil)
	flag.Parse()

	portRange, err := controllers.ParsePortRange(*sessionPortRange)
	if err != nil {
		klog.Fatalf("--session-port-range: %v", err)
	}
	nodeSelector, err := labels.ConvertSelectorToLabelsMap(*sessionNodeSelector)
	if err != nil {
		klog.Fatalf("--session-node-selector: %v", err)
	}
	tolerations, err := controllers.ParseTolerations(*sessionTolerations)
	if err != nil {
		klog.Fatalf("--session-tolerations: %v", err)
	}

	if *disconnectGracePeriod < 0 {
		klog.Fatalf("--disconnect-grace-period must not be negative, got %s", *disconnectGracePeriod)
	}

	libraryTrusted, err := util.ParsePrefixes(*libraryTrustedProxies)
	if err != nil {
		klog.Fatalf("--library-trusted-proxies: %v", err)
	}
	if *libraryPort != 0 && (len(libraryTrusted) == 0 || *libraryHomePVC == "" || *libraryGamesPVC == "") {
		klog.Fatal("--library-port requires --library-trusted-proxies, --library-home-pvc and --library-games-pvc")
	}
	if *libraryPort < 0 || *libraryPort > 65535 {
		klog.Fatalf("--library-port must be 0-65535, got %d", *libraryPort)
	}
	for _, p := range libraryTrusted {
		if p.Bits() == 0 {
			klog.Fatalf("--library-trusted-proxies: %s would trust every peer", p)
		}
	}
	if pathErr := controllers.ValidateLibraryGamesPath(*libraryGamesPath); *libraryPort != 0 && pathErr != nil {
		klog.Fatalf("--library-games-path: %v", pathErr)
	}
	if *libraryIdleTimeout <= 0 {
		klog.Fatalf("--library-idle-timeout must be positive, got %s", *libraryIdleTimeout)
	}

	restConfig, err := util.GetRESTConfig()
	if err != nil {
		klog.Fatal("Error getting Kubernetes config", err)
	}
	k8sClient, direwolfClient, gatewayClient, _, err := util.GetKubernetesClients()
	if err != nil {
		klog.Fatal("Error getting Kubernetes clients", err)
	}

	// Just create all the informers and warm them up before starting anything
	// to keep things simple.
	direwolfFactory := externalversions.NewSharedInformerFactoryWithOptions(
		direwolfClient, 15*time.Minute, externalversions.WithNamespace(*namespace))
	appInformer := direwolfFactory.Direwolf().V1alpha1().Apps().Informer()
	userInformer := direwolfFactory.Direwolf().V1alpha1().Users().Informer()
	sessionInformer := direwolfFactory.Direwolf().V1alpha1().Sessions().Informer()
	direwolfFactory.Start(appContext.Done())
	defer direwolfFactory.Shutdown()

	// Only session pods: the operator has no business caching the rest.
	k8sFactory := informers.NewSharedInformerFactoryWithOptions(
		k8sClient, 15*time.Minute, informers.WithNamespace(*namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = labels.SelectorFromSet(labels.Set{
				direwolfv1alpha1.SessionPodLabel: direwolfv1alpha1.SessionPodLabelValue,
			}).String()
		}))
	podInformer := k8sFactory.Core().V1().Pods().Informer()
	k8sFactory.Start(appContext.Done())
	defer k8sFactory.Shutdown()

	// The Library pod, apart from session pods: the nri-input plugin hands
	// input devices to anything carrying the session label.
	libraryFactory := informers.NewSharedInformerFactoryWithOptions(
		k8sClient, 15*time.Minute, informers.WithNamespace(*namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = labels.SelectorFromSet(labels.Set{
				direwolfv1alpha1.LibraryPodLabel: direwolfv1alpha1.LibraryPodLabelValue,
			}).String()
		}))
	libraryPodInformer := libraryFactory.Core().V1().Pods().Informer()
	libraryFactory.Start(appContext.Done())
	defer libraryFactory.Shutdown()

	k8sFactory.WaitForCacheSync(appContext.Done())
	direwolfFactory.WaitForCacheSync(appContext.Done())
	libraryFactory.WaitForCacheSync(appContext.Done())

	// Run a leader election so that only one instance of operator is running
	// at a time in the cluster for a single namespace.
	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		*namespace,
		"direwolf-controller",
		k8sClient.CoreV1(),
		k8sClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity: *holderIdentity,
		},
	)
	if err != nil {
		klog.Fatal("Error creating resource lock", err)
	}

	sessionController := controllers.NewSessionController(
		k8sClient,
		gatewayClient.GatewayV1alpha2().TCPRoutes(*namespace),
		gatewayClient.GatewayV1alpha2().UDPRoutes(*namespace),
		direwolfClient.DirewolfV1alpha1().Sessions(*namespace),
		generic.NewInformer[*direwolfv1alpha1.Session](sessionInformer),
		generic.NewInformer[*direwolfv1alpha1.App](appInformer),
		generic.NewInformer[*direwolfv1alpha1.User](userInformer),
		generic.NewInformer[*corev1.Pod](podInformer),
		controllers.SessionControllerOptions{
			WolfAgentImage:      *wolfAgentImage,
			SessionPortRange:    portRange,
			SessionNodeSelector: nodeSelector,
			SessionTolerations:  tolerations,

			DisconnectGracePeriod: *disconnectGracePeriod,
		},
	)

	var libraryController *controllers.LibraryController
	if *libraryPort != 0 {
		libraryController = controllers.NewLibraryController(
			*namespace,
			k8sClient,
			direwolfClient.DirewolfV1alpha1().Sessions(*namespace),
			generic.NewInformer[*corev1.Pod](libraryPodInformer),
			controllers.NewPodExecutor(restConfig, k8sClient),
			controllers.LibraryControllerOptions{
				Image:        *libraryImage,
				HomePVC:      *libraryHomePVC,
				GamesPVC:     *libraryGamesPVC,
				GamesPath:    *libraryGamesPath,
				NodeSelector: nodeSelector,
				Tolerations:  tolerations,
				IdleTimeout:  *libraryIdleTimeout,
			},
		)
		// Every replica serves the page (activity is recorded on the pod);
		// only the leader runs the idle check.
		libraryServer := controllers.NewLibraryServer(
			libraryController,
			generic.NewLister[*corev1.Pod](libraryPodInformer.GetIndexer()).Namespaced(*namespace),
			libraryTrusted,
		)
		go func() {
			if err := libraryServer.Run(appContext, *libraryPort); err != nil {
				klog.Errorf("Library server failed: %v", err)
				appCancel()
			}
		}()
	}

	leaderelection.RunOrDie(appContext, leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Info("started leading")
				if libraryController != nil {
					go func() {
						err := libraryController.Run(appContext)
						if err != nil && !errors.Is(err, context.Canceled) {
							klog.Errorf("error running library controller: %v", err)
							appCancel()
						}
					}()
				}
				err := sessionController.Run(appContext)
				if err != nil && !errors.Is(err, context.Canceled) {
					klog.Errorf("error running session controller: %v", err)
					appCancel()
				}
			},
			OnStoppedLeading: func() {
				appCancel()
			},
			OnNewLeader: func(identity string) {
				klog.InfoS("new leader", "holderIdentity", identity)
			},
		},
	})
	klog.Info("Shutting down")
}
