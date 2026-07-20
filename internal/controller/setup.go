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
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupAll constructs and registers all four reconcilers on the manager. It is the
// single entrypoint agent D (operator main and the CLI --local path) calls after
// building the scheme, opening the store, and creating the plugin host:
//
//	controller.SetupAll(mgr, controller.Deps{Store: st, Host: host})
//
// Each reconciler is filled with the manager's client, scheme, and a named event
// recorder, plus the shared Deps.
func SetupAll(mgr ctrl.Manager, deps Deps) error {
	if err := (&StudyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("study-controller"),
		Deps:     deps,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup study controller: %w", err)
	}

	if err := (&TrialReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("trial-controller"),
		Deps:     deps,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup trial controller: %w", err)
	}

	if err := (&PluginReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("plugin-controller"),
		Deps:     deps,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup plugin controller: %w", err)
	}

	if err := (&DatasetReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("dataset-controller"),
		Deps:     deps,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup dataset controller: %w", err)
	}

	return nil
}
