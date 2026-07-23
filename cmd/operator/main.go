package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	wImagePullPolicy := os.Getenv("AGENT_IMAGE_PULL_POLICY")
	switch wImagePullPolicy {
	case "Always", "IfNotPresent", "Never":
		klog.Infof("Wolf-Agent Image Pull policy %s", wImagePullPolicy)
	default:
		klog.Infof("Wolf-Agent Image Pull policy %q is not valid: defaulting to %q", wImagePullPolicy, "IfNotPresent")
		wImagePullPolicy = "IfNotPresent"
	}
	wolfAgentImage := flag.String("wolf-agent-image", im, "Wolf Agent image")
	wolfAgentImagePullPolicy := flag.String("wolf-agent-image-pull-policy", wImagePullPolicy, "wolf agent image pull policy (Always, IfNotPresent, Never)")
	holderIdentity := flag.String("holder-identity", os.Getenv("POD_NAME"), "Holder identity")
	namespace := flag.String("namespace", os.Getenv("POD_NAMESPACE"), "Namespace to watch")
	lbSharingKey := flag.String("lb-sharing-key", os.Getenv("POD_NAMESPACE"), "LoadBalancer sharing key")

	klog.InitFlags(nil)
	flag.Parse()

	k8sClient, direwolfClient, gatewayClient, _, err := util.GetKubernetesClients()
	if err != nil {
		klog.Fatal("Error getting Kubernetes clients", err)
	}

	// Just create all the informers and warm them up before starting anything
	// to keep things simple.
	direwolfFactory := externalversions.NewSharedInformerFactoryWithOptions(
		direwolfClient, 15*time.Minute, externalversions.WithNamespace(*namespace))
	appInformer := direwolfFactory.Direwolf().V1alpha1().Apps().Informer()
	profileInformer := direwolfFactory.Direwolf().V1alpha1().Profiles().Informer()
	sessionInformer := direwolfFactory.Direwolf().V1alpha1().Sessions().Informer()
	direwolfFactory.Start(appContext.Done())
	defer direwolfFactory.Shutdown()
	k8sFactory := informers.NewSharedInformerFactoryWithOptions(
		k8sClient, 15*time.Minute, informers.WithNamespace(*namespace))
	deploymentInformer := k8sFactory.Apps().V1().Deployments().Informer()

	k8sFactory.Start(appContext.Done())
	defer k8sFactory.Shutdown()

	klog.Info("Waiting for caches to sync")
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

	// Session Controller
	sessionController := controllers.NewSessionController(
		k8sClient,
		gatewayClient.GatewayV1alpha2().TCPRoutes(*namespace),
		gatewayClient.GatewayV1alpha2().UDPRoutes(*namespace),
		direwolfClient.DirewolfV1alpha1().Sessions(*namespace),
		generic.NewInformer[*direwolfv1alpha1.Session](sessionInformer),
		generic.NewInformer[*direwolfv1alpha1.App](appInformer),
		generic.NewInformer[*direwolfv1alpha1.Profile](profileInformer),
		generic.NewInformer[*appsv1.Deployment](deploymentInformer),
		controllers.SessionControllerOptions{
			WolfAgentImage:           *wolfAgentImage,
			WolfAgentImagePullPolicy: *wolfAgentImagePullPolicy,
			LBSharingKey:             *lbSharingKey,
		},
	)

	leaderelection.RunOrDie(appContext, leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(_ context.Context) {
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
