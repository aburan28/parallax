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
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/analysis"
	"github.com/aburan28/parallax/internal/space"
	"github.com/aburan28/parallax/internal/store"
)

// newSelectCmd runs (or replays) selection for a run: it reads the run's trials and
// SLI values from the results DB, computes the decision (guardrails → Pareto → rank →
// top-K), prints it, and — unless --dry-run — persists a decisions row. The whole
// computation is pure and offline (DESIGN.md §12), so re-running reproduces it.
func newSelectCmd() *cobra.Command {
	var (
		dsn    string
		runID  int64
		format string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "select",
		Short: "Compute (or replay) candidate selection for a run from the results DB",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runID == 0 {
				return fmt.Errorf("--run-id is required")
			}
			ctx := cmd.Context()
			st, err := store.Open(ctx, dsn)
			if err != nil {
				return fmt.Errorf("open store %q: %w", dsn, err)
			}
			defer func() { _ = st.Close() }()

			rec, err := runSelect(ctx, st, runID)
			if err != nil {
				return err
			}

			if !dryRun {
				payload, _ := json.Marshal(rec)
				if _, err := st.InsertDecision(ctx, &store.DecisionRecord{
					RunID:  runID,
					Stage:  rec.Stage,
					Record: payload,
				}); err != nil {
					return fmt.Errorf("persist decision: %w", err)
				}
			}

			return printDecision(cmd, rec, format)
		},
	}
	cmd.Flags().StringVar(&dsn, "db-dsn", defaultDSN, "Results store DSN (sqlite://path or postgres://...).")
	cmd.Flags().Int64Var(&runID, "run-id", 0, "results DB run id to select over (required)")
	cmd.Flags().StringVarP(&format, "output", "o", "table", "output format: table|json")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "compute and print without writing a decisions row")
	return cmd
}

// runSelect loads a run's study spec + trials + SLI values and computes the decision.
func runSelect(ctx context.Context, st store.Store, runID int64) (*analysis.DecisionRecord, error) {
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("get run %d: %w", runID, err)
	}
	studyRec, err := st.GetStudy(ctx, run.StudyID)
	if err != nil {
		return nil, fmt.Errorf("get study %d: %w", run.StudyID, err)
	}
	var spec v1alpha1.StudySpec
	if err := json.Unmarshal(studyRec.Spec, &spec); err != nil {
		return nil, fmt.Errorf("decode study spec: %w", err)
	}

	trials, err := st.ListTrialsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list trials: %w", err)
	}
	slis, err := st.ListSLIValuesForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list sli values: %w", err)
	}

	// Join SLI rows to their trial, then to the trial's config hash.
	hashByTrial := make(map[int64]string, len(trials))
	modeByTrial := make(map[int64]string, len(trials))
	validByTrial := make(map[int64]string, len(trials))
	for _, t := range trials {
		hashByTrial[t.ID] = t.ConfigHash
		modeByTrial[t.ID] = t.Mode
		validByTrial[t.ID] = t.Validity
	}
	sliByTrial := map[int64]map[string]float64{}
	for _, v := range slis {
		m := sliByTrial[v.TrialID]
		if m == nil {
			m = map[string]float64{}
			sliByTrial[v.TrialID] = m
		}
		m[v.Name] = v.Value
	}

	data := make([]analysis.TrialData, 0, len(trials))
	for _, t := range trials {
		data = append(data, analysis.TrialData{
			ConfigHash: t.ConfigHash,
			Mode:       t.Mode,
			Validity:   t.Validity,
			SLIs:       sliByTrial[t.ID],
		})
	}

	rec := analysis.Select(analysis.SelectInput{
		Objectives: spec.Objectives,
		Guardrails: spec.Guardrails,
		Selection:  spec.Selection,
		Trials:     data,
		Baseline:   space.BaselineHash(spec),
	})
	return rec, nil
}

func printDecision(cmd *cobra.Command, rec *analysis.DecisionRecord, format string) error {
	out := cmd.OutOrStdout()
	if format == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rec)
	}

	fmt.Fprintf(out, "Selection (%s, top-%d)\n", rec.Method, rec.TopK)
	objNames := sortedObjNames(rec.Objectives)
	fmt.Fprintf(out, "Objectives: ")
	for i, n := range objNames {
		if i > 0 {
			fmt.Fprintf(out, ", ")
		}
		fmt.Fprintf(out, "%s (%s)", n, rec.Objectives[n])
	}
	fmt.Fprintln(out)

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RANK\tCONFIG\tSCORE\tFEASIBLE\tFRONT\tFLAGS")
	pts := append([]analysis.PointDecision(nil), rec.Points...)
	sort.SliceStable(pts, func(i, j int) bool {
		ri, rj := pts[i].Rank, pts[j].Rank
		if (ri == 0) != (rj == 0) {
			return ri != 0 // ranked points first
		}
		if ri != rj {
			return ri < rj
		}
		return pts[i].Score > pts[j].Score
	})
	for _, p := range pts {
		rank := "-"
		if p.Rank > 0 {
			rank = fmt.Sprintf("%d", p.Rank)
		}
		flags := ""
		if p.IsBaseline {
			flags += "baseline "
		}
		if p.IsCandidate {
			flags += "candidate"
		}
		fmt.Fprintf(tw, "%s\t%s\t%.4g\t%v\t%v\t%s\n",
			rank, short(p.Hash), p.Score, p.Feasible, p.OnFront, flags)
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\nCandidates advancing to validation: %d\n", len(rec.Candidates))
	for _, h := range rec.Candidates {
		fmt.Fprintf(out, "  - %s\n", short(h))
	}
	return nil
}

func sortedObjNames(dirs map[string]string) []string {
	out := make([]string, 0, len(dirs))
	for k := range dirs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
