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
	"sort"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tofuv1alpha1 "github.com/kubetofu/kubetofu/api/v1alpha1"
)

const testNamespace = "default"

func newTestReconciler() *TofuModuleReconciler {
	return &TofuModuleReconciler{
		Client:      k8sClient,
		APIReader:   k8sClient,
		Scheme:      k8sClient.Scheme(),
		RunnerImage: "ghcr.io/kubetofu/tofu-runner:test",
	}
}

func newTestModule(name string, mutate func(*tofuv1alpha1.TofuModule)) *tofuv1alpha1.TofuModule {
	cr := &tofuv1alpha1.TofuModule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: tofuv1alpha1.TofuModuleSpec{
			Module: tofuv1alpha1.ModuleSource{
				Git: &tofuv1alpha1.GitSource{URL: "https://github.com/example/example-infra", Ref: "main"},
			},
		},
	}
	if mutate != nil {
		mutate(cr)
	}
	return cr
}

func testReconcile(r *TofuModuleReconciler, name string) ctrl.Result {
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
	Expect(err).NotTo(HaveOccurred())
	return res
}

func testGetModule(name string) *tofuv1alpha1.TofuModule {
	cr := &tofuv1alpha1.TofuModule{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, cr)).To(Succeed())
	return cr
}

// listTestJobs returns the run Jobs for a module and action, oldest run first.
func listTestJobs(name, action string) []batchv1.Job {
	jobs := &batchv1.JobList{}
	Expect(k8sClient.List(context.Background(), jobs,
		client.InNamespace(testNamespace),
		client.MatchingLabels{moduleLabelKey: name, actionLabelKey: action},
	)).To(Succeed())
	out := jobs.Items
	sort.Slice(out, func(i, j int) bool { return jobSeq(&out[i]) < jobSeq(&out[j]) })
	return out
}

// finishTestJob marks a Job as completed or failed in the apiserver.
func finishTestJob(job *batchv1.Job, succeeded bool) {
	now := metav1.Now()
	start := metav1.NewTime(now.Time.Add(-time.Minute))
	job.Status.StartTime = &start
	if succeeded {
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, Reason: "Succeeded"},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Completed"},
		}
	} else {
		// A failed Job has no completionTime (that requires Complete=True).
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "plan failed"},
		}
	}
	Expect(k8sClient.Status().Update(context.Background(), job)).To(Succeed())
}

func cleanupTestModule(name string) {
	cr := &tofuv1alpha1.TofuModule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	_ = k8sClient.Delete(context.Background(), cr)
}

