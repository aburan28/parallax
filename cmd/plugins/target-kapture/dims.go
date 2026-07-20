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

// knownDimensions is the set of capture-agent dimension paths target-kapture can
// map onto flags / pod spec (DESIGN.md §8, §11.3, gap K4). Contract() rejects any
// dimension outside this set at admission.
//
//	agent.batchSize          -> capture-agent --batch-size
//	agent.flushInterval      -> --flush-interval
//	agent.writeQueueSize     -> --write-queue-size (bounded async writer)
//	agent.maxBodyBytes       -> --max-body-bytes
//	agent.resources.cpuLimit -> direct mode owns the pod spec
//	agent.replicas           -> capture-agent Deployment replicas
var knownDimensions = map[string]struct{}{
	"agent.batchSize":          {},
	"agent.flushInterval":      {},
	"agent.writeQueueSize":     {},
	"agent.maxBodyBytes":       {},
	"agent.resources.cpuLimit": {},
	"agent.replicas":           {},
}

// unknownDimensions returns the subset of names that target-kapture does not know
// how to map, preserving the caller's order. A nil result means all are known.
func unknownDimensions(names []string) []string {
	var unknown []string
	for _, n := range names {
		if _, ok := knownDimensions[n]; !ok {
			unknown = append(unknown, n)
		}
	}
	return unknown
}

// supportedModes reports the target modes kapture supports (DESIGN.md §8.1):
// direct (plugin owns the pod spec) and integrated (hub/spoke reconciliation).
func supportedModes() []string {
	return []string{"direct", "integrated"}
}
