package validating

import (
	"context"
	"fmt"

	appsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
	clonesetcore "github.com/openkruise/kruise/pkg/controller/cloneset/core"
	"github.com/openkruise/kruise/pkg/util"
	"github.com/openkruise/kruise/pkg/webhook/util/convertor"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	unversionedvalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/types"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	appsvalidation "k8s.io/kubernetes/pkg/apis/apps/validation"
	apivalidation "k8s.io/kubernetes/pkg/apis/core/validation"
)

func (h *CloneSetCreateUpdateHandler) validateCloneSet(cloneSet, oldCloneSet *appsv1alpha1.CloneSet) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&cloneSet.ObjectMeta, true, apimachineryvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	var oldCloneSetSpec *appsv1alpha1.CloneSetSpec
	if oldCloneSet != nil {
		oldCloneSetSpec = &oldCloneSet.Spec
	}
	allErrs = append(allErrs, h.validateCloneSetSpec(&cloneSet.Spec, oldCloneSetSpec, &cloneSet.ObjectMeta, field.NewPath("spec"))...)
	return allErrs
}

func (h *CloneSetCreateUpdateHandler) validateCloneSetSpec(spec, oldSpec *appsv1alpha1.CloneSetSpec, metadata *metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*spec.Replicas), fldPath.Child("replicas"))...)
	if spec.Selector == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("selector"), ""))
	} else {
		allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(spec.Selector, fldPath.Child("selector"))...)
		if len(spec.Selector.MatchLabels)+len(spec.Selector.MatchExpressions) == 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, "empty selector is invalid for cloneset"))
		}
	}

	selector, err := metav1.LabelSelectorAsSelector(spec.Selector)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, ""))
	} else {
		coreTemplate, err := convertor.ConvertPodTemplateSpec(&spec.Template)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Root(), spec.Template, fmt.Sprintf("Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec failed: %v", err)))
			return allErrs
		}
		allErrs = append(allErrs, appsvalidation.ValidatePodTemplateSpecForStatefulSet(coreTemplate, selector, fldPath.Child("template"))...)
	}

	if spec.Template.Spec.RestartPolicy != "" && spec.Template.Spec.RestartPolicy != v1.RestartPolicyAlways {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("template", "spec", "restartPolicy"), spec.Template.Spec.RestartPolicy, []string{string(v1.RestartPolicyAlways)}))
	}
	if spec.Template.Spec.ActiveDeadlineSeconds != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("template", "spec", "activeDeadlineSeconds"), "activeDeadlineSeconds in cloneset is not Supported"))
	}

	var oldScaleStrategy *appsv1alpha1.CloneSetScaleStrategy
	if oldSpec != nil {
		oldScaleStrategy = &oldSpec.ScaleStrategy
	}

	allErrs = append(allErrs, h.validateScaleStrategy(&spec.ScaleStrategy, oldScaleStrategy, metadata, fldPath.Child("scaleStrategy"))...)
	allErrs = append(allErrs, h.validateUpdateStrategy(&spec.UpdateStrategy, int(*spec.Replicas), fldPath.Child("updateStrategy"))...)

	return allErrs
}

func (h *CloneSetCreateUpdateHandler) validateScaleStrategy(strategy, oldStrategy *appsv1alpha1.CloneSetScaleStrategy, metadata *metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if list := util.CheckDuplicate(strategy.PodsToDelete); len(list) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("podsToDelete"), strategy.PodsToDelete, fmt.Sprintf("duplicated items %v", list)))
		return allErrs
	}

	podsToDeleteSet := sets.NewString(strategy.PodsToDelete...)

	if oldStrategy != nil && len(oldStrategy.PodsToDelete) > 0 {
		podsToDeleteSet.Delete(oldStrategy.PodsToDelete...)
	}

	for _, podName := range podsToDeleteSet.List() {
		pod := &v1.Pod{}
		if err := h.Client.Get(context.TODO(), types.NamespacedName{Namespace: metadata.Namespace, Name: podName}, pod); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("podsToDelete"), podName, fmt.Sprintf("find pod %s failed: %v", podName, err)))
		} else if pod.DeletionTimestamp != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("podsToDelete"), podName, fmt.Sprintf("find pod %s already terminating", podName)))
		} else if owner := metav1.GetControllerOf(pod); owner.UID != metadata.UID {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("podsToDelete"), podName, fmt.Sprintf("find pod %s owner is not this CloneSet", podName)))
		}
	}

	return allErrs
}

