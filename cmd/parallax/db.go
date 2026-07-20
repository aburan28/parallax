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

	"github.com/aburan28/parallax/internal/store"
)

// newDBCmd groups results-store maintenance commands.
func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Results store maintenance (migrate, ...)",
	}
	cmd.AddCommand(newDBMigrateCmd())
	return cmd
}

// newDBMigrateCmd opens the store and applies pending schema migrations. Real.
func newDBMigrateCmd() *cobra.Command {
	var dsn string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending schema migrations to the results store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			st, err := store.Open(ctx, dsn)
			if err != nil {
				return fmt.Errorf("open store %q: %w", dsn, err)
			}
			defer func() { _ = st.Close() }()
			if err := st.Migrate(ctx); err != nil {
				return fmt.Errorf("migrate store: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "migrations applied to %s\n", dsn)
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "db-dsn", defaultDSN,
		"Results store DSN (sqlite://path or postgres://...).")
	return cmd
}
