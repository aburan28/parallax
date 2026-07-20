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
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/aburan28/parallax/internal/controller"
	"github.com/aburan28/parallax/internal/pluginhost"
	"github.com/aburan28/parallax/internal/store"
)

// newApplyCmd applies a parallax manifest (Study/Trial/Plugin/Dataset). Without
// --local it server-side-applies to the API server; with --local it stands up an
// embedded manager + SQLite store + plugin host in-process (DESIGN.md §16).
func newApplyCmd() *cobra.Command {
	var (
		file  string
		local bool
	)
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a parallax manifest (Study/Trial/Plugin/Dataset)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errors.New("apply: -f/--file is required")
			}
			if local {
				return runApplyLocal(cmd.Context(), cmd.OutOrStdout(), file)
			}
			return runApplyRemote(cmd.Context(), cmd.OutOrStdout(), file)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to a YAML manifest to apply")
	cmd.Flags().BoolVar(&local, "local", false,
		"run against the current kubeconfig with an embedded manager + SQLite store")
	return cmd
}

// runApplyRemote server-side-applies every document in the manifest to the API server.
func runApplyRemote(ctx context.Context, out io.Writer, file string) error {
	c, err := newAPIClient()
	if err != nil {
		return err
	}
	objs, err := decodeManifest(file)
	if err != nil {
		return err
	}
	for i := range objs {
		if err := applyOne(ctx, c, &objs[i], out); err != nil {
			return err
		}
	}
	return nil
}

// runApplyLocal brings up the same wiring as the operator (scheme, manager, store,
// plugin host, controller.SetupAll) against the current kubeconfig with a SQLite
// store, then applies the manifest through a direct client.
func runApplyLocal(ctx context.Context, out io.Writer, file string) error {
	scheme, err := buildScheme()
	if err != nil {
		return err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	// Embedded manager: no leader election, and metrics/health disabled ("0") so the
	// CLI never binds ports on a developer laptop.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		return fmt.Errorf("build embedded manager: %w", err)
	}

	st, err := store.Open(ctx, defaultDSN)
	if err != nil {
		return fmt.Errorf("open local store %q: %w", defaultDSN, err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate local store: %w", err)
	}

	// --local trusts local plugin binaries: skip cosign verification.
	host := pluginhost.New(pluginhost.Options{PluginDir: "/plugins", SkipVerify: true})
	defer func() { _ = host.Close(ctx) }()

	if err := controller.SetupAll(mgr, controller.Deps{Store: st, Host: host}); err != nil {
		return fmt.Errorf("wire embedded controllers: %w", err)
	}

	// Apply the manifest through a direct client so the objects exist before the
	// embedded funnel would pick them up.
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build embedded client: %w", err)
	}
	objs, err := decodeManifest(file)
	if err != nil {
		return err
	}
	for i := range objs {
		if err := applyOne(ctx, c, &objs[i], out); err != nil {
			return err
		}
	}

	// TODO(m1): run the embedded funnel to completion — start the manager
	// (mgr.Start(ctx)) so the wired reconcilers drive the applied Study through
	// Sweeping→…→Promoted, stream phase/status back to the terminal, and block until
	// the study reaches a terminal phase (or ctx is cancelled). For the M0 skeleton we
	// only prove the wiring compiles and the manifest is accepted.
	fmt.Fprintln(out, "note: --local wiring is in place; embedded funnel run is deferred to M1")
	return nil
}

// decodeManifest reads a (possibly multi-document) YAML/JSON manifest into a slice of
// unstructured objects, skipping empty documents.
func decodeManifest(file string) ([]unstructured.Unstructured, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("open manifest %q: %w", file, err)
	}
	defer func() { _ = f.Close() }()

	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	var out []unstructured.Unstructured
	for {
		var obj unstructured.Unstructured
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode manifest %q: %w", file, err)
		}
		if obj.Object == nil || obj.GetKind() == "" {
			continue // empty document between "---" separators
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("manifest %q contained no objects", file)
	}
	return out, nil
}

// applyOne server-side-applies a single object and reports the result.
func applyOne(ctx context.Context, c client.Client, obj *unstructured.Unstructured, out io.Writer) error {
	if err := c.Patch(ctx, obj, client.Apply,
		client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s: %w (is the parallax.dev CRD installed?)",
			obj.GetKind(), obj.GetName(), err)
	}
	ns := obj.GetNamespace()
	if ns == "" {
		fmt.Fprintf(out, "applied %s %s\n", obj.GetKind(), obj.GetName())
	} else {
		fmt.Fprintf(out, "applied %s %s/%s\n", obj.GetKind(), ns, obj.GetName())
	}
	return nil
}
