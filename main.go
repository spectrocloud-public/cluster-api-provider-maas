/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"math/rand"
	"os"
	"time"

	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/controllers/remote"

	"github.com/spectrocloud/cluster-api-provider-maas/controllers"
	maasmachine "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/machine"

	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/feature"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	//flags
	metricsBindAddr      string
	enableLeaderElection bool
	syncPeriod           time.Duration
	machineConcurrency   int
	healthAddr           string
	webhookPort          int
	watchNamespace       string
	releaseErase         string
)

func init() {
	klog.InitFlags(nil)

	_ = clientgoscheme.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = infrav1beta1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme) // Add this for DaemonSet support
	// +kubebuilder:scaffold:scheme
}

func main() {
	rand.Seed(time.Now().UnixNano())

	initFlags(pflag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	ctrl.SetLogger(textlogger.NewLogger(textlogger.NewConfig()))

	eraseMode, err := maasmachine.ParseEraseMode(releaseErase)
	if err != nil {
		setupLog.Error(err, "invalid --release-erase")
		os.Exit(1)
	}
	maasmachine.ReleaseErase = eraseMode

	if watchNamespace != "" {
		setupLog.Info("Watching cluster-api objects only in namespace for reconciliation", "namespace", watchNamespace)
	}

	ctrl.SetLogger(textlogger.NewLogger(textlogger.NewConfig()))

	restCfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsBindAddr,
		},
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "controller-leader-election-capmaas",
		Cache: cache.Options{
			SyncPeriod: &syncPeriod,
		},
		HealthProbeBindAddress: healthAddr,
		WebhookServer: webhookserver.NewServer(webhookserver.Options{
			Port: webhookPort,
		}),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Setup the context that's going to be used in controllers and for the manager.
	// v1alpha4
	//ctx := ctrl.SetupSignalHandler()
	ctx := context.Background()

	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to create ready check")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to create health check")
		os.Exit(1)
	}

	// Set up a ClusterCache to provide to controllers requiring a connection to a remote cluster.
	clusterCache, err := clustercache.SetupWithManager(ctx, mgr, clustercache.Options{
		SecretClient: mgr.GetClient(),
		Client: clustercache.ClientOptions{
			UserAgent: remote.DefaultClusterAPIUserAgent("cluster-api-provider-maas"),
		},
	}, concurrency(1))
	if err != nil {
		setupLog.Error(err, "unable to create cluster cache")
		os.Exit(1)
	}

	if err := (&controllers.MaasMachineReconciler{
		Client:   mgr.GetClient(),
		Log:      ctrl.Log.WithName("controllers").WithName("MaasMachine"),
		Recorder: mgr.GetEventRecorderFor("maasmachine-controller"),
		Tracker:  clusterCache,
	}).SetupWithManager(ctx, mgr, concurrency(machineConcurrency)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MaasMachine")
		os.Exit(1)
	}

	if err := (&controllers.MaasClusterReconciler{
		Client:   mgr.GetClient(),
		Log:      ctrl.Log.WithName("controllers").WithName("MaasCluster"),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("maascluster-controller"),
		Tracker:  clusterCache,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MaasCluster")
		os.Exit(1)
	}

	// Both maintenance controllers always run; each filters to the objects it owns
	// (HMC to HCP-cluster hosts, VEC skips HCP clusters), so one management cluster
	// serves mixed HCP + WLC fleets.
	if err := (&controllers.VMEvacuationReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("VEC"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(ctx, mgr, concurrency(1)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VEC")
		os.Exit(1)
	}
	if err := (&controllers.HMCMaintenanceReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("HMC"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(ctx, mgr, concurrency(1)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HMC")
		os.Exit(1)
	}

	if err = (&infrav1beta1.MaasCluster{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "MaasCluster")
		os.Exit(1)
	}
	if err = (&infrav1beta1.MaasMachine{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "MaasMachine")
		os.Exit(1)
	}
	if err = (&infrav1beta1.MaasMachineTemplate{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "MaasMachineTemplate")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// One-time upgrade migration (v0.8.0->v0.9.0): rewrite any MaasCluster whose
	// status.failureDomains is still the pre-v0.9.0 map into a slice, otherwise the
	// typed cache List fails and the controller can never reconcile. Runs before
	// mgr.Start (and thus before the typed cache) using a direct unstructured client.
	directClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create client for failureDomains migration")
		os.Exit(1)
	}
	if err := controllers.MigrateMaasClusterFailureDomains(ctx, directClient, setupLog); err != nil {
		setupLog.Error(err, "failed to migrate MaasCluster status.failureDomains")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	// v1alpha4 change to "ctx"
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)

	}
}

func initFlags(fs *pflag.FlagSet) {
	fs.StringVar(&metricsBindAddr, "metrics-bind-addr", ":8080",
		"The address the metric endpoint binds to.")
	fs.IntVar(&machineConcurrency, "machine-concurrency", 2,
		"The number of maas machines to process simultaneously")
	fs.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	fs.DurationVar(&syncPeriod, "sync-period", 120*time.Minute,
		"The minimum interval at which watched resources are reconciled (e.g. 15m)")
	fs.StringVar(&healthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")
	fs.IntVar(&webhookPort, "webhook-port", 9443,
		"Webhook Server port")
	fs.StringVar(&watchNamespace, "namespace", "",
		"Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.",
	)
	fs.StringVar(&releaseErase, "release-erase", string(maasmachine.EraseNone),
		"Disk erase MAAS runs when a machine is released: none, quick, secure or full.")

	feature.MutableGates.AddFlag(fs)
}

func concurrency(c int) controller.Options {
	return controller.Options{MaxConcurrentReconciles: c}
}
