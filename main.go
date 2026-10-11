// Command cloudflare-quick-tunnel-operator exposes annotated Kubernetes Services to the
// public internet through Cloudflare Quick Tunnels (trycloudflare.com),
// without a Cloudflare account, a domain, or any inbound ports.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Inokinoki/cloudflare-quick-tunnel-operator/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gwv1.Install(scheme))
}

func main() {
	var metricsAddr, probeAddr, cloudflaredImage, namespace string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for the health probe endpoint")
	flag.StringVar(&cloudflaredImage, "cloudflared-image", controller.DefaultCloudflaredImage, "cloudflared image used for tunnel pods")
	flag.StringVar(&namespace, "namespace", "", "Restrict reconciliation to this namespace (empty = all namespaces)")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election (recommended when running multiple replicas)")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cloudflare-quick-tunnel-operator.leader-election",
	}
	if namespace != "" {
		mgrOpts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// The manager client cannot stream pod logs; a plain typed client can.
	coreClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create core client")
		os.Exit(1)
	}

	reconciler := &controller.ServiceReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		CloudflaredImage: cloudflaredImage,
		Watchdog:         controller.NewTunnelWatchdog(mgr.GetClient(), coreClient),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	ingRec := &controller.IngressReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		CloudflaredImage: cloudflaredImage,
		Watchdog:         reconciler.Watchdog,
	}
	if err := ingRec.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create Ingress controller")
		os.Exit(1)
	}

	if gatewayAPIAvailable(mgr.GetConfig()) {
		gcRec := &controller.GatewayClassReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		gwRec := &controller.GatewayReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		hrRec := &controller.HTTPRouteReconciler{
			Client:           mgr.GetClient(),
			Scheme:           mgr.GetScheme(),
			CloudflaredImage: cloudflaredImage,
			Watchdog:         reconciler.Watchdog,
		}
		for _, c := range []interface{ SetupWithManager(ctrl.Manager) error }{gcRec, gwRec, hrRec} {
			if err := c.SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create Gateway API controller")
				os.Exit(1)
			}
		}
		setupLog.Info("Gateway API support enabled", "controllerName", controller.GatewayControllerName)
	} else {
		setupLog.Info("Gateway API CRDs (gateway.networking.k8s.io/v1) not installed; " +
			"skipping Gateway API support, Service annotations still work")
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting cloudflare-quick-tunnel-operator",
		"cloudflaredImage", cloudflaredImage, "namespace", namespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "operator exited with error")
		os.Exit(1)
	}
}

// gatewayAPIAvailable reports whether the Gateway API CRDs are installed;
// starting informers for absent CRDs would crash the manager.
func gatewayAPIAvailable(cfg *rest.Config) bool {
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false
	}
	_, err = disc.ServerResourcesForGroupVersion("gateway.networking.k8s.io/v1")
	return err == nil
}
