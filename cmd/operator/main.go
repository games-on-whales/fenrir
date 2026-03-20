package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"time"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/controllers"
	"games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
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
	lbSharingKey := flag.String("lb-sharing-key", os.Getenv("POD_NAMESPACE"), "LoadBalancer sharing key")

	// service used by moonlight-proxy
	proxySvcName := flag.String("proxy-service-name", "direwolf", "Name of the proxy service")

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
	userInformer := direwolfFactory.Direwolf().V1alpha1().Users().Informer()
	sessionInformer := direwolfFactory.Direwolf().V1alpha1().Sessions().Informer()
	direwolfFactory.Start(appContext.Done())

	k8sFactory := informers.NewSharedInformerFactoryWithOptions(
		k8sClient, 15*time.Minute, informers.WithNamespace(*namespace))
	deploymentInformer := k8sFactory.Apps().V1().Deployments().Informer()

	serviceInformer := k8sFactory.Core().V1().Services().Informer()

	k8sFactory.Start(appContext.Done())

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
		generic.NewInformer[*direwolfv1alpha1.User](userInformer),
		generic.NewInformer[*appsv1.Deployment](deploymentInformer),
		controllers.SessionControllerOptions{
			WolfAgentImage: *wolfAgentImage,
			LBSharingKey:   *lbSharingKey,
		},
	)

	// User Controller
	// will need a lot of debugging
	userController := controllers.NewUserController(
		k8sClient,
		direwolfClient.DirewolfV1alpha1().Users(*namespace),
		generic.NewInformer[*direwolfv1alpha1.User](userInformer),
		generic.NewInformer[*corev1.Service](serviceInformer),
		controllers.UserControllerOptions{
			ProxyServiceName: *proxySvcName,
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

				// run User Controller
				go func() {
					if err := userController.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
						klog.Errorf("error running user controller: %v", err)
					}
				}()

				// run Session Controller
				go func() {
					if err := sessionController.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
						klog.Errorf("error running session controller: %v", err)
					}
				}()

				<-ctx.Done()
			},
			OnStoppedLeading: func() {
				appCancel()
			},
		},
	})
	klog.Info("Shutting down")
}
