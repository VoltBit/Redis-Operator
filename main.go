package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dbv1 "github.com/PayU/redis-operator/api/v1"
	"github.com/PayU/redis-operator/controllers"
	"github.com/PayU/redis-operator/controllers/rediscli"
	// +kubebuilder:scaffold:imports
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = dbv1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr, namespace, enableLeaderElection, devmode string

	flag.StringVar(&metricsAddr, "metrics-addr", "0.0.0.0:9808", "The address the metric endpoint binds to.")
	flag.StringVar(&namespace, "namespace", "default", "The namespace the operator will manage.")
	flag.StringVar(&devmode, "devmode", "false", "Development mode toggle.")
	flag.StringVar(&enableLeaderElection, "enable-leader-election", "true",
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.Parse()

	setupLogger := zap.New(zap.UseDevMode(devmode == "true")).WithName("setup")
	// ctrl.SetLogger(setupLogger)

	retryLockDuration := 4 * time.Second

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:             scheme,
		Namespace:          namespace,
		MetricsBindAddress: metricsAddr,
		Port:               9443,
		LeaderElection:     enableLeaderElection == "true",
		LeaderElectionID:   "1747e98e.payu.com",
		RetryPeriod:        &retryLockDuration,
	})
	if err != nil {
		setupLogger.Error(err, "failed to create new manager")
		os.Exit(1)
	}

	rdcLogger := zap.New(zap.UseDevMode(devmode == "true")).
		WithName("controllers").
		WithName("RedisCluster")
	configLogger := zap.New(zap.UseDevMode(devmode == "true")).
		WithName("controllers").
		WithName("RedisConfig")

	if err = (&controllers.RedisClusterReconciler{
		Client:   mgr.GetClient(),
		Log:      rdcLogger,
		Scheme:   mgr.GetScheme(),
		RedisCLI: rediscli.NewRedisCLI(rdcLogger),
		State:    controllers.NotExists,
	}).SetupWithManager(mgr); err != nil {
		setupLogger.Error(err, "unable to create controller", "controller", "RedisCluster")
		os.Exit(1)
	}

	if err = (&controllers.RedisConfigReconciler{
		Client:   mgr.GetClient(),
		Log:      configLogger,
		Scheme:   mgr.GetScheme(),
		RedisCLI: rediscli.NewRedisCLI(configLogger),
	}).SetupWithManager(mgr); err != nil {
		setupLogger.Error(err, "unable to create controller", "controller", "RedisConfig")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	setupLogger.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLogger.Error(err, "failed to start the manager")
		os.Exit(1)
	}
}
