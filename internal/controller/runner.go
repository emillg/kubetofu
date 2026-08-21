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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	tofuv1alpha1 "github.com/kubetofu/kubetofu/api/v1alpha1"
)

// The runner image is expected to honor the following contract:
//
// Environment:
//
//	TOFU_ACTION          "plan" or "apply"
//	TOFU_NAMESPACE       namespace of the TofuModule
//	TOFU_STATE_SECRET    Secret holding terraform.tfstate (read on apply, written on plan and apply)
//	TOFU_PLAN_SECRET     Secret holding the saved plan (written by plan runs, read by apply runs)
//	TOFU_OUTPUTS_SECRET  Secret that outputs.json is written to after a successful apply
//	TOFU_GIT_URL         git repository URL (git module source)
//	TOFU_GIT_REF         git ref to check out
//	TOFU_GIT_SUBPATH     directory inside the repository containing the module
//	TOFU_CONFIGMAP       ConfigMap holding the module files (configMapRef module source)
//	TOFU_BACKEND_TYPE    state backend type (s3, azurerm, gcs, ...)
//
// Optional path overrides (defaults to the mounted /tofu paths):
//
//	TOFU_CONFIG_DIR            directory holding the module files (configMapRef source)
//	TOFU_TFVARS_FILE           rendered terraform.tfvars.json
//	TOFU_BACKEND_CONFIG_FILE   backend configuration JSON
//
// Mounted volumes:
//
//	/tofu/tfvars.json          rendered terraform.tfvars.json (always)
//	/tofu/backend-config.json  backend configuration (when spec.backend is set)
//	/tofu/config               module files (when a configMapRef module source is used)
//
// Plan runs: prepare the workspace (git clone or copy /tofu/config), run
// `tofu init` and `tofu plan -out=plan.tfplan -var-file=/tofu/tfvars.json`, then
// patch the plan Secret's data with plan.tfplan and plan.txt, and patch the state
// Secret's data with terraform.tfstate.
//
// Apply runs: prepare the workspace the same way, run `tofu init`, download
// plan.tfplan from the plan Secret, run `tofu apply plan.tfplan`, then patch the
// state Secret with terraform.tfstate and write outputs.json to the outputs Secret
// (using `tofu output -json`).
const (
	// DefaultRunnerImage is the image used to execute OpenTofu runs when neither
	// the --runner-image flag nor spec.runner.image is set.
	DefaultRunnerImage = "ghcr.io/kubetofu/tofu-runner:latest"

	// defaultInterval is used for drift detection and failure retries when
	// spec.interval is not set.
	defaultInterval = 10 * time.Minute
	// runPollInterval is how often the controller re-checks an in-flight run.
	runPollInterval = 5 * time.Second

	moduleLabelKey = "kubetofu.io/module"
	runLabelKey    = "kubetofu.io/run"
	actionLabelKey = "kubetofu.io/action"
	reasonLabelKey = "kubetofu.io/reason"
	seqLabelKey    = "kubetofu.io/seq"
	// failureRecordedKey marks a failed run Job once its failure has been
	// surfaced in the module status. The Job is kept (so its pod logs remain
	// available for debugging) instead of being deleted; the label's value is
	// the time the failure was recorded, used to gate the retry interval.
	failureRecordedKey = "kubetofu.io/failure-recorded"

	actionPlan  = "plan"
	actionApply = "apply"

	// reasonSpec marks a run triggered by a spec change, reasonDrift a run
	// triggered by drift detection.
	reasonSpec  = "spec"
	reasonDrift = "drift"

	// runnerSA is the ServiceAccount (and Role/RoleBinding name) provisioned in
	// the TofuModule's namespace for runner pods.
	runnerSA = "tofu-runner"

	stateSecretKey      = "terraform.tfstate"
	planSecretPlanKey   = "plan.tfplan"
	planSecretOutputKey = "plan.txt"
	outputsSecretKey    = "outputs.json"

	cfgVarsKey    = "tfvars.json"
	cfgBackendKey = "backend-config.json"
)

// planHashSpec captures the parts of the spec that determine the OpenTofu plan.
// Operational fields (approvePlan, interval, paused, runner) are excluded so
// that toggling them does not invalidate an existing plan.
type planHashSpec struct {
	Module    tofuv1alpha1.ModuleSource   `json:"module"`
	Variables []tofuv1alpha1.Variable     `json:"variables,omitempty"`
	Backend   *tofuv1alpha1.BackendConfig `json:"backend,omitempty"`
}

