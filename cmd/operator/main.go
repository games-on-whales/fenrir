package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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

	k8sFactory := informers.NewSharedInformerFactoryWithOptions(
		k8sClient, 15*time.Minute, informers.WithNamespace(*namespace))
	deploymentInformer := k8sFactory.Apps().V1().Deployments().Informer()
	k8sFactory.Start(appContext.Done())
	defer k8sFactory.Shutdown()

	k8sFactory.WaitForCacheSync(appContext.Done())
	direwolfFactory.WaitForCacheSync(appContext.Done())

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
		generic.NewInformer[*appsv1.Deployment](deploymentInformer),
		controllers.SessionControllerOptions{
			WolfAgentImage:      *wolfAgentImage,
			SessionPortRange:    portRange,
			SessionNodeSelector: nodeSelector,
			SessionTolerations:  tolerations,
		},
	)

	leaderelection.RunOrDie(appContext, leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Info("started leading")
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
