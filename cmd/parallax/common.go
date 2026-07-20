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

package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"

	// Blank-import the store drivers so their init() self-registers with the store
	// registry; store.Open then dispatches on the DSN scheme. The CLI needs both so
	// `parallax db migrate` / `--local` work regardless of the configured DSN.
	_ "github.com/aburan28/parallax/internal/store/postgres"
	_ "github.com/aburan28/parallax/internal/store/sqlite"
)

// defaultDSN mirrors the operator's default results store: SQLite under /var/lib.
const defaultDSN = "sqlite:///var/lib/parallax/parallax.db"

// fieldManager is the server-side-apply field owner for CLI-driven writes.
const fieldManager = "parallax-cli"

// buildScheme returns a scheme with core client-go types plus the parallax.dev
// v1alpha1 API group registered.
func buildScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("register client-go scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("register parallax scheme: %w", err)
	}
	return s, nil
}

// newAPIClient builds a controller-runtime client against the ambient kubeconfig
// (KUBECONFIG / --kubeconfig / in-cluster), with the parallax scheme registered.
func newAPIClient() (client.Client, error) {
	s, err := buildScheme()
	if err != nil {
		return nil, err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, fmt.Errorf("build API client: %w", err)
	}
	return c, nil
}

// notImplemented returns a RunE that reports the command is not yet implemented and
// names the milestone that will deliver it. The command wiring is real; only the
// behavior is deferred.
func notImplemented(milestone string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		return fmt.Errorf("%q is not yet implemented (%s)", cmd.CommandPath(), milestone)
	}
}
