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

// newReportCmd renders a study/run report (rankings, SLI deltas, provenance).
// TODO(m2): replay from the results DB via internal/report and emit md/html/json.
func newReportCmd() *cobra.Command {
	var (
		runID  int64
		format string
	)
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Render a report for a study run (rankings, SLI deltas, provenance)",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("m2"),
	}
	cmd.Flags().Int64Var(&runID, "run-id", 0, "results DB run id to report on")
	cmd.Flags().StringVarP(&format, "output", "o", "md", "report format: md|html|json")
	return cmd
}
