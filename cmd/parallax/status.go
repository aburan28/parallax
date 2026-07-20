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

// newStatusCmd summarizes a study's funnel progress and its trials.
// TODO(m1): read the Study CR + results DB and render phase, budgets, and trial roll-ups.
func newStatusCmd() *cobra.Command {
	var studyName string
	cmd := &cobra.Command{
		Use:   "status [study]",
		Short: "Show funnel status for a study and its trials",
		Args:  cobra.MaximumNArgs(1),
		RunE:  notImplemented("m1"),
	}
	cmd.Flags().StringVar(&studyName, "study", "", "study name to inspect")
	return cmd
}
