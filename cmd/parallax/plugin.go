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

// newPluginCmd manages Plugin resources and the plugin host (DESIGN.md §5).
func newPluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Inspect plugins (list, conformance)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List Plugin resources with kind, phase, digest, and ABI",
			Args:  cobra.NoArgs,
			// TODO(m1): list Plugin CRs via the API client and print kind/phase/digest/ABI.
			RunE: notImplemented("m1"),
		},
		&cobra.Command{
			Use:   "conformance [plugin]",
			Short: "Run the ABI conformance suite against a plugin (DESIGN.md §5.6)",
			Args:  cobra.MaximumNArgs(1),
			// TODO(m2): launch the plugin via the host and exercise the conformance RPCs.
			RunE: notImplemented("m2"),
		},
	)
	return cmd
}
