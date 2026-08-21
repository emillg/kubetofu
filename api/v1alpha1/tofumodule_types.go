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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TofuModuleSpec defines the desired state of TofuModule
type TofuModuleSpec struct {
	// Module is the source of the OpenTofu configuration.
	// +required
	Module ModuleSource `json:"module"`

	// Variables are the input variables passed to OpenTofu on every run.
	// They are rendered into a terraform.tfvars.json file in the module directory.
	// +optional
	Variables []Variable `json:"variables,omitempty"`

	// Backend configures the OpenTofu state backend.
	// When omitted, the controller stores state in a Kubernetes Secret.
	// +optional
	Backend *BackendConfig `json:"backend,omitempty"`

	// ApprovePlan approves the most recent plan for apply.
	// Setting it to true triggers an apply of the plan generated from the
	// current spec. It is idempotent: once the plan is applied, keeping it
	// true has no effect until the spec changes and a new plan is generated.
	// +optional
	ApprovePlan bool `json:"approvePlan,omitempty"`

	// Runner configures the pod that executes OpenTofu.
	// +optional
	Runner *RunnerConfig `json:"runner,omitempty"`

	// Interval is the period after which the controller re-plans the module
	// to detect drift. Defaults to 10m when unset. Set to 0 to disable
	// drift detection.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Paused suspends reconciliation of this module.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// ModuleSource describes where the OpenTofu configuration lives.
// +kubebuilder:validation:XValidation:rule="has(self.git) || has(self.configMapRef)",message="exactly one of git or configMapRef must be set"
type ModuleSource struct {
	// Git points to a git repository that contains the OpenTofu configuration.
	// +optional
	Git *GitSource `json:"git,omitempty"`

	// ConfigMapRef references a ConfigMap that contains the OpenTofu configuration files.
	// +optional
	ConfigMapRef *corev1.LocalObjectReference `json:"configMapRef,omitempty"`
}

// GitSource describes a git repository containing the OpenTofu configuration.
type GitSource struct {
	// URL is the git repository URL, e.g. https://github.com/org/repo.
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// Ref is the git reference to check out: branch, tag, or commit SHA.
	// Defaults to the default branch HEAD when unset.
	// +optional
	Ref string `json:"ref,omitempty"`

	// SubPath is a directory inside the repository that contains the
	// OpenTofu configuration (the module root). Defaults to the repository root.
	// +optional
	SubPath string `json:"subPath,omitempty"`

	// SecretRef references a Secret with git credentials (username/password
	// for HTTPS, or SSH key for git+ssh).
	// +optional
	SecretRef *corev1.SecretReference `json:"secretRef,omitempty"`
}

// Variable is a single input variable passed to OpenTofu.
//
// NOTE: exactly one of Value or ValueFrom must be set. This is enforced by the
// controller (see validateSpec); a CEL rule cannot reference the open-schema
// JSON fields (Kubernetes CEL does not expose x-kubernetes-preserve-unknown-fields
// fields to validation rules).
type Variable struct {
	// Name is the variable name as declared in the module.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Value is the literal variable value. Any JSON value is allowed.
	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`

	// ValueFrom resolves the value from a Secret or ConfigMap key.
	// +optional
	ValueFrom *VariableValueFrom `json:"valueFrom,omitempty"`
}

// VariableValueFrom resolves a variable value from a Secret or ConfigMap.
type VariableValueFrom struct {
	// SecretKeyRef references a key in a Secret.
	// +optional
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`

	// ConfigMapKeyRef references a key in a ConfigMap.
	// +optional
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`
}

// BackendConfig configures the OpenTofu state backend.
type BackendConfig struct {
	// Type is the backend type, e.g. s3, azurerm, gcs, kubernetes.
	// +kubebuilder:validation:MinLength=1
	Type string `json:"type"`

	// Config is the backend configuration as a JSON object. It is rendered
	// as a JSON file and passed to OpenTofu via -backend-config.
	// +kubebuilder:validation:Type=object
	// +optional
	Config *apiextensionsv1.JSON `json:"config,omitempty"`
}

// RunnerConfig configures the pod that executes OpenTofu.
type RunnerConfig struct {
	// Image overrides the runner container image.
	// +optional
	Image string `json:"image,omitempty"`

	// ServiceAccountName is the ServiceAccount used by the runner pod.
	// Defaults to the controller's default runner ServiceAccount.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// Resources are the compute resources for the runner pod.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env are additional environment variables for the runner pod.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
}

// TofuModuleStatus defines the observed state of TofuModule
type TofuModuleStatus struct {
	// ObservedGeneration is the generation of the spec most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RunSequence is a monotonically increasing counter of runs started for
	// this module. It orders runs that were created within the same second.
	// +optional
	RunSequence int64 `json:"runSequence,omitempty"`

	// Phase is a high-level summary of the reconciliation state.
	// +kubebuilder:validation:Enum=Pending;Planning;PlanGenerated;Applying;Applied;Failed;Suspended
	// +optional
	Phase TofuModulePhase `json:"phase,omitempty"`

	// PlanHash is the hash of the spec the current plan was generated from.
	// A mismatch between PlanHash and the current spec hash means a new plan is needed.
	// +optional
	PlanHash string `json:"planHash,omitempty"`

	// PlanSecretRef references the Secret holding the current plan output.
	// +optional
	PlanSecretRef *corev1.LocalObjectReference `json:"planSecretRef,omitempty"`

	// StateSecretRef references the Secret holding the OpenTofu state.
	// +optional
	StateSecretRef *corev1.LocalObjectReference `json:"stateSecretRef,omitempty"`

	// LastAppliedPlanHash is the hash of the spec the last successful apply ran on.
	// +optional
	LastAppliedPlanHash string `json:"lastAppliedPlanHash,omitempty"`

	// LastAppliedTime is the time the last successful apply completed.
	// +optional
	LastAppliedTime *metav1.Time `json:"lastAppliedTime,omitempty"`

	// Outputs are the module outputs from the last successful apply.
	// +optional
	Outputs []Output `json:"outputs,omitempty"`

	// Conditions represent the latest available observations of the resource.
	// Condition types: PlanGenerated, ApplySucceeded, Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Output is a single OpenTofu module output.
type Output struct {
	// Name is the output name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Value is the output value.
	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`

	// Sensitive indicates the output is marked sensitive and its value is
	// not stored in the status.
	// +optional
	Sensitive bool `json:"sensitive,omitempty"`
}

// TofuModulePhase is the reconciliation phase of a TofuModule.
type TofuModulePhase string

const (
	// TofuModulePhasePending means the module was queued and no run has started yet.
	TofuModulePhasePending TofuModulePhase = "Pending"
	// TofuModulePhasePlanning means a plan run is in progress.
	TofuModulePhasePlanning TofuModulePhase = "Planning"
	// TofuModulePhasePlanGenerated means a plan exists for the current spec and awaits approval.
	TofuModulePhasePlanGenerated TofuModulePhase = "PlanGenerated"
	// TofuModulePhaseApplying means an apply run is in progress.
	TofuModulePhaseApplying TofuModulePhase = "Applying"
	// TofuModulePhaseApplied means the current spec was successfully applied.
	TofuModulePhaseApplied TofuModulePhase = "Applied"
	// TofuModulePhaseFailed means the last run failed.
	TofuModulePhaseFailed TofuModulePhase = "Failed"
	// TofuModulePhaseSuspended means reconciliation is paused.
	TofuModulePhaseSuspended TofuModulePhase = "Suspended"
)

// Condition types used on TofuModule.
const (
	// ConditionPlanGenerated is True when a plan exists for the current spec.
	ConditionPlanGenerated = "PlanGenerated"
	// ConditionApplySucceeded is True when the last apply succeeded.
	ConditionApplySucceeded = "ApplySucceeded"
	// ConditionReady is True when the desired state is applied and no run is pending.
	ConditionReady = "Ready"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// TofuModule is the Schema for the tofumodules API
type TofuModule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of TofuModule
	// +required
	Spec TofuModuleSpec `json:"spec"`

	// status defines the observed state of TofuModule
	// +optional
	Status TofuModuleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TofuModuleList contains a list of TofuModule
type TofuModuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TofuModule `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TofuModule{}, &TofuModuleList{})
		return nil
	})
}
