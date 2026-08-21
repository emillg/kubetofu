/*
Copyright 2026.

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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	tofuv1alpha1 "github.com/kubetofu/kubetofu/api/v1alpha1"
)

// TofuModuleReconciler reconciles a TofuModule object
type TofuModuleReconciler struct {
	client.Client
	// APIReader reads directly from the API server, bypassing the informer
	// cache, for checks that must be race-free (e.g. duplicate run detection).
	APIReader client.Reader
	Scheme    *runtime.Scheme

	// RunnerImage is the default container image used to execute OpenTofu
	// runs. Overridden by spec.runner.image.
	RunnerImage string
}

// +kubebuilder:rbac:groups=tofu.kubetofu.io,resources=tofumodules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tofu.kubetofu.io,resources=tofumodules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tofu.kubetofu.io,resources=tofumodules/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch

// Reconcile drives a TofuModule through its lifecycle:
//
//  1. If a run Job is in flight, report its phase and wait.
//  2. Process the outcome of the most recent finished run.
//  3. If there is no plan for the current spec (or drift is due), start a plan run.
//  4. If spec.approvePlan is set and the current plan has not been applied, start an apply run.
//  5. Otherwise report the steady state (Applied or PlanGenerated awaiting approval).
func (r *TofuModuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cr := &tofuv1alpha1.TofuModule{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if cr.Spec.Paused {
		return ctrl.Result{}, r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
			s.Phase = tofuv1alpha1.TofuModulePhaseSuspended
			setCondition(s, tofuv1alpha1.ConditionReady, metav1.ConditionFalse, "Paused", "reconciliation is paused")
		})
	}

	// Surface spec misconfigurations as a Failed phase instead of an endless
	// retry loop.
	if msg := validateSpec(cr); msg != "" {
		return ctrl.Result{}, r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
			s.Phase = tofuv1alpha1.TofuModulePhaseFailed
			setCondition(s, tofuv1alpha1.ConditionReady, metav1.ConditionFalse, "InvalidSpec", msg)
		})
	}

	specHash, err := hashSpec(cr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("hashing spec: %w", err)
	}
	specHash = shortHash(specHash)

	// Idempotent scaffolding owned by the module.
	if err := r.ensureSecret(ctx, cr, stateSecretName(cr.Name), stateSecretKey); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring state secret: %w", err)
	}
	if err := r.ensureSecret(ctx, cr, outputsSecretName(cr.Name), outputsSecretKey); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring outputs secret: %w", err)
	}
	if err := r.ensureRunnerRBAC(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring runner RBAC: %w", err)
	}

	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(cr.Namespace), client.MatchingLabels{moduleLabelKey: cr.Name}); err != nil {
		return ctrl.Result{}, err
	}
	active := newestJob(jobs.Items, false)
	finished := newestJob(jobs.Items, true)

	// 1. A run is in flight: report its phase and wait for it.
	if active != nil {
		phase := tofuv1alpha1.TofuModulePhasePlanning
		condType, reason := tofuv1alpha1.ConditionPlanGenerated, "Planning"
		if active.Labels[actionLabelKey] == actionApply {
			phase = tofuv1alpha1.TofuModulePhaseApplying
			condType, reason = tofuv1alpha1.ConditionApplySucceeded, "Applying"
		}
		err := r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
			s.Phase = phase
			setCondition(s, condType, metav1.ConditionFalse, reason, "run in progress")
		})
		return ctrl.Result{RequeueAfter: runPollInterval}, err
	}

	// 2. Process the outcome of the most recent finished run.
	if finished != nil {
		runHash := finished.Labels[runLabelKey]
		action := finished.Labels[actionLabelKey]
		reason := finished.Labels[reasonLabelKey]

		if !jobSucceeded(finished) {
			if handled, res, err := r.handleFailedRun(ctx, cr, finished, runHash, specHash, action, reason); handled {
				return res, err
			}
		} else if err := r.handleSucceededRun(ctx, cr, runHash, action, reason, specHash); err != nil {
			return ctrl.Result{}, err
		}
	}

	planCurrent := cr.Status.PlanHash == specHash
	appliedCurrent := cr.Status.LastAppliedPlanHash == specHash

	// 3. No plan for the current spec, or drift is due: start a plan run.
	if !planCurrent || driftDue(cr) {
		reason := reasonSpec
		if planCurrent {
			reason = reasonDrift
		}
		if err := r.createRun(ctx, cr, actionPlan, reason, specHash); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating plan run: %w", err)
		}
		return ctrl.Result{RequeueAfter: runPollInterval}, nil
	}

	// 4. The plan is approved and not yet applied: start an apply run.
	if cr.Spec.ApprovePlan && !appliedCurrent {
		if err := r.createRun(ctx, cr, actionApply, reasonSpec, specHash); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating apply run: %w", err)
		}
		return ctrl.Result{RequeueAfter: runPollInterval}, nil
	}

	// 5. Steady state.
	if cr.Status.Phase == tofuv1alpha1.TofuModulePhaseFailed {
		// Stay Failed until something changes (e.g. a spec edit); do not
		// resurrect Applied or re-trigger drift detection.
		return ctrl.Result{}, nil
	}
	if appliedCurrent {
		err := r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
			s.Phase = tofuv1alpha1.TofuModulePhaseApplied
			setCondition(s, tofuv1alpha1.ConditionReady, metav1.ConditionTrue, "Applied", "desired state applied")
		})
		// Poll so drift detection runs on the configured interval.
		return ctrl.Result{RequeueAfter: retryInterval(cr)}, err
	}

	// A plan exists for the current spec but has not been approved.
	err = r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
		s.Phase = tofuv1alpha1.TofuModulePhasePlanGenerated
		setCondition(s, tofuv1alpha1.ConditionPlanGenerated, metav1.ConditionTrue, "PlanReady", "plan generated; set spec.approvePlan=true to apply")
	})
	return ctrl.Result{}, err
}

// handleFailedRun records a failed run's outcome in the module status (once)
// and keeps the failed Job around so its pod logs remain available for
// debugging. It returns handled=true with a result when the failure needs
// recording or a retry is not yet due; handled=false when the reconcile should
// continue to start a new run (retry interval elapsed, or the spec changed).
func (r *TofuModuleReconciler) handleFailedRun(ctx context.Context, cr *tofuv1alpha1.TofuModule, finished *batchv1.Job, runHash, specHash, action, reason string) (bool, ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Record the failure once and keep the Job (and its pod logs) around for
	// debugging; a marker label prevents re-recording and gates the retry on
	// the configured interval.
	if finished.Labels[failureRecordedKey] == "" {
		msg := jobFailureMessage(finished)
		condType, reasonStr := tofuv1alpha1.ConditionPlanGenerated, "PlanFailed"
		if action == actionApply {
			condType, reasonStr = tofuv1alpha1.ConditionApplySucceeded, "ApplyFailed"
		}
		err := r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
			s.Phase = tofuv1alpha1.TofuModulePhaseFailed
			setCondition(s, condType, metav1.ConditionFalse, reasonStr, msg)
			setCondition(s, tofuv1alpha1.ConditionReady, metav1.ConditionFalse, reasonStr, msg)
		})
		if err != nil {
			return true, ctrl.Result{}, err
		}
		if err := r.markRunFailed(ctx, finished); err != nil {
			return true, ctrl.Result{}, fmt.Errorf("marking run as failed: %w", err)
		}
		log.Info("tofu run failed", "action", action, "reason", reason, "run", runHash, "message", msg)
		// A failed drift plan is not retried automatically (it would loop
		// forever); recover by changing the spec. Other failures retry on
		// the configured interval.
		if reason == reasonDrift {
			return true, ctrl.Result{}, nil
		}
		return true, ctrl.Result{RequeueAfter: retryInterval(cr)}, nil
	}

	// Failure already recorded: keep the Job for its logs. Start a retry only
	// once the retry interval has elapsed since the failure was recorded (and
	// only for the current spec — a spec change starts a new run at once).
	if runHash == specHash {
		if remaining := retryInterval(cr) - time.Since(recordedAt(finished)); remaining > 0 {
			return true, ctrl.Result{RequeueAfter: remaining}, nil
		}
	}
	return false, ctrl.Result{}, nil
}

// handleSucceededRun processes the outcome of a successfully finished run,
// updating the module status with the resulting plan hash or apply results.
func (r *TofuModuleReconciler) handleSucceededRun(ctx context.Context, cr *tofuv1alpha1.TofuModule, runHash, action, reason, specHash string) error {
	if action == actionPlan {
		// A drift re-plan invalidates the previous approval so that any
		// changes it detects must be (re-)approved before applying.
		if reason == reasonDrift && cr.Status.LastAppliedPlanHash != "" {
			err := r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
				s.LastAppliedPlanHash = ""
				s.Phase = tofuv1alpha1.TofuModulePhasePlanGenerated
				setCondition(s, tofuv1alpha1.ConditionPlanGenerated, metav1.ConditionTrue, "PlanReady", "drift detected; plan awaits approval")
			})
			if err != nil {
				return err
			}
		}
		if cr.Status.PlanHash != runHash {
			err := r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
				s.PlanHash = runHash
				s.PlanSecretRef = &corev1.LocalObjectReference{Name: planSecretName(cr.Name, runHash)}
				if runHash == specHash {
					s.Phase = tofuv1alpha1.TofuModulePhasePlanGenerated
					setCondition(s, tofuv1alpha1.ConditionPlanGenerated, metav1.ConditionTrue, "PlanReady", "plan generated for the current spec")
				}
			})
			if err != nil {
				return err
			}
		}
	}
	if action == actionApply && cr.Status.LastAppliedPlanHash != runHash {
		outputs, err := r.readOutputs(ctx, cr)
		if err != nil {
			return err
		}
		now := metav1.Now()
		err = r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
			s.LastAppliedPlanHash = runHash
			s.LastAppliedTime = &now
			s.Outputs = outputs
			s.Phase = tofuv1alpha1.TofuModulePhaseApplied
			setCondition(s, tofuv1alpha1.ConditionPlanGenerated, metav1.ConditionTrue, "PlanReady", "plan generated for the current spec")
			setCondition(s, tofuv1alpha1.ConditionApplySucceeded, metav1.ConditionTrue, "ApplySucceeded", "apply completed successfully")
			setCondition(s, tofuv1alpha1.ConditionReady, metav1.ConditionTrue, "Applied", "desired state applied")
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// patchStatus applies a mutation to the TofuModule status via a merge patch and
// keeps observedGeneration up to date.
func (r *TofuModuleReconciler) patchStatus(ctx context.Context, cr *tofuv1alpha1.TofuModule, mutate func(*tofuv1alpha1.TofuModuleStatus)) error {
	original := cr.DeepCopy()
	cr.Status.ObservedGeneration = cr.Generation
	mutate(&cr.Status)
	return r.Status().Patch(ctx, cr, client.MergeFrom(original))
}

// validateSpec returns a human-readable message describing the first spec
// problem found, or "" when the spec is valid. It enforces the invariants
// that cannot be expressed as CEL rules on open-schema JSON fields.
func validateSpec(cr *tofuv1alpha1.TofuModule) string {
	for _, v := range cr.Spec.Variables {
		hasValue := v.Value != nil
		hasValueFrom := v.ValueFrom != nil && (v.ValueFrom.SecretKeyRef != nil || v.ValueFrom.ConfigMapKeyRef != nil)
		if hasValue == hasValueFrom {
			return fmt.Sprintf("variable %q: exactly one of value or valueFrom must be set", v.Name)
		}
	}
	return ""
}

// setCondition upserts a condition on the status.
func setCondition(s *tofuv1alpha1.TofuModuleStatus, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: s.ObservedGeneration,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *TofuModuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tofuv1alpha1.TofuModule{}).
		Owns(&batchv1.Job{}).
		Named("tofumodule").
		Complete(r)
}