func (h *CloneSetCreateUpdateHandler) validateUpdateStrategy(strategy *appsv1alpha1.CloneSetUpdateStrategy, replicas int, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	var err error

	switch strategy.Type {
	case appsv1alpha1.RecreateCloneSetUpdateStrategyType,
		appsv1alpha1.InPlaceIfPossibleCloneSetUpdateStrategyType,
		appsv1alpha1.InPlaceOnlyCloneSetUpdateStrategyType:
	default:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("type"), strategy.Type, fmt.Sprintf("must be '%s', %s or '%s'",
			appsv1alpha1.RecreateCloneSetUpdateStrategyType,
			appsv1alpha1.InPlaceIfPossibleCloneSetUpdateStrategyType,
			appsv1alpha1.InPlaceOnlyCloneSetUpdateStrategyType)))
	}

	partition, err := intstrutil.GetValueFromIntOrPercent(strategy.Partition, replicas, true)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("partition"), strategy.Partition.String(),
			fmt.Sprintf("failed getValueFromIntOrPercent for partition: %v", err)))
	}
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(partition), fldPath.Child("partition"))...)

	if err := strategy.PriorityStrategy.FieldsValidation(); err != nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("priorityStrategy"), err.Error()))
	}

	if err := strategy.ScatterStrategy.FieldsValidation(); err != nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("scatterStrategy"), err.Error()))
	}

	var maxUnavailable int
	if strategy.MaxUnavailable != nil {
		maxUnavailable, err = intstrutil.GetValueFromIntOrPercent(strategy.MaxUnavailable, replicas, true)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("maxUnavailable"), strategy.MaxUnavailable.String(),
				fmt.Sprintf("failed getValueFromIntOrPercent for maxUnavailable: %v", err)))
		}
	}

	var maxSurge int
	if strategy.MaxSurge != nil {
		maxSurge, err = intstrutil.GetValueFromIntOrPercent(strategy.MaxSurge, replicas, true)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("maxSurge"), strategy.MaxSurge.String(),
				fmt.Sprintf("failed getValueFromIntOrPercent for maxSurge: %v", err)))
		}
		if strategy.Type == appsv1alpha1.InPlaceOnlyCloneSetUpdateStrategyType && maxSurge > 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("maxSurge"), strategy.MaxSurge.String(),
				fmt.Sprintf("can not use maxSurge with strategy type InPlaceOnly")))
		}
	}

	if replicas > 0 && maxUnavailable < 1 && maxSurge < 1 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("maxUnavailable"), strategy.MaxUnavailable,
			"maxUnavailable and maxSurge should not both be less than 1"))
	}

	return allErrs
}

func (h *CloneSetCreateUpdateHandler) validateCloneSetUpdate(cloneSet, oldCloneSet *appsv1alpha1.CloneSet) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&cloneSet.ObjectMeta, &oldCloneSet.ObjectMeta, field.NewPath("metadata"))

	clone := cloneSet.DeepCopy()
	clone.Spec.Replicas = oldCloneSet.Spec.Replicas
	clone.Spec.Template = oldCloneSet.Spec.Template
	clone.Spec.ScaleStrategy = oldCloneSet.Spec.ScaleStrategy
	clone.Spec.UpdateStrategy = oldCloneSet.Spec.UpdateStrategy
	clone.Spec.MinReadySeconds = oldCloneSet.Spec.MinReadySeconds
	clone.Spec.Lifecycle = oldCloneSet.Spec.Lifecycle
	clone.Spec.RevisionHistoryLimit = oldCloneSet.Spec.RevisionHistoryLimit
	if !apiequality.Semantic.DeepEqual(clone.Spec, oldCloneSet.Spec) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec"), "updates to cloneset spec for fields other than 'replicas', 'template', 'lifecycle', 'scaleStrategy', 'updateStrategy', 'minReadySeconds' and 'revisionHistoryLimit' are forbidden"))
	}

	coreControl := clonesetcore.New(cloneSet)
	if err := coreControl.ValidateCloneSetUpdate(oldCloneSet, cloneSet); err != nil {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec"), err.Error()))
	}

	allErrs = append(allErrs, h.validateCloneSet(cloneSet, oldCloneSet)...)
	return allErrs
}
