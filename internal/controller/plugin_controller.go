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
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/store"
)

// pluginResync is the periodic reconcile cadence that lets the plugin host surface
// health/hot-reload state onto the CR (DESIGN.md §5.4).
const pluginResync = 30 * time.Second

// PluginReconciler reconciles cluster-scoped Plugin CRs: it drives the plugin host to
// install/verify/load the subprocess and mirrors the host's state onto the CR status
// (DESIGN.md §5.3). It manages a finalizer so the subprocess is drained before removal.
type PluginReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Deps
}

// +kubebuilder:rbac:groups=parallax.dev,resources=plugins,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=parallax.dev,resources=plugins/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=parallax.dev,resources=plugins/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile ensures the plugin subprocess is loaded and mirrors its resolved digest,
// ABI version, and phase onto the CR. On deletion it drains and unloads the subprocess
// before releasing the finalizer.
func (r *PluginReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var plugin v1alpha1.Plugin
	if err := r.Get(ctx, req.NamespacedName, &plugin); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path: drain-and-unload, then drop the finalizer.
	if !plugin.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&plugin, pluginFinalizer) {
			if err := r.Host.Unload(ctx, plugin.Name); err != nil {
				r.Recorder.Eventf(&plugin, corev1.EventTypeWarning, "UnloadFailed", "unload failed: %v", err)
				return ctrl.Result{}, fmt.Errorf("unload plugin %q: %w", plugin.Name, err)
			}
			controllerutil.RemoveFinalizer(&plugin, pluginFinalizer)
			if err := r.Update(ctx, &plugin); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
			r.Recorder.Event(&plugin, corev1.EventTypeNormal, "Unloaded", "plugin subprocess drained and stopped")
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer is present before we load anything.
	if !controllerutil.ContainsFinalizer(&plugin, pluginFinalizer) {
		controllerutil.AddFinalizer(&plugin, pluginFinalizer)
		if err := r.Update(ctx, &plugin); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if plugin.Status.Phase == "" {
		plugin.Status.Phase = v1alpha1.PluginPhasePending
	}
	plugin.Status.ObservedGeneration = plugin.Generation

	// Drive the host toward Ready. Ensure is idempotent and updates status fields
	// (digest, ABI, phase) on the passed-in copy.
	if err := r.Host.Ensure(ctx, &plugin); err != nil {
		plugin.Status.Phase = v1alpha1.PluginPhaseFailed
		apimeta.SetStatusCondition(&plugin.Status.Conditions, metav1.Condition{
			Type:               condLoaded,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: plugin.Generation,
			Reason:             "EnsureFailed",
			Message:            err.Error(),
		})
		r.Recorder.Eventf(&plugin, corev1.EventTypeWarning, "EnsureFailed", "ensure failed: %v", err)
		if uerr := r.Status().Update(ctx, &plugin); uerr != nil {
			log.Error(uerr, "update plugin status after ensure failure")
		}
		return ctrl.Result{}, fmt.Errorf("ensure plugin %q: %w", plugin.Name, err)
	}

	// Mirror host state onto status.
	if dig, ok := r.Host.Digest(plugin.Name); ok && plugin.Status.ResolvedDigest == "" {
		plugin.Status.ResolvedDigest = dig
	}
	if plugin.Status.ABIVersion == "" {
		// TODO(m1): carry the negotiated ABI from the plugin Describe handshake.
		plugin.Status.ABIVersion = "v1"
	}

	ready := r.Host.Ready(plugin.Name)
	loadedStatus := metav1.ConditionFalse
	if ready {
		plugin.Status.Phase = v1alpha1.PluginPhaseReady
		loadedStatus = metav1.ConditionTrue
	} else {
		plugin.Status.Phase = v1alpha1.PluginPhaseInstalling
	}

	// TODO(m1): real cosign verification drives the Verified condition (§5.3); the host
	// gates install on Options.SkipVerify for now.
	apimeta.SetStatusCondition(&plugin.Status.Conditions, metav1.Condition{
		Type:               condVerified,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: plugin.Generation,
		Reason:             "VerificationDeferred",
		Message:            "signature verification deferred to m2",
	})
	apimeta.SetStatusCondition(&plugin.Status.Conditions, metav1.Condition{
		Type:               condLoaded,
		Status:             loadedStatus,
		ObservedGeneration: plugin.Generation,
		Reason:             "HostEnsured",
		Message:            fmt.Sprintf("plugin phase %s", plugin.Status.Phase),
	})

	if ready {
		// Record the digest in the audit trail (DESIGN.md §5.3, §17.5).
		if err := r.Store.RecordPluginAudit(ctx, &store.PluginAuditRecord{
			Plugin: plugin.Name,
			Kind:   string(plugin.Spec.Kind),
			Digest: plugin.Status.ResolvedDigest,
			Action: "installed",
			Actor:  "plugin-controller",
			At:     time.Now().UTC(),
		}); err != nil {
			// Audit failures should not wedge the load; surface as an Event.
			r.Recorder.Eventf(&plugin, corev1.EventTypeWarning, "AuditFailed", "record plugin audit: %v", err)
		}
		r.Recorder.Eventf(&plugin, corev1.EventTypeNormal, "Ready", "plugin loaded (digest %s)", plugin.Status.ResolvedDigest)
	}

	if err := r.Status().Update(ctx, &plugin); err != nil {
		return ctrl.Result{}, fmt.Errorf("update plugin status: %w", err)
	}
	log.Info("reconciled plugin", "phase", plugin.Status.Phase, "digest", plugin.Status.ResolvedDigest)
	return ctrl.Result{RequeueAfter: pluginResync}, nil
}

// SetupWithManager registers the reconciler for cluster-scoped Plugin objects.
func (r *PluginReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Plugin{}).
		Named("plugin").
		Complete(r)
}
