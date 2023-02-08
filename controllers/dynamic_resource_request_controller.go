/*
Copyright 2021 Syntasso.

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

package controllers

import (
	"context"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/uuid"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterutil "sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	workFinalizer     = finalizerPrefix + "work-cleanup"
	pipelineFinalizer = finalizerPrefix + "pipeline-cleanup"
)

var rrFinalizers = []string{workFinalizer, pipelineFinalizer}

type dynamicResourceRequestController struct {
	//use same naming conventions as other controllers
	Client                 client.Client
	gvk                    *schema.GroupVersionKind
	scheme                 *runtime.Scheme
	promiseIdentifier      string
	promiseClusterSelector labels.Set
	xaasRequestPipeline    []string
	log                    logr.Logger
	finalizers             []string
	uid                    string
	enabled                *bool
}

//+kubebuilder:rbac:groups="",resources=pods,verbs=create;list;watch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create

func (r *dynamicResourceRequestController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if !*r.enabled {
		//temporary fix until https://github.com/kubernetes-sigs/controller-runtime/issues/1884 is resolved
		//once resolved, this won't be necessary since the dynamic controller will be deleted
		return ctrl.Result{}, nil
	}

	logger := r.log.WithValues("uid", r.uid, r.promiseIdentifier, req.NamespacedName)
	resourceRequestIdentifier := fmt.Sprintf("%s-%s-%s", r.promiseIdentifier, req.Namespace, req.Name)

	unstructuredCRD := &unstructured.Unstructured{}
	unstructuredCRD.SetGroupVersionKind(*r.gvk)

	err := r.Client.Get(ctx, req.NamespacedName, unstructuredCRD)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed getting Promise CRD")
		return defaultRequeue, nil
	}

	if !unstructuredCRD.GetDeletionTimestamp().IsZero() {
		return r.deleteResources(ctx, unstructuredCRD, resourceRequestIdentifier, logger)
	}

	// Reconcile necessary finalizers
	if finalizersAreMissing(unstructuredCRD, []string{workFinalizer, pipelineFinalizer}) {
		return addFinalizers(ctx, r.Client, unstructuredCRD, []string{workFinalizer, pipelineFinalizer}, logger)
	}

	if r.pipelineHasExecuted(resourceRequestIdentifier) {
		logger.Info("Cannot execute update on pre-existing pipeline for Promise resource request " + resourceRequestIdentifier)
		return ctrl.Result{}, nil
	}

	workCreatorCommand := fmt.Sprintf("./work-creator -identifier %s -input-directory /work-creator-files", resourceRequestIdentifier)

	resourceKindNameNamespace := fmt.Sprintf("%s.%s %s --namespace %s", strings.ToLower(r.gvk.Kind), r.gvk.Group, req.Name, req.Namespace)
	resourceRequestCommand := fmt.Sprintf("kubectl get %s -oyaml > /output/object.yaml", resourceKindNameNamespace)

	err = r.setPipelineCondition(ctx, unstructuredCRD, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "request-pipeline-" + r.promiseIdentifier + "-" + getShortUuid(),
			Namespace: "default",
			Labels: map[string]string{
				"kratix-promise-id":                  r.promiseIdentifier,
				"kratix-promise-resource-request-id": resourceRequestIdentifier,
			},
		},
		Spec: v1.PodSpec{
			RestartPolicy:      v1.RestartPolicyOnFailure,
			ServiceAccountName: r.promiseIdentifier + "-promise-pipeline",
			Containers: []v1.Container{
				{
					Name:    "status-writer",
					Image:   os.Getenv("WC_IMG"),
					Command: []string{"sh", "-c", "update-status"},
					Env: []v1.EnvVar{
						{
							Name:  "RR_KIND",
							Value: fmt.Sprintf("%s.%s", strings.ToLower(r.gvk.Kind), r.gvk.Group),
						},
						{
							Name:  "RR_NAME",
							Value: req.Name,
						},
						{
							Name:  "RR_NAMESPACE",
							Value: req.Namespace,
						},
					},
					VolumeMounts: []v1.VolumeMount{
						{
							MountPath: "/work-creator-files/metadata",
							Name:      "metadata",
						},
					},
				},
			},
			InitContainers: []v1.Container{
				{
					Name:    "reader",
					Image:   "bitnami/kubectl:1.20.10",
					Command: []string{"sh", "-c", resourceRequestCommand},
					VolumeMounts: []v1.VolumeMount{
						{
							MountPath: "/output",
							Name:      "input",
						},
					},
				},
				{
					Name:  "xaas-request-pipeline-stage-1",
					Image: r.xaasRequestPipeline[0],
					//Command: Supplied by the image author via ENTRYPOINT/CMD
					VolumeMounts: []v1.VolumeMount{
						{
							MountPath: "/input",
							Name:      "input",
						},
						{
							MountPath: "/output",
							Name:      "output",
						},
						{
							MountPath: "/metadata",
							Name:      "metadata",
						},
					},
				},
				{
					Name:    "work-writer",
					Image:   os.Getenv("WC_IMG"),
					Command: []string{"sh", "-c", workCreatorCommand},
					VolumeMounts: []v1.VolumeMount{
						{
							MountPath: "/work-creator-files/input",
							Name:      "output",
						},
						{
							MountPath: "/work-creator-files/metadata",
							Name:      "metadata",
						},
						{
							MountPath: "/work-creator-files/kratix-system",
							Name:      "promise-cluster-selectors",
						},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "input",
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "output",
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "metadata",
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "promise-cluster-selectors",
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{
							LocalObjectReference: v1.LocalObjectReference{
								Name: "cluster-selectors-" + r.promiseIdentifier,
							},
							Items: []v1.KeyToPath{
								{
									Key:  "selectors",
									Path: "promise-cluster-selectors",
								},
							},
						},
					},
				},
			},
		},
	}

	logger.Info("Creating Pipeline for Promise resource request: " + resourceRequestIdentifier + ". The pipeline will now execute...")
	err = r.Client.Create(ctx, &pod)
	if err != nil {
		logger.Error(err, "Error creating Pod")
		y, _ := yaml.Marshal(&pod)
		logger.Error(err, string(y))
	}

	return ctrl.Result{}, nil
}

func (r *dynamicResourceRequestController) pipelineHasExecuted(resourceRequestIdentifier string) bool {
	isPromise, _ := labels.NewRequirement("kratix-promise-resource-request-id", selection.Equals, []string{resourceRequestIdentifier})
	selector := labels.NewSelector().
		Add(*isPromise)

	listOps := &client.ListOptions{
		Namespace:     "default",
		LabelSelector: selector,
	}

	ol := &v1.PodList{}
	err := r.Client.List(context.Background(), ol, listOps)
	if err != nil {
		fmt.Println(err.Error())
		return false
	}
	return len(ol.Items) > 0
}

func (r *dynamicResourceRequestController) deleteResources(ctx context.Context, resourceRequest *unstructured.Unstructured, resourceRequestIdentifier string, logger logr.Logger) (ctrl.Result, error) {
	if finalizersAreDeleted(resourceRequest, rrFinalizers) {
		return ctrl.Result{}, nil
	}

	if controllerutil.ContainsFinalizer(resourceRequest, workFinalizer) {
		err := r.deleteWork(ctx, resourceRequest, resourceRequestIdentifier, workFinalizer, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	if controllerutil.ContainsFinalizer(resourceRequest, pipelineFinalizer) {
		err := r.deletePipeline(ctx, resourceRequest, resourceRequestIdentifier, pipelineFinalizer, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	return fastRequeue, nil
}

func (r *dynamicResourceRequestController) deleteWork(ctx context.Context, resourceRequest *unstructured.Unstructured, workName string, finalizer string, logger logr.Logger) error {
	work := &v1alpha1.Work{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      workName,
	}, work)
	if err != nil {
		if errors.IsNotFound(err) {
			// only remove finalizer at this point because deletion success is guaranteed
			controllerutil.RemoveFinalizer(resourceRequest, finalizer)
			if err := r.Client.Update(ctx, resourceRequest); err != nil {
				return err
			}
			return nil
		}

		logger.Error(err, "Error locating Work, will try again in 5 seconds", "workName", workName)
		return err
	}

	err = r.Client.Delete(ctx, work)
	if err != nil {
		if errors.IsNotFound(err) {
			// only remove finalizer at this point because deletion success is guaranteed
			controllerutil.RemoveFinalizer(resourceRequest, finalizer)
			if err := r.Client.Update(ctx, resourceRequest); err != nil {
				return err
			}
			return nil
		}

		logger.Error(err, "Error deleting Work %s, will try again in 5 seconds", "workName", workName)
		return err
	}

	return nil
}

func (r *dynamicResourceRequestController) deletePipeline(ctx context.Context, resourceRequest *unstructured.Unstructured, resourceRequestIdentifier, finalizer string, logger logr.Logger) error {
	podGVK := schema.GroupVersionKind{
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "Pod",
	}

	podLabels := map[string]string{
		"kratix-promise-id":                  r.promiseIdentifier,
		"kratix-promise-resource-request-id": resourceRequestIdentifier,
	}

	resourcesRemaining, err := deleteAllResourcesWithKindMatchingLabel(ctx, r.Client, podGVK, podLabels, logger)
	if err != nil {
		return err
	}

	if !resourcesRemaining {
		controllerutil.RemoveFinalizer(resourceRequest, finalizer)
		if err := r.Client.Update(ctx, resourceRequest); err != nil {
			return err
		}
	}

	return nil
}

func getShortUuid() string {
	envUuid, present := os.LookupEnv("TEST_PROMISE_CONTROLLER_POD_IDENTIFIER_UUID")
	if present {
		return envUuid
	} else {
		return string(uuid.NewUUID()[0:5])
	}
}

func (r *dynamicResourceRequestController) setPipelineCondition(ctx context.Context, unstructuredCRD *unstructured.Unstructured, logger logr.Logger) error {
	setter := clusterutil.UnstructuredSetter(unstructuredCRD)
	getter := clusterutil.UnstructuredGetter(unstructuredCRD)
	condition := clusterutil.Get(getter, clusterv1.ConditionType("PipelineCompleted"))
	if condition == nil {
		clusterutil.Set(setter, &clusterv1.Condition{
			Type:               clusterv1.ConditionType("PipelineCompleted"),
			Status:             v1.ConditionFalse,
			Message:            "Pipeline has not completed",
			Reason:             "PipelineNotCompleted",
			LastTransitionTime: metav1.NewTime(time.Now()),
		})
		logger.Info("setting condition PipelineCompleted false")
		if err := r.Client.Status().Update(ctx, unstructuredCRD); err != nil {
			return err
		}
	}
	return nil
}
