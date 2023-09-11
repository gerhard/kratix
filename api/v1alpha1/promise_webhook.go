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

package v1alpha1

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var (
	promiselog = logf.Log.WithName("promise-resource")
	clientSet  clientset.Interface
)

func (p *Promise) SetupWebhookWithManager(mgr ctrl.Manager, client *clientset.Clientset) error {
	clientSet = client
	return ctrl.NewWebhookManagedBy(mgr).
		For(p).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-platform-kratix-io-v1alpha1-promise,mutating=true,failurePolicy=fail,sideEffects=None,groups=platform.kratix.io,resources=promises,verbs=create;update,versions=v1alpha1,name=mpromise.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &Promise{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (p *Promise) Default() {
	promiselog.Info("default", "name", p.Name)

	// TODO(user): fill in your defaulting logic.
}

//+kubebuilder:webhook:path=/validate-platform-kratix-io-v1alpha1-promise,mutating=false,failurePolicy=fail,sideEffects=None,groups=platform.kratix.io,resources=promises,verbs=create;update,versions=v1alpha1,name=vpromise.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &Promise{}

func (p *Promise) validateCRD() error {
	newCrd, err := p.GetAPIAsCRD()
	if err != nil {
		if err == ErrNoAPI {
			return nil
		}
		return err
	}
	_, err = clientSet.ApiextensionsV1().CustomResourceDefinitions().Create(context.TODO(), newCrd, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			existingCrd, err := clientSet.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), newCrd.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}

			existingCrd.Spec = newCrd.Spec
			_, err = clientSet.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), existingCrd, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
			if err != nil {
				return fmt.Errorf("invalid CRD changes: %w", err)
			}
			return nil
		}
		return fmt.Errorf("invalid CRD: %w", err)
	}
	return nil
}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (p *Promise) ValidateCreate() (admission.Warnings, error) {
	promiselog.Info("validate create", "name", p.Name)
	if err := p.validateCRD(); err != nil {
		return nil, err
	}

	// TODO(user): fill in your validation logic upon object creation.
	return nil, nil
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (p *Promise) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	promiselog.Info("validating promise update", "name", p.Name)
	oldPromise, _ := old.(*Promise)

	oldCrd, _ := oldPromise.GetAPIAsCRD()
	newCrd, _ := p.GetAPIAsCRD()

	if err := p.validateCRD(); err != nil {
		return nil, err
	}

	errors := []string{}
	if oldCrd.Name != newCrd.Name {
		errors = append(errors, fmt.Sprintf("* spec.api.metadata.name: Invalid value: %q: field is immutable", newCrd.Name))
	}

	if oldCrd.Kind != newCrd.Kind {
		errors = append(errors, fmt.Sprintf("* spec.api.kind: Invalid value: %q: field is immutable", newCrd.Kind))
	}

	if oldCrd.APIVersion != newCrd.APIVersion {
		errors = append(errors, fmt.Sprintf("* spec.api.apiVersion: Invalid value: %q: field is immutable", newCrd.APIVersion))
	}

	if !reflect.DeepEqual(oldCrd.Spec.Names, newCrd.Spec.Names) {
		newNames := fmt.Sprintf(
			`{"plural": %q, "singular": %q, "kind": %q}`,
			newCrd.Spec.Names.Plural,
			newCrd.Spec.Names.Singular,
			newCrd.Spec.Names.Kind,
		)
		errors = append(errors, fmt.Sprintf("* spec.api.spec.names: Invalid value: %s: field is immutable", newNames))
	}

	if len(errors) > 0 {
		//TODO: p.Name is coming through empty or so it seems!
		return nil, fmt.Errorf("promises.platform.kratix.io %q was not valid:\n%s", p.Name, strings.Join(errors, "\n"))
	}

	return nil, nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (p *Promise) ValidateDelete() (admission.Warnings, error) {
	promiselog.Info("validate delete", "name", p.Name)

	// TODO(user): fill in your validation logic upon object deletion.
	return nil, nil
}
