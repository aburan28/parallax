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

import "github.com/spf13/cobra"

// newEnvCmd groups environment/execution-model helpers (DESIGN.md §16).
func newEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Environment helpers (doctor, ...)",
	}
	cmd.AddCommand(newEnvDoctorCmd())
	return cmd
}

// newEnvDoctorCmd checks that the local environment can run parallax (kubeconfig
// reachable, CRDs installed, plugin dir present, store reachable).
// TODO(m1): implement the real preflight checks and print a pass/fail table.
func newEnvDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose whether this environment can run parallax",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("m1"),
	}
}
