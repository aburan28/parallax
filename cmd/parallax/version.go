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

	"github.com/aburan28/parallax/internal/version"
)

// newVersionCmd prints the build stamp: semantic version, git ref, and plugin ABI.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the parallax version, git ref, and plugin ABI version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "parallax %s\n", version.Version); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "  vcsRef:     %s\n", version.VCSRef); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "  abiVersion: %s\n", version.ABIVersion); err != nil {
				return err
			}
			return nil
		},
	}
}
