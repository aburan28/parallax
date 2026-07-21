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

// Command parallax is the operator's companion CLI (DESIGN.md §4.1). It applies
// manifests, inspects study/trial status, renders reports, promotes candidates,
// manages datasets and plugins, runs environment doctor checks, and migrates the
// results store. `--local` embeds the controllers + plugin host in-process against
// the current kubeconfig with a SQLite store (DESIGN.md §16).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "parallax",
		Short:         "Parallax — Kubernetes benchmarking & experimentation operator CLI",
		Long:          "parallax drives Study/Trial/Plugin/Dataset resources and the results store.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newVersionCmd(),
		newApplyCmd(),
		newStatusCmd(),
		newSelectCmd(),
		newReportCmd(),
		newPromoteCmd(),
		newDatasetsCmd(),
		newEnvCmd(),
		newPluginCmd(),
		newDBCmd(),
	)
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "parallax: "+err.Error())
		os.Exit(1)
	}
}
