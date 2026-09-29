// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The report exporter runs independently of individual certification lifecycles.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/reportexport"
)

func main() {
	var namespace, healthAddress, metricsAddress string
	var leaderElect, allowHTTP bool
	flag.StringVar(&namespace, "namespace", "nvcre-reports",
		"Namespace containing policies, deliveries, and authentication Secrets.")
	flag.StringVar(&healthAddress, "health-probe-bind-address", ":8081", "Address for health and readiness probes.")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "Address for controller metrics; use 0 to disable.")
	flag.BoolVar(&leaderElect, "leader-elect", true,
		"Coordinate active exporters using a Lease in the reporting namespace.")
	flag.BoolVar(&allowHTTP, "allow-http", false, "Allow plain HTTP destinations for development.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if err := run(namespace, healthAddress, metricsAddress, leaderElect, allowHTTP); err != nil {
		ctrl.Log.Error(err, "report exporter stopped")
		os.Exit(1)
	}
}

func run(namespace, healthAddress, metricsAddress string, leaderElect, allowHTTP bool) error {
	if namespace == "" {
		return fmt.Errorf("--namespace must not be empty")
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nvcrev1alpha1.AddToScheme(scheme))
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces:           map[string]cache.Config{namespace: {}},
			ReaderFailOnMissingInformer: true,
		},
		HealthProbeBindAddress: healthAddress,
		Metrics:                metricsserver.Options{BindAddress: metricsAddress},
		LeaderElection:         leaderElect, LeaderElectionID: "report-exporter.nvcre.nvidia.com",
		LeaderElectionNamespace: namespace,
	})
	if err != nil {
		return err
	}
	if err := (&reportexport.Reconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Namespace: namespace, AllowHTTP: allowHTTP,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
