// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/spf13/cobra"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
	// +kubebuilder:scaffold:imports
)

// version is set at build time via -ldflags.
var version = "dev"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	defaultMaxConcurrentReconciles            = 10
	defaultMeasurementMaxConcurrentReconciles = 5
)

type controllerConcurrencyOptions struct {
	maxConcurrentReconciles            int
	measurementMaxConcurrentReconciles int
}

func (o controllerConcurrencyOptions) validate() error {
	if o.maxConcurrentReconciles <= 0 {
		return fmt.Errorf("--max-concurrent-reconciles must be greater than 0, got %d", o.maxConcurrentReconciles)
	}
	if o.measurementMaxConcurrentReconciles <= 0 {
		return fmt.Errorf(
			"--measurement-max-concurrent-reconciles must be greater than 0, got %d",
			o.measurementMaxConcurrentReconciles,
		)
	}
	return nil
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(nvcrev1alpha1.AddToScheme(scheme))
	utilruntime.Must(trainerv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

// nolint:gocyclo
func newRootCommand() *cobra.Command {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	concurrency := controllerConcurrencyOptions{
		maxConcurrentReconciles:            defaultMaxConcurrentReconciles,
		measurementMaxConcurrentReconciles: defaultMeasurementMaxConcurrentReconciles,
	}

	// Bridge zap flags (registered on standard flag.CommandLine) into cobra
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)

	cmd := &cobra.Command{
		Use:     "nvcre",
		Short:   "NVCRE controller manager",
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := concurrency.validate(); err != nil {
				return err
			}

			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

			// Fail fast if the embedded gpu-defaults catalog is missing or
			// malformed. Without this, reconcilers would silently fall back
			// to baseline values for every architecture.
			if err := catalog.LoadGPUDefaults(); err != nil {
				setupLog.Error(err, "unable to load GPU defaults catalog")
				return err
			}

			// if the enable-http2 flag is false (the default), http/2 should be disabled
			// due to its vulnerabilities. More specifically, disabling http/2 will
			// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
			// Rapid Reset CVEs. For more information see:
			// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
			// - https://github.com/advisories/GHSA-4374-p667-p6c8
			var tlsOpts []func(*tls.Config)
			disableHTTP2 := func(c *tls.Config) {
				setupLog.Info("disabling http/2")
				c.NextProtos = []string{"http/1.1"}
			}

			if !enableHTTP2 {
				tlsOpts = append(tlsOpts, disableHTTP2)
			}

			// Initial webhook TLS options
			webhookTLSOpts := tlsOpts
			webhookServerOptions := webhook.Options{
				TLSOpts: webhookTLSOpts,
			}

			if len(webhookCertPath) > 0 {
				setupLog.Info("Initializing webhook certificate watcher using provided certificates",
					"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

				webhookServerOptions.CertDir = webhookCertPath
				webhookServerOptions.CertName = webhookCertName
				webhookServerOptions.KeyName = webhookCertKey
			}

			webhookServer := webhook.NewServer(webhookServerOptions)

			metricsServerOptions := metricsserver.Options{
				BindAddress:   metricsAddr,
				SecureServing: secureMetrics,
				TLSOpts:       tlsOpts,
			}

			if secureMetrics {
				metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
			}

			if len(metricsCertPath) > 0 {
				setupLog.Info("Initializing metrics certificate watcher using provided certificates",
					"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

				metricsServerOptions.CertDir = metricsCertPath
				metricsServerOptions.CertName = metricsCertName
				metricsServerOptions.KeyName = metricsCertKey
			}

			mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
				Scheme:                 scheme,
				Metrics:                metricsServerOptions,
				WebhookServer:          webhookServer,
				HealthProbeBindAddress: probeAddr,
				Cache:                  controller.CacheOptions(),
				LeaderElection:         enableLeaderElection,
				LeaderElectionID:       "240fe9c6.nvidia.com",
				// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
				// when the Manager ends. This requires the binary to immediately end when the
				// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
				// speeds up voluntary leader transitions as the new leader don't have to wait
				// LeaseDuration time first.
				//
				// In the default scaffold provided, the program ends immediately after
				// the manager stops, so would be fine to enable this option. However,
				// if you are doing or is intended to do any operation such as perform cleanups
				// after the manager stops then its usage might be unsafe.
				// LeaderElectionReleaseOnCancel: true,
			})
			if err != nil {
				return fmt.Errorf("unable to start manager: %w", err)
			}

			// Register every field index the controllers depend on. Reads that
			// filter on these fields use client.MatchingFields, which fails if the
			// index is absent — so this must run before any controller starts.
			if err := controller.RegisterFieldIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return fmt.Errorf("unable to register field indexes: %w", err)
			}

			clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
			if err != nil {
				return fmt.Errorf("unable to create kubernetes clientset: %w", err)
			}

			if err := (&controller.JobReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Clientset:               clientset,
				Recorder:                mgr.GetEventRecorder("job-controller"),
				MaxConcurrentReconciles: concurrency.maxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("unable to create controller Job: %w", err)
			}
			if err := (&controller.WorkflowReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Clientset:               clientset,
				Recorder:                mgr.GetEventRecorder("workflow-controller"),
				MaxConcurrentReconciles: concurrency.maxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("unable to create controller Workflow: %w", err)
			}
			if err := (&controller.CertificationReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				MaxConcurrentReconciles: concurrency.maxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("unable to create controller Certification: %w", err)
			}
			if err := (&controller.GoodputMeasurementReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Clientset:               clientset,
				LogFetcher:              podlogs.NewKubernetesLogFetcher(clientset),
				MaxConcurrentReconciles: concurrency.measurementMaxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("unable to create controller GoodputMeasurement: %w", err)
			}
			if err := (&controller.BandwidthMeasurementReconciler{
				Client:                  mgr.GetClient(),
				APIReader:               mgr.GetAPIReader(),
				Scheme:                  mgr.GetScheme(),
				Clientset:               clientset,
				LogFetcher:              podlogs.NewKubernetesLogFetcher(clientset),
				MaxConcurrentReconciles: concurrency.measurementMaxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("unable to create controller BandwidthMeasurement: %w", err)
			}
			if err := (&controller.WorkloadRunReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				MaxConcurrentReconciles: concurrency.maxConcurrentReconciles,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("unable to create controller WorkloadRun: %w", err)
			}
			// +kubebuilder:scaffold:builder

			if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
				return fmt.Errorf("unable to set up health check: %w", err)
			}
			if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
				return fmt.Errorf("unable to set up ready check: %w", err)
			}

			setupLog.Info("starting manager")
			return mgr.Start(ctrl.SetupSignalHandler())
		},
	}

	// Register flags on the command
	cmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	cmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	cmd.Flags().BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	cmd.Flags().IntVar(&concurrency.maxConcurrentReconciles, "max-concurrent-reconciles",
		defaultMaxConcurrentReconciles,
		"Maximum number of concurrent reconciles for Job, Workflow, Certification, and WorkloadRun controllers.")
	cmd.Flags().IntVar(&concurrency.measurementMaxConcurrentReconciles, "measurement-max-concurrent-reconciles",
		defaultMeasurementMaxConcurrentReconciles,
		"Maximum number of concurrent reconciles for GoodputMeasurement and BandwidthMeasurement controllers.")
	cmd.Flags().BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	cmd.Flags().StringVar(&webhookCertPath, "webhook-cert-path", "",
		"The directory that contains the webhook certificate.")
	cmd.Flags().StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	cmd.Flags().StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	cmd.Flags().StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	cmd.Flags().StringVar(&metricsCertName, "metrics-cert-name", "tls.crt",
		"The name of the metrics server certificate file.")
	cmd.Flags().StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	cmd.Flags().BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	cmd.Flags().AddGoFlagSet(flag.CommandLine)

	return cmd
}
