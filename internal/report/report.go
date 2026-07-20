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

// Package report renders study result bundles (funnel summary, per-candidate
// stats) as Markdown or HTML (DESIGN.md §13.4). M0 ships a real Markdown table
// renderer; HTML is a stub.
package report

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

// ReportData is the minimal input to a renderer: study identity plus the ranked
// candidate rows.
type ReportData struct {
	StudyName   string
	Namespace   string
	RunID       string
	GeneratedAt time.Time
	// Summary is a one-line funnel outcome (e.g. "80 trials, 3 candidates, 1 promoted").
	Summary string
	// Candidates are the rows of the results table, already ranked.
	Candidates []CandidateRow
	// Notes are free-form caveats appended below the table (e.g. power warnings).
	Notes []string
}

// CandidateRow is one configuration in the report table.
type CandidateRow struct {
	Hash     string
	Rank     int
	Score    float64
	Feasible bool
	Pareto   bool
	// Objectives maps objective SLI name -> value; columns are unioned and
	// sorted across all rows so output is deterministic.
	Objectives map[string]float64
	// PValue and CliffsDelta are the validation stats vs. baseline (0 if unset).
	PValue      float64
	CliffsDelta float64
}

// Renderer renders ReportData to a writer in a chosen format.
type Renderer struct{}

// Markdown renders a simple, deterministic Markdown report: a header block and
// a candidate table with a stable column order.
func (Renderer) Markdown(w io.Writer, data ReportData) error {
	bw := &errWriter{w: w}

	bw.printf("# Study report: %s\n\n", data.StudyName)
	if data.Namespace != "" {
		bw.printf("- **Namespace:** %s\n", data.Namespace)
	}
	if data.RunID != "" {
		bw.printf("- **Run:** %s\n", data.RunID)
	}
	if !data.GeneratedAt.IsZero() {
		bw.printf("- **Generated:** %s\n", data.GeneratedAt.UTC().Format(time.RFC3339))
	}
	if data.Summary != "" {
		bw.printf("- **Summary:** %s\n", data.Summary)
	}
	bw.printf("\n")

	objCols := objectiveColumns(data.Candidates)

	// Header row.
	bw.printf("| Rank | Config | Score | Feasible | Pareto | p | delta |")
	for _, c := range objCols {
		bw.printf(" %s |", c)
	}
	bw.printf("\n")

	// Separator row.
	bw.printf("|------|--------|-------|----------|--------|---|-------|")
	for range objCols {
		bw.printf("---|")
	}
	bw.printf("\n")

	// Data rows.
	for _, row := range data.Candidates {
		bw.printf("| %d | %s | %s | %s | %s | %s | %s |",
			row.Rank,
			shortHash(row.Hash),
			formatFloat(row.Score),
			yesNo(row.Feasible),
			yesNo(row.Pareto),
			formatFloat(row.PValue),
			formatFloat(row.CliffsDelta),
		)
		for _, c := range objCols {
			if v, ok := row.Objectives[c]; ok {
				bw.printf(" %s |", formatFloat(v))
			} else {
				bw.printf(" - |")
			}
		}
		bw.printf("\n")
	}

	if len(data.Notes) > 0 {
		bw.printf("\n## Notes\n\n")
		for _, n := range data.Notes {
			bw.printf("- %s\n", n)
		}
	}

	if bw.err != nil {
		return fmt.Errorf("report: render markdown: %w", bw.err)
	}
	return nil
}

// HTML renders an HTML report.
//
// TODO(m1): implement an HTML bundle (Pareto plots, per-candidate stats tables,
// scenario timelines). Returns an error in M0 rather than emitting a misleading
// partial page.
func (Renderer) HTML(w io.Writer, data ReportData) error {
	_ = w
	_ = data
	return fmt.Errorf("report: HTML rendering not implemented yet")
}

// objectiveColumns returns the sorted union of objective names across rows.
func objectiveColumns(rows []CandidateRow) []string {
	set := map[string]struct{}{}
	for _, r := range rows {
		for name := range r.Objectives {
			set[name] = struct{}{}
		}
	}
	cols := make([]string, 0, len(set))
	for name := range set {
		cols = append(cols, name)
	}
	sort.Strings(cols)
	return cols
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	if h == "" {
		return "-"
	}
	return h
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// errWriter defers error handling so the render body stays linear.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}