var _ = Describe("TofuModule Controller", func() {
	Context("plan -> approve -> apply lifecycle", func() {
		It("plans, waits for approval, applies after approval, and re-plans on spec change", func() {
			name := "lifecycle"
			r := newTestReconciler()
			Expect(k8sClient.Create(context.Background(), newTestModule(name, nil))).To(Succeed())
			defer cleanupTestModule(name)

			// First reconcile starts a plan run and provisions scaffolding.
			testReconcile(r, name)
			cr := testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhasePlanning))
			planJobs := listTestJobs(name, actionPlan)
			Expect(planJobs).To(HaveLen(1))

			Expect(k8sClient.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: stateSecretName(name)}, &corev1.Secret{})).To(Succeed())
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: runnerSA}, &corev1.ServiceAccount{})).To(Succeed())

			// Plan succeeds: phase moves to PlanGenerated, awaiting approval.
			finishTestJob(&planJobs[0], true)
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhasePlanGenerated))
			Expect(cr.Status.PlanHash).NotTo(BeEmpty())
			Expect(cr.Status.PlanSecretRef).NotTo(BeNil())

			// Not approved: still PlanGenerated.
			testReconcile(r, name)
			Expect(testGetModule(name).Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhasePlanGenerated))

			// Approve: an apply run starts.
			cr.Spec.ApprovePlan = true
			Expect(k8sClient.Update(context.Background(), cr)).To(Succeed())
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhaseApplying))
			applyJobs := listTestJobs(name, actionApply)
			Expect(applyJobs).To(HaveLen(1))

			// Apply succeeds: phase Applied, approvals are idempotent.
			finishTestJob(&applyJobs[0], true)
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhaseApplied))
			Expect(cr.Status.LastAppliedPlanHash).To(Equal(cr.Status.PlanHash))
			Expect(cr.Status.LastAppliedTime).NotTo(BeNil())

			// Spec change: a new plan run is started for the new spec.
			cr.Spec.Module.Git.Ref = "v1.0.0"
			Expect(k8sClient.Update(context.Background(), cr)).To(Succeed())
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhasePlanning))
			Expect(listTestJobs(name, actionPlan)).To(HaveLen(2)) // previous + new
		})
	})

	Context("failed runs", func() {
		It("records plan failures, keeps the failed Job for logs, and retries after the interval", func() {
			name := "failplan"
			r := newTestReconciler()
			Expect(k8sClient.Create(context.Background(), newTestModule(name, nil))).To(Succeed())
			defer cleanupTestModule(name)

			testReconcile(r, name)
			planJobs := listTestJobs(name, actionPlan)
			Expect(planJobs).To(HaveLen(1))

			finishTestJob(&planJobs[0], false)
			testReconcile(r, name)
			cr := testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhaseFailed))
			cond := apimeta.FindStatusCondition(cr.Status.Conditions, tofuv1alpha1.ConditionPlanGenerated)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Message).To(ContainSubstring("plan failed"))

			// The failed Job is retained (so its pod logs stay available) and
			// marked as recorded.
			kept := listTestJobs(name, actionPlan)
			Expect(kept).To(HaveLen(1))
			Expect(kept[0].Labels[failureRecordedKey]).NotTo(BeEmpty())

			// Before the retry interval has elapsed, no new run is started.
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhaseFailed))
			Expect(listTestJobs(name, actionPlan)).To(HaveLen(1))

			// Once the retry interval has elapsed, a fresh plan run starts.
			stale := kept[0].DeepCopy()
			stale.Labels[failureRecordedKey] = strconv.FormatInt(time.Now().Add(-time.Hour).UnixNano(), 10)
			Expect(k8sClient.Update(context.Background(), stale)).To(Succeed())
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhasePlanning))
			Expect(listTestJobs(name, actionPlan)).To(HaveLen(2)) // failed (kept) + new
		})
	})

	Context("drift detection", func() {
		It("re-plans for drift, invalidates the previous approval, and re-applies after approval", func() {
			name := "drift"
			r := newTestReconciler()
			Expect(k8sClient.Create(context.Background(), newTestModule(name, func(cr *tofuv1alpha1.TofuModule) {
				cr.Spec.Interval = &metav1.Duration{Duration: 5 * time.Minute}
			}))).To(Succeed())
			defer cleanupTestModule(name)

			// Drive the module to Applied first.
			testReconcile(r, name)
			planJobs := listTestJobs(name, actionPlan)
			finishTestJob(&planJobs[0], true)
			testReconcile(r, name)
			cr := testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhasePlanGenerated))
			cr.Spec.ApprovePlan = true
			Expect(k8sClient.Update(context.Background(), cr)).To(Succeed())
			testReconcile(r, name)
			applyJobs := listTestJobs(name, actionApply)
			finishTestJob(&applyJobs[0], true)
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhaseApplied))

			// Backdate the last apply beyond the interval and revoke approval.
			past := metav1.NewTime(time.Now().Add(-30 * time.Minute))
			cr.Status.LastAppliedTime = &past
			Expect(k8sClient.Status().Update(context.Background(), cr)).To(Succeed())
			cr.Spec.ApprovePlan = false
			Expect(k8sClient.Update(context.Background(), cr)).To(Succeed())

			// Drift is due: a plan run with the drift reason is started.
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhasePlanning))
			driftPlans := listTestJobs(name, actionPlan)
			Expect(driftPlans).To(HaveLen(2)) // original plan Job + drift re-plan
			driftJob := &driftPlans[len(driftPlans)-1]
			Expect(driftJob.Labels[reasonLabelKey]).To(Equal(reasonDrift))

			// The drift plan succeeds: the previous approval is invalidated.
			finishTestJob(driftJob, true)
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.LastAppliedPlanHash).To(BeEmpty())
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhasePlanGenerated))

			// Approving the drift plan applies it.
			cr.Spec.ApprovePlan = true
			Expect(k8sClient.Update(context.Background(), cr)).To(Succeed())
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhaseApplying))
			applyJobs = listTestJobs(name, actionApply)
			Expect(applyJobs).To(HaveLen(2)) // original apply Job + drift apply
			applyJob := &applyJobs[len(applyJobs)-1]

			finishTestJob(applyJob, true)
			testReconcile(r, name)
			cr = testGetModule(name)
			Expect(cr.Status.Phase).To(Equal(tofuv1alpha1.TofuModulePhaseApplied))
			Expect(cr.Status.LastAppliedPlanHash).To(Equal(cr.Status.PlanHash))
		})
	})
})
