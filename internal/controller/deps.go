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

package controller

import (
	"github.com/aburan28/parallax/internal/pluginhost"
	"github.com/aburan28/parallax/internal/store"
)

// Deps carries the shared collaborators every reconciler needs: the results-DB store
// (system of record, DESIGN.md §15) and the plugin host (subprocess supervisor, §5.2).
// It is embedded into every reconciler so agent D can construct them uniformly and the
// controllers can reach r.Store / r.Host directly.
type Deps struct {
	Store store.Store
	Host  pluginhost.Host
}
