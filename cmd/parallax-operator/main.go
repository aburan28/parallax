/*
Copyright 2026 The Parallax Authors.

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

// Command parallax-operator is the controller-runtime manager that runs the four
// parallax reconcilers (Study/Trial/Plugin/Dataset) against a Kubernetes API server.
// It opens the results store, brings up the plugin host, wires every reconciler via
// controller.SetupAll, and serves metrics + health probes (DESIGN.md §4.1, §16).
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/controller"
	"github.com/aburan28/parallax/internal/pluginhost"
	"github.com/aburan28/parallax/internal/store"
	"github.com/aburan28/parallax/internal/version"

	// Blank-import the store drivers so their init() self-registers with the store
	// registry; store.Open then dispatches on the DSN scheme.
	_ "github.com/aburan28/parallax/internal/store/postgres"
	_ "github.com/aburan28/parallax/internal/store/sqlite"
)

// scheme carries the core client-go types plus the parallax.dev v1alpha1 API group.
var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		pluginDir            string
		dbDSN                string
		skipPluginVerify     bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to. Use \"0\" to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Ensures only one active manager.")
	flag.StringVar(&pluginDir, "plugin-dir", "/plugins",
		"Directory the plugin host discovers and launches plugin binaries from.")
	flag.StringVar(&dbDSN, "db-dsn", "sqlite:///var/lib/parallax/parallax.db",
		"Results store DSN. Scheme selects the driver (sqlite://path or postgres://...).")
	flag.BoolVar(&skipPluginVerify, "skip-plugin-verify", false,
		"Skip cosign verification of plugin images (dev/--local only).")

	// zap logger flags (e.g. --zap-log-level) integrate with the standard flag set.
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting parallax-operator",
		"version", version.Version, "vcsRef", version.VCSRef, "abiVersion", version.ABIVersion)

	// A single signal-scoped context governs store I/O, the plugin host, and Start.
	ctx := ctrl.SetupSignalHandler()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "parallax-operator.parallax.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Results store: open, then apply pending migrations before any reconcile writes.
	st, err := store.Open(ctx, dbDSN)
	if err != nil {
		setupLog.Error(err, "unable to open results store", "dsn", dbDSN)
		os.Exit(1)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			setupLog.Error(cerr, "error closing results store")
		}
	}()
	if err := st.Migrate(ctx); err != nil {
		setupLog.Error(err, "unable to migrate results store")
		os.Exit(1)
	}

	// Plugin host: subprocess supervisor for target/loaddriver/provider/... plugins.
	host := pluginhost.New(pluginhost.Options{
		PluginDir:  pluginDir,
		SkipVerify: skipPluginVerify,
	})
	defer func() {
		if cerr := host.Close(ctx); cerr != nil {
			setupLog.Error(cerr, "error closing plugin host")
		}
	}()

	// Wire all four reconcilers onto the manager. Agent D calls ONLY SetupAll.
	if err := controller.SetupAll(mgr, controller.Deps{Store: st, Host: host}); err != nil {
		setupLog.Error(err, "unable to set up controllers")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
