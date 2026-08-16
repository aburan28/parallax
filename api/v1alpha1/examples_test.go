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

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
)

// Every shipped example must decode strictly against the typed API. Strict decoding
// is the point: the API server silently *prunes* unknown fields, so an example using
// a field the CRD does not have (as checkout-latency-bakeoff.yaml did with
// dimensions[].path) looks fine in review and quietly does nothing at runtime.
func TestExamplesDecodeStrictly(t *testing.T) {
	for _, tc := range []struct {
		dir string
		new func() any
	}{
		{"../../examples/studies", func() any { return &v1alpha1.Study{} }},
		{"../../examples/datasets", func() any { return &v1alpha1.Dataset{} }},
		{"../../examples/plugins", func() any { return &v1alpha1.Plugin{} }},
	} {
		matches, err := filepath.Glob(filepath.Join(tc.dir, "*.yaml"))
		if err != nil {
			t.Fatalf("glob %s: %v", tc.dir, err)
		}
		if len(matches) == 0 {
			t.Fatalf("no examples found in %s", tc.dir)
		}
		for _, path := range matches {
			t.Run(filepath.Base(path), func(t *testing.T) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				// Catalog files carry several objects separated by ---.
				for i, doc := range strings.Split(string(raw), "\n---") {
					if strings.TrimSpace(stripComments(doc)) == "" {
						continue
					}
					if err := yaml.UnmarshalStrict([]byte(doc), tc.new()); err != nil {
						t.Errorf("document %d does not decode against the typed API: %v", i, err)
					}
				}
			})
		}
	}
}

// stripComments removes whole-line comments so a comment-only document (the header
// block before the first ---) is recognised as empty.
func stripComments(doc string) string {
	var kept []string
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
