// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package kubeconfig provides a thin wrapper over k8s.io/client-go/tools/clientcmd
// for registering kubeconfig/context/namespace CLI flags, replacing the
// k8s.io/cli-runtime/pkg/genericclioptions.ConfigFlags dependency.
package kubeconfig

import (
	"github.com/spf13/pflag"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ConfigFlags holds pointers to kubeconfig, context, and namespace flag values.
// A nil pointer field is skipped when AddFlags is called, making that flag
// unavailable in the command — matching the genericclioptions.ConfigFlags idiom.
type ConfigFlags struct {
	KubeConfig *string
	Context    *string
	Namespace  *string
}

// NewConfigFlags returns a ConfigFlags with all fields initialised to empty
// (non-nil) string pointers. The usePersistentConfig parameter is accepted for
// drop-in compatibility with genericclioptions.NewConfigFlags but is unused.
func NewConfigFlags(_ bool) *ConfigFlags {
	return &ConfigFlags{
		KubeConfig: new(string),
		Context:    new(string),
		Namespace:  new(string),
	}
}

// AddFlags registers whichever flags have non-nil pointer fields onto fs.
func (f *ConfigFlags) AddFlags(fs *pflag.FlagSet) {
	if f.KubeConfig != nil {
		fs.StringVar(f.KubeConfig, "kubeconfig", *f.KubeConfig,
			"Path to the kubeconfig file to use for CLI requests.")
	}
	if f.Context != nil {
		fs.StringVar(f.Context, "context", *f.Context,
			"Name of the kubeconfig context to use.")
	}
	if f.Namespace != nil {
		fs.StringVarP(f.Namespace, "namespace", "n", *f.Namespace,
			"Namespace scope for this request.")
	}
}

// ToRESTConfig returns a *rest.Config built from the configured kubeconfig and context.
func (f *ConfigFlags) ToRESTConfig() (*rest.Config, error) {
	return f.clientConfig().ClientConfig()
}

// ToRawKubeConfigLoader returns a clientcmd.ClientConfig for low-level access
// (e.g., reading raw context/cluster information).
func (f *ConfigFlags) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return f.clientConfig()
}

func (f *ConfigFlags) clientConfig() clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if f.KubeConfig != nil && *f.KubeConfig != "" {
		rules.ExplicitPath = *f.KubeConfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if f.Context != nil && *f.Context != "" {
		overrides.CurrentContext = *f.Context
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
}