// hashSpec returns a stable SHA-256 hash of the plan-relevant spec, used to
// detect changes that require a new plan and to name runs, config maps and
// plan secrets.
func hashSpec(cr *tofuv1alpha1.TofuModule) (string, error) {
	b, err := json.Marshal(planHashSpec{
		Module:    cr.Spec.Module,
		Variables: cr.Spec.Variables,
		Backend:   cr.Spec.Backend,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// shortHash returns a compact prefix of a spec hash, safe for resource names.
func shortHash(hash string) string {
	const n = 12
	if len(hash) > n {
		return hash[:n]
	}
	return hash
}

// truncate shortens s to at most n bytes. Module names are DNS-1123 labels
// (ASCII), so byte truncation is safe.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// jobName returns the name of the run Job for a module, action, spec hash and
// sequence number. The sequence makes names unique and orders runs even when
// they were created within the same second.
func jobName(module, action, hash string, seq int64) string {
	return fmt.Sprintf("tofu-%s-%s-%s-%d", truncate(module, 32), action, shortHash(hash), seq)
}

// jobSeq returns the run sequence of a Job, 0 when unknown.
func jobSeq(j *batchv1.Job) int64 {
	n, _ := strconv.ParseInt(j.Labels[seqLabelKey], 10, 64)
	return n
}

// cfgName returns the name of the per-run ConfigMap holding rendered inputs.
func cfgName(module, hash string) string {
	return fmt.Sprintf("tofu-%s-cfg-%s", truncate(module, 40), shortHash(hash))
}

// planSecretName returns the name of the per-run Secret holding the saved plan.
func planSecretName(module, hash string) string {
	return fmt.Sprintf("tofu-%s-plan-%s", truncate(module, 40), shortHash(hash))
}

// stateSecretName returns the name of the Secret holding the module state.
func stateSecretName(module string) string {
	return "tofu-" + truncate(module, 40) + "-state"
}

// outputsSecretName returns the name of the Secret holding module outputs.
func outputsSecretName(module string) string {
	return "tofu-" + truncate(module, 40) + "-outputs"
}

// jobFinished reports whether a Job has reached a terminal state.
func jobFinished(j *batchv1.Job) bool {
	return j.Status.CompletionTime != nil || j.Status.Succeeded > 0 || j.Status.Failed > 0
}

// jobSucceeded reports whether a Job completed successfully.
func jobSucceeded(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// jobFailureMessage extracts a human-readable failure message from a Job.
func jobFailureMessage(j *batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return c.Message
			}
			return c.Reason
		}
	}
	if j.Status.Failed > 0 {
		return fmt.Sprintf("job failed (%d failed pod)", j.Status.Failed)
	}
	return "job did not complete successfully"
}

// markRunFailed labels a failed run Job with the time its failure was recorded
// so the failure is surfaced in the module status only once. The Job itself is
// kept so its pod logs remain available until it expires via TTL.
func (r *TofuModuleReconciler) markRunFailed(ctx context.Context, job *batchv1.Job) error {
	original := job.DeepCopy()
	if job.Labels == nil {
		job.Labels = map[string]string{}
	}
	job.Labels[failureRecordedKey] = strconv.FormatInt(time.Now().UnixNano(), 10)
	return r.Patch(ctx, job, client.MergeFrom(original))
}

// recordedAt returns the time a failed run's failure was recorded, or the zero
// time when the marker label is missing or unparseable.
func recordedAt(j *batchv1.Job) time.Time {
	n, err := strconv.ParseInt(j.Labels[failureRecordedKey], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// newestJob returns the job with the highest run sequence among the finished
// (or unfinished) jobs, or nil. The run sequence, not the creation timestamp,
// defines run order: timestamps only have second precision.
func newestJob(jobs []batchv1.Job, finished bool) *batchv1.Job {
	var best *batchv1.Job
	for i := range jobs {
		j := &jobs[i]
		if jobFinished(j) != finished {
			continue
		}
		if best == nil || jobSeq(j) > jobSeq(best) {
			best = j
		}
	}
	return best
}

// retryInterval returns the interval used for drift detection and failure
// retries, honoring spec.interval (0 disables both).
func retryInterval(cr *tofuv1alpha1.TofuModule) time.Duration {
	if cr.Spec.Interval != nil {
		return cr.Spec.Interval.Duration
	}
	return defaultInterval
}

// driftDue reports whether a drift-detection re-plan is due.
func driftDue(cr *tofuv1alpha1.TofuModule) bool {
	if cr.Status.Phase != tofuv1alpha1.TofuModulePhaseApplied {
		return false
	}
	interval := retryInterval(cr)
	if interval <= 0 || cr.Status.LastAppliedTime == nil {
		return false
	}
	return time.Since(cr.Status.LastAppliedTime.Time) >= interval
}

// createRun creates the config inputs and the run Job for a plan or apply run.
// It replaces any previous finished Job with the same name (e.g. a drift
// re-plan or a retry) so names stay deterministic and no two runs of the same
// kind can race on the same state.
// createRun starts a new plan or apply run: it records the run's sequence in
// the module status and creates the run Job. The sequence is persisted before
// the Job is created so that run order is stable across reconciles.
func (r *TofuModuleReconciler) createRun(ctx context.Context, cr *tofuv1alpha1.TofuModule, action, reason, hash string) error {
	// Guard against duplicate runs: a racing reconcile may try to start
	// another run for the same module/action/hash while one is already in
	// flight or awaiting processing. Query the API server directly to bypass
	// the informer cache, which can lag behind recent writes.
	//
	// A run is considered current (and creation skipped) when its sequence
	// number is at least the status RunSequence: it is either the run we are
	// racing to create or a successor that has not been processed yet. Older
	// runs (e.g. a previous apply invalidated by a drift re-plan) do not block
	// new runs. Failed jobs never block.
	existing := &batchv1.JobList{}
	if err := r.APIReader.List(ctx, existing, client.InNamespace(cr.Namespace),
		client.MatchingLabels{moduleLabelKey: cr.Name, actionLabelKey: action, runLabelKey: hash}); err != nil {
		return err
	}
	for i := range existing.Items {
		j := &existing.Items[i]
		if jobFinished(j) && !jobSucceeded(j) {
			continue
		}
		if jobSeq(j) >= cr.Status.RunSequence {
			return nil
		}
	}

	if action == actionPlan {
		if err := r.ensureRunConfigMap(ctx, cr, hash); err != nil {
			return err
		}
		if err := r.ensureSecret(ctx, cr, planSecretName(cr.Name, hash), planSecretPlanKey, planSecretOutputKey); err != nil {
			return err
		}
	}

	seq := cr.Status.RunSequence + 1
	job, err := r.buildRunnerJob(cr, action, reason, hash, seq)
	if err != nil {
		return err
	}

	phase := tofuv1alpha1.TofuModulePhasePlanning
	condType, condReason := tofuv1alpha1.ConditionPlanGenerated, "Planning"
	if action == actionApply {
		phase = tofuv1alpha1.TofuModulePhaseApplying
		condType, condReason = tofuv1alpha1.ConditionApplySucceeded, "Applying"
	}
	if err := r.patchStatus(ctx, cr, func(s *tofuv1alpha1.TofuModuleStatus) {
		s.RunSequence = seq
		s.Phase = phase
		setCondition(s, condType, metav1.ConditionFalse, condReason, "run in progress")
	}); err != nil {
		return err
	}
	return client.IgnoreAlreadyExists(r.Create(ctx, job))
}

// buildRunnerJob builds the runner Job for a plan or apply run.
func (r *TofuModuleReconciler) buildRunnerJob(cr *tofuv1alpha1.TofuModule, action, reason, hash string, seq int64) (*batchv1.Job, error) {
	image := DefaultRunnerImage
	sa := runnerSA
	if r.RunnerImage != "" {
		image = r.RunnerImage
	}
	if cr.Spec.Runner != nil {
		if cr.Spec.Runner.Image != "" {
			image = cr.Spec.Runner.Image
		}
		if cr.Spec.Runner.ServiceAccountName != "" {
			sa = cr.Spec.Runner.ServiceAccountName
		}
	}

	env := []corev1.EnvVar{
		{Name: "TOFU_ACTION", Value: action},
		{Name: "TOFU_NAMESPACE", Value: cr.Namespace},
		{Name: "TOFU_STATE_SECRET", Value: stateSecretName(cr.Name)},
		{Name: "TOFU_PLAN_SECRET", Value: planSecretName(cr.Name, hash)},
		{Name: "TOFU_OUTPUTS_SECRET", Value: outputsSecretName(cr.Name)},
	}
	if cr.Spec.Module.Git != nil {
		env = append(env,
			corev1.EnvVar{Name: "TOFU_GIT_URL", Value: cr.Spec.Module.Git.URL},
			corev1.EnvVar{Name: "TOFU_GIT_REF", Value: cr.Spec.Module.Git.Ref},
			corev1.EnvVar{Name: "TOFU_GIT_SUBPATH", Value: cr.Spec.Module.Git.SubPath},
		)
	}
	if cr.Spec.Module.ConfigMapRef != nil {
		env = append(env, corev1.EnvVar{Name: "TOFU_CONFIGMAP", Value: cr.Spec.Module.ConfigMapRef.Name})
	}
	if cr.Spec.Backend != nil {
		env = append(env, corev1.EnvVar{Name: "TOFU_BACKEND_TYPE", Value: cr.Spec.Backend.Type})
	}
	if cr.Spec.Runner != nil {
		env = append(env, cr.Spec.Runner.Env...)
	}

	volumes := []corev1.Volume{{
		Name: "vars",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: cfgName(cr.Name, hash)},
			Items:                []corev1.KeyToPath{{Key: cfgVarsKey, Path: cfgVarsKey}},
		}},
	}}
	mounts := []corev1.VolumeMount{
		{Name: "vars", MountPath: "/tofu/" + cfgVarsKey, SubPath: cfgVarsKey, ReadOnly: true},
	}
	if cr.Spec.Backend != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "backend",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cfgName(cr.Name, hash)},
				Items:                []corev1.KeyToPath{{Key: cfgBackendKey, Path: cfgBackendKey}},
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "backend", MountPath: "/tofu/" + cfgBackendKey, SubPath: cfgBackendKey, ReadOnly: true})
	}
	if cr.Spec.Module.ConfigMapRef != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "module",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: *cr.Spec.Module.ConfigMapRef,
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "module", MountPath: "/tofu/config", ReadOnly: true})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(cr.Name, action, hash, seq),
			Namespace: cr.Namespace,
			Labels: map[string]string{
				moduleLabelKey: cr.Name,
				runLabelKey:    hash,
				actionLabelKey: action,
				reasonLabelKey: reason,
				seqLabelKey:    strconv.FormatInt(seq, 10),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(0)),
			TTLSecondsAfterFinished: ptr.To(int32(600)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: sa,
					Containers: []corev1.Container{{
						Name:         "tofu",
						Image:        image,
						Env:          env,
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cr, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil
}

// ensureRunConfigMap creates the per-run ConfigMap with the rendered tfvars
// and backend configuration if it does not exist yet.
func (r *TofuModuleReconciler) ensureRunConfigMap(ctx context.Context, cr *tofuv1alpha1.TofuModule, hash string) error {
	name := cfgName(cr.Name, hash)
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: name}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	data := map[string]string{}
	vars, err := r.renderVars(ctx, cr)
	if err != nil {
		return err
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return err
	}
	data[cfgVarsKey] = string(b)
	if cr.Spec.Backend != nil && cr.Spec.Backend.Config != nil {
		data[cfgBackendKey] = string(cr.Spec.Backend.Config.Raw)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			Labels:    map[string]string{moduleLabelKey: cr.Name},
		},
		Data: data,
	}
	if err := controllerutil.SetControllerReference(cr, cm, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, cm)
}

