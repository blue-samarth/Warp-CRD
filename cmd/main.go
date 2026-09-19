package main

import (
	"crypto/tls"
	"flag"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/blue-samarth/Warp-CRD/internal/resources"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
	"github.com/blue-samarth/Warp-CRD/internal/controller"
	webhookv1alpha1 "github.com/blue-samarth/Warp-CRD/internal/webhook/v1alpha1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntimeMust(clientgoscheme.AddToScheme(scheme))
	utilruntimeMust(v1alpha1.AddToScheme(scheme))
}

func utilruntimeMust(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	var metricsAddr, probeAddr, webhookCertDir, metricsCertDir string
	var enableLeaderElection, enableWebhooks, secureMetrics bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "address the metric endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the probe endpoint binds to")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "directory holding the webhook serving certs")
	flag.StringVar(&metricsCertDir, "metrics-cert-dir", "", "directory holding the metrics serving certs; empty serves an in-memory certificate for localhost")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "enable leader election for controller manager")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true, "serve the admission webhooks")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "serve metrics over HTTPS")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	metricsOpts := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
	}
	if secureMetrics {
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
		metricsOpts.CertDir = metricsCertDir
	}

	// Every object the operator creates carries this label, so the informers
	// only have to hold what it manages. Adoption reads bypass the cache, so a
	// foreign object missing from it cannot be silently taken over.
	managed := labels.SelectorFromSet(labels.Set{
		resources.ManagedByLabel: resources.ManagedByValue,
	})
	byManaged := cache.ByObject{Label: managed}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsOpts,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&appsv1.Deployment{}:                     byManaged,
				&corev1.Service{}:                        byManaged,
				&networkingv1.Ingress{}:                  byManaged,
				&autoscalingv2.HorizontalPodAutoscaler{}: byManaged,
			},
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			CertDir: webhookCertDir,
			TLSOpts: []func(*tls.Config){func(c *tls.Config) { c.MinVersion = tls.VersionTLS12 }},
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "webapp-operator.webapps.example.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.WebAppReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("webapp-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WebApp")
		os.Exit(1)
	}

	if enableWebhooks {
		if err := webhookv1alpha1.SetupWebAppWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "WebApp")
			os.Exit(1)
		}
		if err := webhookv1alpha1.SetupWebAppPolicyWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "WebAppPolicy")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	readyz := healthz.Ping
	if enableWebhooks {
		readyz = mgr.GetWebhookServer().StartedChecker()
	}
	if err := mgr.AddReadyzCheck("readyz", readyz); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
