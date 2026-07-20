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

// newPromoteCmd approves a validated candidate, moving a Study from AwaitingApproval
// to Promoted and recording the decision (DESIGN.md §13.4).
// TODO(m2): patch Study approval status and write a promotion row to the results DB.
func newPromoteCmd() *cobra.Command {
	var (
		studyName string
		candidate string
	)
	cmd := &cobra.Command{
		Use:   "promote [study]",
		Short: "Approve and promote a validated candidate configuration",
		Args:  cobra.MaximumNArgs(1),
		RunE:  notImplemented("m2"),
	}
	cmd.Flags().StringVar(&studyName, "study", "", "study whose candidate is being promoted")
	cmd.Flags().StringVar(&candidate, "candidate", "", "candidate config hash to promote")
	return cmd
}