// ensureSecret creates an owned Secret with the given (empty) data keys if it
// does not exist yet. Runner pods write the actual content.
func (r *TofuModuleReconciler) ensureSecret(ctx context.Context, cr *tofuv1alpha1.TofuModule, name string, keys ...string) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: name}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	data := make(map[string][]byte, len(keys))
	for _, k := range keys {
		data[k] = []byte{}
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			Labels:    map[string]string{moduleLabelKey: cr.Name},
		},
		Data: data,
	}
	if err := controllerutil.SetControllerReference(cr, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

// ensureRunnerRBAC provisions the ServiceAccount, Role and RoleBinding the
// runner pods use to read and write Secrets, unless a user-managed ServiceAccount
// is configured via spec.runner.serviceAccountName.
func (r *TofuModuleReconciler) ensureRunnerRBAC(ctx context.Context, cr *tofuv1alpha1.TofuModule) error {
	if cr.Spec.Runner != nil && cr.Spec.Runner.ServiceAccountName != "" {
		return nil
	}
	ns := cr.Namespace

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: runnerSA, Namespace: ns}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sa), sa); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: runnerSA, Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
		}},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(role), role); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: runnerSA, Namespace: ns},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: runnerSA, Namespace: ns}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: runnerSA},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.Create(ctx, binding); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

