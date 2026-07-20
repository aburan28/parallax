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

// newDatasetsCmd manages Dataset resources (list/verify) used by load drivers
// (DESIGN.md §10).
func newDatasetsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "datasets",
		Short: "Manage benchmark datasets (list, verify)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List Dataset resources and their phases",
			Args:  cobra.NoArgs,
			// TODO(m1): list Dataset CRs via the API client and print phase + digest.
			RunE: notImplemented("m1"),
		},
		&cobra.Command{
			Use:   "verify [dataset]",
			Short: "Verify a Dataset against object storage (manifest ↔ slices ↔ objects)",
			Args:  cobra.MaximumNArgs(1),
			// TODO(m2): drive the Dataset controller's verification path and report digests.
			RunE: notImplemented("m2"),
		},
	)
	return cmd
}
