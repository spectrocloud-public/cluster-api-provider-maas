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

	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/feature"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
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
	clusterRole          string
)

const (
	HCPClusterRoleValue = "hcp"
	WLCClusterRoleValue = "wlc"
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

	// Validate cluster role if provided
	if clusterRole != "" && clusterRole != HCPClusterRoleValue && clusterRole != WLCClusterRoleValue {
		setupLog.Error(nil, "invalid cluster-role value", "provided", clusterRole, "valid_values", []string{HCPClusterRoleValue, WLCClusterRoleValue})
		os.Exit(1)
	}

	if watchNamespace == "" {
		setupLog.Info("No namespace specified, watching all namespaces")
	} else {
		setupLog.Info("Watching cluster-api objects only in namespace for reconciliation", "namespace", watchNamespace)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsBindAddr,
		},
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "controller-leader-election-capmaas",
		Cache: cache.Options{
			SyncPeriod: &syncPeriod,
			DefaultNamespaces: map[string]cache.Config{
				watchNamespace: {},
			},
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

	// Probe guard (per v1beta2 migration cheatsheet §"Fork main.go"):
	//   webhookPort != 0 (webhook-only pod, capi-webhook-system) -> StartedChecker (waits for the
	//     webhook server to actually be listening before reporting ready).
	//   webhookPort == 0 (controller-only pod, tenant ns)         -> healthz.Ping. Without this
	//     the pod has no /healthz handler, kubelet 404s, and CrashLoops.
	if webhookPort != 0 {
		if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			setupLog.Error(err, "unable to create ready check")
			os.Exit(1)
		}
		if err := mgr.AddHealthzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			setupLog.Error(err, "unable to create health check")
			os.Exit(1)
		}
	} else {
		if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
			setupLog.Error(err, "unable to create ready check")
			os.Exit(1)
		}
		if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
			setupLog.Error(err, "unable to create health check")
			os.Exit(1)
		}
	}

	if webhookPort == 0 {
		// Set up a ClusterCache to provide cached clients/REST configs for workload clusters.
		// In v1.13 (v1beta2), ClusterCacheTracker + ClusterCacheReconciler were unified into
		// clustercache.SetupWithManager, which returns a ClusterCache interface directly.
		//
		// Match spectro-master's semantics: use mgr.GetClient() (cached) as the SecretClient and
		// register NodeProviderIDIndex as the old ClusterCacheTracker did. UserAgent is a v1beta2
		// hard requirement (cluster_cache.go validates it non-empty).
		clusterCache, err := clustercache.SetupWithManager(ctx, mgr, clustercache.Options{
			SecretClient: mgr.GetClient(),
			Cache: clustercache.CacheOptions{
				Indexes: []clustercache.CacheOptionsIndex{clustercache.NodeProviderIDIndex},
			},
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

		// Register VM Evacuation Controller for WLC clusters
		if clusterRole == WLCClusterRoleValue {
			if err := (&controllers.VMEvacuationReconciler{
				Client: mgr.GetClient(),
				Log:    ctrl.Log.WithName("controllers").WithName("VEC"),
				Scheme: mgr.GetScheme(),
			}).SetupWithManager(ctx, mgr, concurrency(1)); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "VEC")
				os.Exit(1)
			}
			setupLog.Info("VEC controller enabled via --cluster-role=wlc")
		}
		// PCP-5336: Wire HMC controller when cluster-role is set to hcp
		if clusterRole == HCPClusterRoleValue {
			if err := (&controllers.HMCMaintenanceReconciler{
				Client: mgr.GetClient(),
				Log:    ctrl.Log.WithName("controllers").WithName("HMC"),
				Scheme: mgr.GetScheme(),
			}).SetupWithManager(ctx, mgr, concurrency(1)); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "HMC")
				os.Exit(1)
			}
			setupLog.Info("HMC controller enabled via --cluster-role=hcp")
		}
	}

	if webhookPort != 0 {
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
	}

	// +kubebuilder:scaffold:builder

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
	fs.IntVar(&webhookPort, "webhook-port", 0,
		"Webhook Server port")
	fs.StringVar(&watchNamespace, "namespace", "",
		"Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.",
	)
	fs.StringVar(&clusterRole, "cluster-role", "",
		"Role of this cluster: 'hcp' (Host Control Plane) or 'wlc' (Workload Cluster). When set to 'wlc', enables VM evacuation controller.",
	)

	feature.MutableGates.AddFlag(fs)
}

func concurrency(c int) controller.Options {
	return controller.Options{MaxConcurrentReconciles: c}
}