// renderVars renders the module variables as a JSON object for
// terraform.tfvars.json. Values are either literal JSON or resolved from the
// referenced Secret/ConfigMap keys (as strings).
func (r *TofuModuleReconciler) renderVars(ctx context.Context, cr *tofuv1alpha1.TofuModule) (map[string]any, error) {
	vars := map[string]any{}
	for _, v := range cr.Spec.Variables {
		switch {
		case v.Value != nil:
			var val any
			if err := json.Unmarshal(v.Value.Raw, &val); err != nil {
				return nil, fmt.Errorf("variable %q: %w", v.Name, err)
			}
			vars[v.Name] = val
		case v.ValueFrom != nil:
			val, err := r.resolveVariableValue(ctx, cr.Namespace, v)
			if err != nil {
				return nil, err
			}
			vars[v.Name] = val
		}
	}
	return vars, nil
}

// resolveVariableValue resolves a variable from a Secret or ConfigMap key.
func (r *TofuModuleReconciler) resolveVariableValue(ctx context.Context, namespace string, v tofuv1alpha1.Variable) (string, error) {
	if v.ValueFrom.SecretKeyRef != nil {
		sel := v.ValueFrom.SecretKeyRef
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sel.Name}, secret); err != nil {
			return "", fmt.Errorf("variable %q: resolving secret %q: %w", v.Name, sel.Name, err)
		}
		val, ok := secret.Data[sel.Key]
		if !ok {
			return "", fmt.Errorf("variable %q: secret %q has no key %q", v.Name, sel.Name, sel.Key)
		}
		return string(val), nil
	}
	if v.ValueFrom.ConfigMapKeyRef != nil {
		sel := v.ValueFrom.ConfigMapKeyRef
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sel.Name}, cm); err != nil {
			return "", fmt.Errorf("variable %q: resolving configmap %q: %w", v.Name, sel.Name, err)
		}
		val, ok := cm.Data[sel.Key]
		if !ok {
			return "", fmt.Errorf("variable %q: configmap %q has no key %q", v.Name, sel.Name, sel.Key)
		}
		return val, nil
	}
	return "", fmt.Errorf("variable %q: no value or valueFrom set", v.Name)
}

