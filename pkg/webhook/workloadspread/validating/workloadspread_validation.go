/*
Copyright 2021 The Kruise Authors.

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

package validating

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/apis/core"
	corev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	webhookutil "github.com/openkruise/kruise/pkg/webhook/util"
	"github.com/openkruise/kruise/pkg/webhook/util/convertor"

	appsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
	appsvbeta1 "github.com/openkruise/kruise/apis/apps/v1beta1"
	"github.com/openkruise/kruise/pkg/util/configuration"
)

const (
	MaxScheduledFailedDuration = 300 * time.Second
)

var (
	controllerKruiseKindCS       = appsv1alpha1.SchemeGroupVersion.WithKind("CloneSet")
	controllerKindSts            = appsv1.SchemeGroupVersion.WithKind("StatefulSet")
	controllerKindRS             = appsv1.SchemeGroupVersion.WithKind("ReplicaSet")
	controllerKindDep            = appsv1.SchemeGroupVersion.WithKind("Deployment")
	controllerKindJob            = batchv1.SchemeGroupVersion.WithKind("Job")
	controllerKruiseKindBetaSts  = appsvbeta1.SchemeGroupVersion.WithKind("StatefulSet")
	controllerKruiseKindAlphaSts = appsv1alpha1.SchemeGroupVersion.WithKind("StatefulSet")
)

func verifyGroupKind(ref *appsv1alpha1.TargetReference, expectedKind string, expectedGroups []string) (bool, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		klog.ErrorS(err, "failed to parse GroupVersion for apiVersion", "apiVersion", ref.APIVersion)
		return false, err
	}

	if ref.Kind != expectedKind {
		return false, nil
	}

	for _, group := range expectedGroups {
		if group == gv.Group {
			return true, nil
		}
	}

	return false, nil
}

func (h *WorkloadSpreadCreateUpdateHandler) validatingWorkloadSpreadFn(obj *appsv1alpha1.WorkloadSpread) field.ErrorList {
	// validate ws.spec.
	allErrs := validateWorkloadSpreadSpec(h, obj, field.NewPath("spec"))

	// validate whether ws.spec.targetRef is in conflict with others.
	wsList := &appsv1alpha1.WorkloadSpreadList{}
	if err := h.Client.List(context.TODO(), wsList, &client.ListOptions{Namespace: obj.Namespace}); err != nil {
		allErrs = append(allErrs, field.InternalError(field.NewPath(""), fmt.Errorf("query other WorkloadSpread failed, err: %v", err)))
	} else {
		allErrs = append(allErrs, validateWorkloadSpreadConflict(obj, wsList.Items, field.NewPath("spec"))...)
	}

	return allErrs
}

func validateWorkloadSpreadSpec(h *WorkloadSpreadCreateUpdateHandler, obj *appsv1alpha1.WorkloadSpread, fldPath *field.Path) field.ErrorList {
	spec := &obj.Spec
	allErrs := field.ErrorList{}
	var workloadTemplate client.Object

	// validate targetRef
	if spec.TargetReference == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("targetRef"), "no targetRef defined in WorkloadSpread"))
	} else {
		if spec.TargetReference.APIVersion == "" || spec.TargetReference.Name == "" || spec.TargetReference.Kind == "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("targetRef"), spec.TargetReference, "empty TargetReference is not valid for WorkloadSpread."))
		} else {
			switch spec.TargetReference.Kind {
			case controllerKruiseKindCS.Kind:
				ok, err := verifyGroupKind(spec.TargetReference, controllerKruiseKindCS.Kind, []string{controllerKruiseKindCS.Group})
				if !ok || err != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("targetRef"), spec.TargetReference, "TargetReference is not valid for CloneSet."))
				} else {
					set := &appsv1alpha1.CloneSet{}
					if getErr := h.Client.Get(context.TODO(), client.ObjectKey{Name: spec.TargetReference.Name, Namespace: obj.Namespace}, set); getErr == nil {
						workloadTemplate = set
					}
				}
			case controllerKindDep.Kind:
				ok, err := verifyGroupKind(spec.TargetReference, controllerKindDep.Kind, []string{controllerKindDep.Group})
				if !ok || err != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("targetRef"), spec.TargetReference, "TargetReference is not valid for Deployment."))
				} else {
					set := &appsv1.Deployment{}
					if getErr := h.Client.Get(context.TODO(), client.ObjectKey{Name: spec.TargetReference.Name, Namespace: obj.Namespace}, set); getErr == nil {
						workloadTemplate = set
					}
				}
			case controllerKindRS.Kind:
				ok, err := verifyGroupKind(spec.TargetReference, controllerKindRS.Kind, []string{controllerKindRS.Group})
				if !ok || err != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("targetRef"), spec.TargetReference, "TargetReference is not valid for ReplicaSet."))
				} else {
					set := &appsv1.ReplicaSet{}
					if getErr := h.Client.Get(context.TODO(), client.ObjectKey{Name: spec.TargetReference.Name, Namespace: obj.Namespace}, set); getErr == nil {
						workloadTemplate = set
					}
				}
			case controllerKindJob.Kind:
				ok, err := verifyGroupKind(spec.TargetReference, controllerKindJob.Kind, []string{controllerKindJob.Group})
				if !ok || err != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("targetRef"), spec.TargetReference, "TargetReference is not valid for Job."))
				} else {
					set := &batchv1.Job{}
					if getErr := h.Client.Get(context.TODO(), client.ObjectKey{Name: spec.TargetReference.Name, Namespace: obj.Namespace}, set); getErr == nil {
						workloadTemplate = set
					}
				}
			case controllerKindSts.Kind:
				ok, err := verifyGroupKind(spec.TargetReference, controllerKindSts.Kind, []string{controllerKindSts.Group, controllerKruiseKindAlphaSts.Group, controllerKruiseKindBetaSts.Group})
				if !ok || err != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("targetRef"), spec.TargetReference, "TargetReference is not valid for StatefulSet."))
				} else {
					set := &appsv1.StatefulSet{}
					if getErr := h.Client.Get(context.TODO(), client.ObjectKey{Name: spec.TargetReference.Name, Namespace: obj.Namespace}, set); getErr == nil {
						workloadTemplate = set
					}
				}
			default:
				whiteList, err := configuration.GetWSWatchCustomWorkloadWhiteList(h.Client)
				if err != nil {
					allErrs = append(allErrs, field.InternalError(fldPath.Child("targetRef"), err))
					break
				}
				matched := false
				for _, wl := range whiteList.Workloads {
					if ok, _ := verifyGroupKind(spec.TargetReference, wl.Kind, []string{wl.Group}); ok {
						matched = true
						break
					}
				}
				if !matched {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("targetRef"), spec.TargetReference, "TargetReference's GroupKind is not permitted."))
				}
			}
		}
	}

	// validate subsets
	allErrs = append(allErrs, validateWorkloadSpreadSubsets(obj, spec.Subsets, workloadTemplate, fldPath.Child("subsets"))...)

	// validate scheduleStrategy
	if spec.ScheduleStrategy.Type != "" &&
		spec.ScheduleStrategy.Type != appsv1alpha1.FixedWorkloadSpreadScheduleStrategyType &&
		spec.ScheduleStrategy.Type != appsv1alpha1.AdaptiveWorkloadSpreadScheduleStrategyType {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("scheduleStrategy").Child("type"),
			spec.ScheduleStrategy.Type, "ScheduleStrategy's type is not valid"))
	}

	if spec.ScheduleStrategy.Adaptive != nil {
		if spec.ScheduleStrategy.Type != appsv1alpha1.AdaptiveWorkloadSpreadScheduleStrategyType {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("scheduleStrategy").Child("type"),
				spec.ScheduleStrategy.Adaptive.RescheduleCriticalSeconds, "the scheduleStrategy's type must be adaptive when using adaptive scheduleStrategy"))
		}

		if len(spec.Subsets) > 1 && spec.Subsets[len(spec.Subsets)-1].MaxReplicas != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("scheduleStrategy").Child("adaptive"),
				spec.ScheduleStrategy.Adaptive.RescheduleCriticalSeconds, "the last subset's maxReplicas must be not specified when using adaptive scheduleStrategy"))
		}

		allowedMaxSeconds := int32(math.MaxInt32)
		if len(spec.Subsets) > 1 {
			// This constraint is to avoid the scene where a pod is re-scheduled among unschedulable subsets over and over again.
			// MaxScheduledFailedDurationSeconds is the maximum safe value in theory.
			// Deducting 5 is out of the consideration of reconcile cost, etc.
			allowedMaxSeconds = int32(MaxScheduledFailedDuration.Seconds()-5) / int32(len(spec.Subsets)-1)
		}
		if spec.ScheduleStrategy.Adaptive.RescheduleCriticalSeconds != nil &&
			(*spec.ScheduleStrategy.Adaptive.RescheduleCriticalSeconds < 0 || *spec.ScheduleStrategy.Adaptive.RescheduleCriticalSeconds > allowedMaxSeconds) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("scheduleStrategy").Child("adaptive").Child("rescheduleCriticalSeconds"),
				spec.ScheduleStrategy.Adaptive.RescheduleCriticalSeconds, fmt.Sprintf("rescheduleCriticalSeconds < 0 or rescheduleCriticalSeconds > %d is not permitted", allowedMaxSeconds)))
		}
	}

	// validate targetFilter
	if spec.TargetFilter != nil {
		if _, err := metav1.LabelSelectorAsSelector(spec.TargetFilter.Selector); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("targetFilter"), spec.TargetFilter, err.Error()))
		}
	}

	return allErrs
}

func validateWorkloadSpreadSubsets(ws *appsv1alpha1.WorkloadSpread, subsets []appsv1alpha1.WorkloadSpreadSubset, workloadTemplate client.Object, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	//if len(subsets) < 2 {
	//	allErrs = append(allErrs, field.Required(fldPath, "subsets number must >= 2 in WorkloadSpread"))
	//	return allErrs
	//}

	if len(subsets) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, "subsets number must >= 1 in WorkloadSpread"))
		return allErrs
	}

	subSetNames := sets.String{}
	maxReplicasSum := 0
	var firstMaxReplicasType *intstr.Type

	for i, subset := range subsets {
		subsetName := subset.Name
		if subsetName == "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("name"), subsetName, ""))
		} else {
			if subSetNames.Has(subsetName) {
				// Name should be unique between all of the subsets under one WorkloadSpread.
				allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("name"), subsetName, fmt.Sprintf("duplicated subset name %s", subsetName)))
			}
			subSetNames.Insert(subsetName)
		}

		// at least one of requiredNodeSelectorTerm, preferredNodeSelectorTerms, tolerations.
		//if subset.RequiredNodeSelectorTerm == nil && subset.PreferredNodeSelectorTerms == nil && subset.Tolerations == nil {
		//	allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("requiredNodeSelectorTerm"), subset.RequiredNodeSelectorTerm, "The requiredNodeSelectorTerm, preferredNodeSelectorTerms and tolerations are empty that is not valid for WorkloadSpread"))
		//} else {
		if subset.RequiredNodeSelectorTerm != nil {
			coreNodeSelectorTerm := &core.NodeSelectorTerm{}
			if err := corev1.Convert_v1_NodeSelectorTerm_To_core_NodeSelectorTerm(subset.RequiredNodeSelectorTerm.DeepCopy(), coreNodeSelectorTerm, nil); err != nil {
				allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("requiredNodeSelectorTerm"), subset.RequiredNodeSelectorTerm, fmt.Sprintf("Convert_v1_NodeSelectorTerm_To_core_NodeSelectorTerm failed: %v", err)))
			} else {
				allErrs = append(allErrs, corevalidation.ValidateNodeSelectorTerm(*coreNodeSelectorTerm, false, fldPath.Index(i).Child("requiredNodeSelectorTerm"))...)
			}
		}

		if subset.PreferredNodeSelectorTerms != nil {
			corePreferredSchedulingTerms := make([]core.PreferredSchedulingTerm, 0, len(subset.PreferredNodeSelectorTerms))
			for i, term := range subset.PreferredNodeSelectorTerms {
				corePreferredSchedulingTerm := &core.PreferredSchedulingTerm{}
				if err := corev1.Convert_v1_PreferredSchedulingTerm_To_core_PreferredSchedulingTerm(term.DeepCopy(), corePreferredSchedulingTerm, nil); err != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("preferredSchedulingTerms"), subset.PreferredNodeSelectorTerms, fmt.Sprintf("Convert_v1_PreferredSchedulingTerm_To_core_PreferredSchedulingTerm failed: %v", err)))
				} else {
					corePreferredSchedulingTerms = append(corePreferredSchedulingTerms, *corePreferredSchedulingTerm)
				}
			}

			allErrs = append(allErrs, corevalidation.ValidatePreferredSchedulingTerms(corePreferredSchedulingTerms, fldPath.Index(i).Child("preferredSchedulingTerms"))...)
		}
		//}

		if subset.Tolerations != nil {
			var coreTolerations []core.Toleration
			for i, toleration := range subset.Tolerations {
				coreToleration := &core.Toleration{}
				if err := corev1.Convert_v1_Toleration_To_core_Toleration(&toleration, coreToleration, nil); err != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("tolerations"), subset.Tolerations, fmt.Sprintf("Convert_v1_Toleration_To_core_Toleration failed: %v", err)))
				} else {
					coreTolerations = append(coreTolerations, *coreToleration)
				}
			}
			allErrs = append(allErrs, corevalidation.ValidateTolerations(coreTolerations, fldPath.Index(i).Child("tolerations"))...)
		}

		if subset.Patch.Raw != nil {
			// In the case the WorkloadSpread is created before the workload,so no workloadTemplate is obtained, skip the remaining checks.
			if workloadTemplate != nil {
				// get the PodTemplateSpec from the workload
				var podSpec v1.PodTemplateSpec
				switch workloadTemplate.GetObjectKind().GroupVersionKind() {
				case controllerKruiseKindCS:
					cs := workloadTemplate.(*appsv1alpha1.CloneSet)
					podSpec = withVolumeClaimTemplates(cs.Spec.Template, cs.Spec.VolumeClaimTemplates)
				case controllerKindDep:
					podSpec = workloadTemplate.(*appsv1.Deployment).Spec.Template
				case controllerKindRS:
					podSpec = workloadTemplate.(*appsv1.ReplicaSet).Spec.Template
				case controllerKindJob:
					podSpec = workloadTemplate.(*batchv1.Job).Spec.Template
				case controllerKindSts:
					sts := workloadTemplate.(*appsv1.StatefulSet)
					podSpec = withVolumeClaimTemplates(sts.Spec.Template, sts.Spec.VolumeClaimTemplates)
				}
				podBytes, _ := json.Marshal(podSpec)
				modified, err := strategicpatch.StrategicMergePatch(podBytes, subset.Patch.Raw, &v1.Pod{})
				if err != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("patch"), string(subset.Patch.Raw), fmt.Sprintf("failed to merge patch: %v", err)))
				}
				newPod := &v1.Pod{}
				if err = json.Unmarshal(modified, newPod); err != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("patch"), string(subset.Patch.Raw), fmt.Sprintf("failed to unmarshal: %v", err)))
				}
				coreNewPod, CovErr := convertor.ConvertPod(newPod)
				if CovErr != nil {
					allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("patch"), newPod, fmt.Sprintf("Convert_v1_Pod_To_core_Pod failed: %v", err)))
				}
				allErrs = append(allErrs, corevalidation.ValidatePodSpec(&coreNewPod.Spec, &coreNewPod.ObjectMeta, fldPath.Index(i).Child("patch"), webhookutil.DefaultPodValidationOptions)...)
			}
		}

		//1. All subset maxReplicas must be the same type: int or percent.
		//2. Adaptive: the last subset must be not specified.
		//3. If all maxReplicas is specified as percent, the total maxReplicas must equal 1, except the last subset is not specified.
		if subset.MaxReplicas != nil {
			if firstMaxReplicasType == nil {
				firstMaxReplicasType = &subset.MaxReplicas.Type
			} else if subset.MaxReplicas.Type != *firstMaxReplicasType {
				allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("maxReplicas"), subset.MaxReplicas, "the maxReplicas type of all subsets must be the same"))
				return allErrs
			}

			if ws.Spec.TargetReference != nil && ws.Spec.TargetReference.Kind == controllerKindSts.Kind && subset.MaxReplicas.Type != intstr.Int {
				allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("maxReplicas"), subset.MaxReplicas, "the maxReplicas type must be Int for StatefulSet"))
				return allErrs
			}

			subsetMaxReplicas, err := intstr.GetValueFromIntOrPercent(subset.MaxReplicas, 100, true)
			if err != nil || subsetMaxReplicas < 0 {
				allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("maxReplicas"), subset.MaxReplicas, "maxReplicas is not valid for subset"))
				return allErrs
			}

			if subset.MaxReplicas.Type == intstr.String {
				maxReplicasSum += subsetMaxReplicas
				if maxReplicasSum > 100 {
					allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("maxReplicas"), subset.MaxReplicas, "the sum of all subset's maxReplicas exceeds 100% is no permitted"))
					return allErrs
				}
			}
		}
	}

	if firstMaxReplicasType != nil && *firstMaxReplicasType == intstr.String && maxReplicasSum < 100 && subsets[len(subsets)-1].MaxReplicas != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Index(0).Child("maxReplicas"), subsets[0].MaxReplicas, "maxReplicas sum of all subsets must equal 100% when type is specified as percent"))
	}
	return allErrs
}

func withVolumeClaimTemplates(pod v1.PodTemplateSpec, claims []v1.PersistentVolumeClaim) v1.PodTemplateSpec {
	for _, pvc := range claims {
		pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
			Name: pvc.Name,
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc.Name,
				},
			},
		})
	}
	return pod
}

func validateWorkloadSpreadConflict(ws *appsv1alpha1.WorkloadSpread, others []appsv1alpha1.WorkloadSpread, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, other := range others {
		if other.Name == ws.Name {
			continue
		}
		// TargetReference cannot be managed by multiple ws
		if ws.Spec.TargetReference != nil && other.Spec.TargetReference != nil {
			targetRef1 := ws.Spec.TargetReference
			targetRef2 := other.Spec.TargetReference

			gv1, _ := schema.ParseGroupVersion(targetRef1.APIVersion)
			gv2, _ := schema.ParseGroupVersion(targetRef2.APIVersion)

			if gv1.Group == gv2.Group && targetRef1.Kind == targetRef2.Kind && targetRef1.Name == targetRef2.Name {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("targetRef"), ws.Spec.TargetReference, fmt.Sprintf(
					"ws.spec.targetRef is in conflict with other WorkloadSpread %s", other.Name)))
				return allErrs
			}
		}
	}
	return allErrs
}

func validateWorkloadSpreadUpdate(new, old *appsv1alpha1.WorkloadSpread) field.ErrorList {
	// validate metadata
	allErrs := corevalidation.ValidateObjectMetaUpdate(&new.ObjectMeta, &old.ObjectMeta, field.NewPath("metadata"))
	// validate targetRef
	allErrs = append(allErrs, validateWorkloadSpreadTargetRefUpdate(new.Spec.TargetReference, old.Spec.TargetReference, field.NewPath("spec"))...)
	return allErrs
}

func validateWorkloadSpreadTargetRefUpdate(targetRef, oldTargetRef *appsv1alpha1.TargetReference, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if targetRef != nil && oldTargetRef != nil {
		gv1, _ := schema.ParseGroupVersion(targetRef.APIVersion)
		gv2, _ := schema.ParseGroupVersion(oldTargetRef.APIVersion)
		if gv1.Group != gv2.Group || targetRef.Kind != oldTargetRef.Kind || targetRef.Name != oldTargetRef.Name {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("targetRef"), targetRef, "change TargetReference is not permitted for WorkloadSpread"))
		}
	}
	return allErrs
}