// readOutputs reads the outputs written by the runner after an apply.
func (r *TofuModuleReconciler) readOutputs(ctx context.Context, cr *tofuv1alpha1.TofuModule) ([]tofuv1alpha1.Output, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: outputsSecretName(cr.Name)}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	data, ok := secret.Data[outputsSecretKey]
	if !ok || len(data) == 0 {
		return nil, nil
	}
	return parseOutputs(data)
}

// parseOutputs parses the output of `tofu output -json`:
//
//	{"name": {"sensitive": false, "type": "string", "value": "..."}}
//
// Sensitive values are reported by name only, without their value.
func parseOutputs(data []byte) ([]tofuv1alpha1.Output, error) {
	var raw map[string]struct {
		Sensitive bool            `json:"sensitive"`
		Value     json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	outputs := make([]tofuv1alpha1.Output, 0, len(raw))
	for name, o := range raw {
		out := tofuv1alpha1.Output{Name: name, Sensitive: o.Sensitive}
		if !o.Sensitive && len(o.Value) > 0 {
			out.Value = &apiextensionsv1.JSON{Raw: o.Value}
		}
		outputs = append(outputs, out)
	}
	slices.SortFunc(outputs, func(a, b tofuv1alpha1.Output) int { return cmp.Compare(a.Name, b.Name) })
	return outputs, nil
}
