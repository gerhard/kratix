package controllers_test

import (
	"context"
	"os"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/controllers"
	"github.com/syntasso/kratix/controllers/controllersfakes"
)

var (
	ctx                  context.Context
	reconciler           *controllers.PromiseReconciler
	promise              *v1alpha1.Promise
	promiseName          types.NamespacedName
	promiseResourcesName types.NamespacedName

	promiseGroup        = "marketplace.kratix.io"
	promiseResourceName string
	expectedCRDName     string
	promiseCommonLabels map[string]string
	managerRestarted    bool
)

var _ = Describe("PromiseController", func() {
	BeforeEach(func() {
		promiseResourceName = "redis"
		expectedCRDName = promiseResourceName + "." + promiseGroup
		ctx = context.Background()
		managerRestarted = false
		reconciler = &controllers.PromiseReconciler{
			Client:              fakeK8sClient,
			ApiextensionsClient: fakeApiExtensionsClient,
			Log:                 ctrl.Log.WithName("controllers").WithName("Promise"),
			Manager:             &controllersfakes.FakeManager{},
			RestartManager: func() {
				managerRestarted = true
			},
		}
	})

	Describe("Promise Reconciliation", func() {
		When("the promise is being created", func() {
			When("it contains everything apart from promise workflows", func() {
				BeforeEach(func() {
					promise = promiseFromFile(promisePath)
					promiseName = types.NamespacedName{
						Name:      promise.GetName(),
						Namespace: promise.GetNamespace(),
					}

					promiseCommonLabels = map[string]string{
						"kratix-promise-id": promise.GetName(),
					}

					Expect(fakeK8sClient.Create(ctx, promise)).To(Succeed())
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					promise.UID = types.UID("1234abcd")
					Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())
				})

				It("re-reconciles until completion", func() {
					result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
						funcs: []func(client.Object) error{autoMarkCRDAsEstablished},
					})

					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					By("creating the CRD", func() {
						crd, err := fakeApiExtensionsClient.CustomResourceDefinitions().Get(ctx, expectedCRDName, metav1.GetOptions{})

						Expect(err).NotTo(HaveOccurred())
						Expect(crd.Spec.Names.Plural + "." + crd.Spec.Group).To(Equal(expectedCRDName))
						status, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
						Expect(ok).To(BeTrue(), "expected .status to exist")

						Expect(status.XPreserveUnknownFields).ToNot(BeNil())
						Expect(*status.XPreserveUnknownFields).To(BeTrue())

						message, ok := status.Properties["message"]
						Expect(ok).To(BeTrue(), ".status.message did not exist. Spec %v", status)
						Expect(message.Type).To(Equal("string"))

						conditions, ok := status.Properties["conditions"]
						Expect(ok).To(BeTrue())
						Expect(conditions.Type).To(Equal("array"))

						conditionsProperties := conditions.Items.Schema.Properties

						lastTransitionTime, ok := conditionsProperties["lastTransitionTime"]
						Expect(ok).To(BeTrue())
						Expect(lastTransitionTime.Type).To(Equal("string"))

						message, ok = conditionsProperties["message"]
						Expect(ok).To(BeTrue())
						Expect(message.Type).To(Equal("string"))

						reason, ok := conditionsProperties["reason"]
						Expect(ok).To(BeTrue())
						Expect(reason.Type).To(Equal("string"))

						status, ok = conditionsProperties["status"]
						Expect(ok).To(BeTrue())
						Expect(status.Type).To(Equal("string"))

						typeField, ok := conditionsProperties["type"]
						Expect(ok).To(BeTrue())
						Expect(typeField.Type).To(Equal("string"))

						printerFields := crd.Spec.Versions[0].AdditionalPrinterColumns
						Expect(printerFields).ToNot(BeNil())
					})

					By("updating the status with the CRD values", func() {
						Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
						Expect(promise.Status.APIVersion).To(Equal("marketplace.kratix.io/v1alpha1"))
						Expect(promise.Status.Kind).To(Equal("redis"))
					})

					By("creating the resources for the dynamic controller", func() {
						controllerResourceName := types.NamespacedName{
							Name: promise.GetControllerResourceName(),
						}
						clusterrole := &rbacv1.ClusterRole{}
						Expect(fakeK8sClient.Get(ctx, controllerResourceName, clusterrole)).To(Succeed())

						Expect(clusterrole.Rules).To(ConsistOf(
							rbacv1.PolicyRule{
								Verbs:     []string{rbacv1.VerbAll},
								APIGroups: []string{promiseGroup},
								Resources: []string{promiseResourceName},
							},
							rbacv1.PolicyRule{
								Verbs:     []string{"update"},
								APIGroups: []string{promiseGroup},
								Resources: []string{promiseResourceName + "/finalizers"},
							},
							rbacv1.PolicyRule{
								Verbs:     []string{"get", "update", "patch"},
								APIGroups: []string{promiseGroup},
								Resources: []string{promiseResourceName + "/status"},
							},
						))
						Expect(clusterrole.GetLabels()).To(Equal(promiseCommonLabels))

						binding := &rbacv1.ClusterRoleBinding{}
						Expect(fakeK8sClient.Get(ctx, controllerResourceName, binding)).To(Succeed(), "Expected controller binding to exist")
						Expect(binding.RoleRef.Name).To(Equal(promise.GetControllerResourceName()))
						Expect(binding.Subjects).To(HaveLen(1))
						Expect(binding.Subjects[0]).To(Equal(rbacv1.Subject{
							Kind:      "ServiceAccount",
							Namespace: v1alpha1.KratixSystemNamespace,
							Name:      "kratix-platform-controller-manager",
						}))
						Expect(binding.GetLabels()).To(Equal(promiseCommonLabels))
					})

					By("setting the finalizers", func() {
						Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
						Expect(promise.Finalizers).To(ConsistOf(
							"kratix.io/dynamic-controller-dependant-resources-cleanup",
							"kratix.io/dependencies-cleanup",
							"kratix.io/resource-request-cleanup",
							"kratix.io/api-crd-cleanup",
						))
					})

					By("creating a Work resource for the dependencies", func() {
						workNamespacedName := types.NamespacedName{
							Name:      promise.GetName(),
							Namespace: v1alpha1.KratixSystemNamespace,
						}
						Expect(fakeK8sClient.Get(ctx, workNamespacedName, &v1alpha1.Work{})).To(Succeed())
					})

					By("updating the status.obeservedGeneration", func() {
						Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
						Expect(promise.Status.ObservedGeneration).To(Equal(promise.Generation))
					})

					By("starting the dynamic controller", func() {
						Expect(reconciler.StartedDynamicControllers).To(HaveLen(1))
						Expect(*reconciler.StartedDynamicControllers["1234abcd"].CanCreateResources).To(BeTrue())
					})
				})
			})
			When("the promise has requirements", func() {
				BeforeEach(func() {
					promise = promiseFromFile(promiseWithRequirements)
					promiseName = types.NamespacedName{
						Name:      promise.GetName(),
						Namespace: promise.GetNamespace(),
					}
					promiseResourceName = "namespaces"
					expectedCRDName = promiseResourceName + "." + promiseGroup

					Expect(fakeK8sClient.Create(ctx, promise)).To(Succeed())
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					promise.UID = types.UID("1234abcd")
					Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())
				})

				When("the promise requirements are not installed", func() {
					It("updates the status to indicate the dependencies are not installed and prevents RRs being reconciled", func() {
						_, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
							funcs: []func(client.Object) error{autoMarkCRDAsEstablished},
						})
						Expect(err).To(MatchError("reconcile loop detected"))
						Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
						Expect(promise.Status.Conditions).To(HaveLen(1))
						Expect(promise.Status.Conditions[0].Type).To(Equal("RequirementsFulfilled"))
						Expect(promise.Status.Conditions[0].Status).To(Equal(metav1.ConditionFalse))
						Expect(promise.Status.Conditions[0].Message).To(Equal("Requirements not fulfilled"))
						Expect(promise.Status.Conditions[0].Reason).To(Equal("RequirementsNotInstalled"))
						Expect(promise.Status.Conditions[0].LastTransitionTime).ToNot(BeNil())

						Expect(promise.Status.Requirements).To(ConsistOf(
							v1alpha1.RequirementStatus{
								Name:    "kafka",
								Version: "v1.2.0",
								State:   "Requirement not installed",
							},
						))
						Expect(promise.Status.Status).To(Equal(v1alpha1.PromiseStatusUnavailable))

						Expect(reconciler.StartedDynamicControllers).To(HaveLen(1))
						Expect(*reconciler.StartedDynamicControllers["1234abcd"].CanCreateResources).To(BeFalse())
					})
				})

				When("the promise requirements are not installed at the specified version", func() {
					BeforeEach(func() {
						requiredPromise := &v1alpha1.Promise{}
						requiredPromiseName := types.NamespacedName{
							Name: "kafka",
						}
						err := fakeK8sClient.Create(ctx, &v1alpha1.Promise{
							ObjectMeta: metav1.ObjectMeta{
								Name: "kafka",
							},
						})
						Expect(err).ToNot(HaveOccurred())
						Expect(fakeK8sClient.Get(ctx, requiredPromiseName, requiredPromise)).To(Succeed())
						requiredPromise.Status.Status = "Available"
						requiredPromise.Status.Version = "v1.0.0"
						Expect(fakeK8sClient.Status().Update(ctx, requiredPromise)).To(Succeed())
					})

					It("updates the status to indicate the dependencies are not installed at the specified version", func() {
						_, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
							funcs: []func(client.Object) error{autoMarkCRDAsEstablished},
						})

						Expect(err).To(MatchError("reconcile loop detected"))
						Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())

						Expect(promise.Status.Conditions).To(HaveLen(1))
						Expect(promise.Status.Conditions[0].Type).To(Equal("RequirementsFulfilled"))
						Expect(promise.Status.Conditions[0].Status).To(Equal(metav1.ConditionFalse))
						Expect(promise.Status.Conditions[0].Message).To(Equal("Requirements not fulfilled"))
						Expect(promise.Status.Conditions[0].Reason).To(Equal("RequirementsNotInstalled"))
						Expect(promise.Status.Conditions[0].LastTransitionTime).ToNot(BeNil())

						Expect(promise.Status.Requirements).To(ConsistOf(
							v1alpha1.RequirementStatus{
								Name:    "kafka",
								Version: "v1.2.0",
								State:   "Requirement not installed at the specified version",
							},
						))

						Expect(promise.Status.Status).To(Equal(v1alpha1.PromiseStatusUnavailable))
					})
				})

				When("the promise requirements are installed at the specified version", func() {
					When("the required promise is not available", func() {
						BeforeEach(func() {
							requiredPromise := &v1alpha1.Promise{}
							requiredPromiseName := types.NamespacedName{
								Name: "kafka",
							}
							err := fakeK8sClient.Create(ctx, &v1alpha1.Promise{
								ObjectMeta: metav1.ObjectMeta{
									Name: "kafka",
								},
							})
							Expect(err).ToNot(HaveOccurred())
							Expect(fakeK8sClient.Get(ctx, requiredPromiseName, requiredPromise)).To(Succeed())
							requiredPromise.Status.Status = "Unavailable"
							requiredPromise.Status.Version = "v1.2.0"
							Expect(fakeK8sClient.Status().Update(ctx, requiredPromise)).To(Succeed())
						})

						It("returns false for the RequirementsFulfilled condition", func() {
							_, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
								funcs: []func(client.Object) error{autoMarkCRDAsEstablished},
							})

							Expect(err).To(MatchError("reconcile loop detected"))
							Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())

							Expect(promise.Status.Conditions).To(HaveLen(1))
							Expect(promise.Status.Conditions[0].Type).To(Equal("RequirementsFulfilled"))
							Expect(promise.Status.Conditions[0].Status).To(Equal(metav1.ConditionFalse))

							Expect(promise.Status.Requirements).To(ConsistOf(
								v1alpha1.RequirementStatus{
									Name:    "kafka",
									Version: "v1.2.0",
									State:   "Requirement not installed at the specified version",
								},
							))

							Expect(promise.Status.Status).To(Equal(v1alpha1.PromiseStatusUnavailable))
						})
					})

					When("the required promise is available", func() {
						BeforeEach(func() {
							requiredPromise := &v1alpha1.Promise{}
							requiredPromiseName := types.NamespacedName{
								Name: "kafka",
							}
							err := fakeK8sClient.Create(ctx, &v1alpha1.Promise{
								ObjectMeta: metav1.ObjectMeta{
									Name: "kafka",
								},
							})
							Expect(err).ToNot(HaveOccurred())
							Expect(fakeK8sClient.Get(ctx, requiredPromiseName, requiredPromise)).To(Succeed())
							requiredPromise.Status.Status = "Available"
							requiredPromise.Status.Version = "v1.2.0"
							Expect(fakeK8sClient.Status().Update(ctx, requiredPromise)).To(Succeed())
						})

						It("returns true for the RequirementsFulfilled condition", func() {
							_, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
								funcs: []func(client.Object) error{autoMarkCRDAsEstablished},
							})

							Expect(err).NotTo(HaveOccurred())
							Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())

							Expect(promise.Status.Conditions).To(HaveLen(1))
							Expect(promise.Status.Conditions[0].Type).To(Equal("RequirementsFulfilled"))
							Expect(promise.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))

							Expect(promise.Status.Requirements).To(ConsistOf(
								v1alpha1.RequirementStatus{
									Name:    "kafka",
									Version: "v1.2.0",
									State:   "Requirement installed",
								},
							))
							Expect(promise.Status.Status).To(Equal(v1alpha1.PromiseStatusAvailable))

							Expect(reconciler.StartedDynamicControllers).To(HaveLen(1))
							Expect(*reconciler.StartedDynamicControllers["1234abcd"].CanCreateResources).To(BeTrue())
						})
					})
				})
			})

			When("it contains Promise workflows", func() {
				BeforeEach(func() {
					promise = promiseFromFile(promiseWithWorkflowPath)
					promiseName = types.NamespacedName{
						Name:      promise.GetName(),
						Namespace: promise.GetNamespace(),
					}
					promiseCommonLabels = map[string]string{
						"kratix-promise-id": promise.GetName(),
					}
					promiseResourcesName = types.NamespacedName{
						Name:      promise.GetName() + "-promise-pipeline",
						Namespace: "kratix-platform-system",
					}

					Expect(fakeK8sClient.Create(ctx, promise)).To(Succeed())
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					promise.UID = types.UID("1234abcd")
					Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())
				})

				It("re-reconciles until completion", func() {
					_, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
						funcs: []func(client.Object) error{autoMarkCRDAsEstablished},
					})

					By("setting the finalizer for the Promise workflows", func() {
						Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
						Expect(promise.Finalizers).To(ContainElement("kratix.io/workflows-cleanup"))
					})

					By("creates a service account for pipeline", func() {
						sa := &v1.ServiceAccount{}
						Expect(fakeK8sClient.Get(ctx, promiseResourcesName, sa)).To(Succeed(), "Expected SA for pipeline to exist")
						Expect(sa.GetLabels()).To(Equal(promiseCommonLabels))
					})

					By("creates a config map with the promise scheduling in it", func() {
						configMap := &v1.ConfigMap{}
						configMapName := types.NamespacedName{
							Name:      "destination-selectors-" + promise.GetName(),
							Namespace: "kratix-platform-system",
						}
						Expect(fakeK8sClient.Get(ctx, configMapName, configMap)).Should(Succeed(), "Expected ConfigMap for pipeline to exist")
						Expect(configMap.GetLabels()).To(Equal(promiseCommonLabels))
						Expect(configMap.Data).To(HaveKey("destinationSelectors"))
						space := regexp.MustCompile(`\s+`)
						destinationSelectors := space.ReplaceAllString(configMap.Data["destinationSelectors"], " ")
						Expect(strings.TrimSpace(destinationSelectors)).To(Equal(`- matchlabels: environment: dev source: promise`))
					})

					promiseResourcesName.Namespace = ""
					By("creates a role for the pipeline service account", func() {
						role := &rbacv1.ClusterRole{}
						Expect(fakeK8sClient.Get(ctx, promiseResourcesName, role)).To(Succeed(), "Expected Role for pipeline to exist")
						Expect(role.GetLabels()).To(Equal(promiseCommonLabels))
						Expect(role.Rules).To(ConsistOf(
							rbacv1.PolicyRule{
								Verbs:     []string{"get", "list", "update", "create", "patch"},
								APIGroups: []string{"platform.kratix.io"},
								Resources: []string{"promises", "promises/status", "works"},
							},
						))
						Expect(role.GetLabels()).To(Equal(promiseCommonLabels))
					})

					By("associates the new role with the new service account", func() {
						binding := &rbacv1.ClusterRoleBinding{}
						Expect(fakeK8sClient.Get(ctx, promiseResourcesName, binding)).To(Succeed(), "Expected ClusterRoleBinding for pipeline to exist")
						Expect(binding.RoleRef.Name).To(Equal(promiseResourcesName.Name))
						Expect(binding.Subjects).To(HaveLen(1))
						Expect(binding.Subjects[0]).To(Equal(rbacv1.Subject{
							Kind:      "ServiceAccount",
							Namespace: "kratix-platform-system",
							Name:      promiseResourcesName.Name,
						}))
						Expect(binding.GetLabels()).To(Equal(promiseCommonLabels))
					})

					By("requeuing forever until jobs finishes", func() {
						Expect(err).To(MatchError("reconcile loop detected"))
						jobs := &batchv1.JobList{}
						Expect(fakeK8sClient.List(ctx, jobs)).To(Succeed())
						Expect(jobs.Items).To(HaveLen(1))
						Expect(jobs.Items[0].Spec.Template.Spec.InitContainers[1].Image).To(Equal("syntasso/promise-with-workflow:v0.1.0"))
					})

					By("finishing the creation once the job is finished", func() {
						result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
							funcs: []func(client.Object) error{
								autoCompleteJobAndCreateWork(promiseCommonLabels, promise.GetName()),
							},
						})

						Expect(err).NotTo(HaveOccurred())
						Expect(result).To(Equal(ctrl.Result{}))
					})
				})
			})

			When("it contains only dependencies, no API/workflows", func() {
				BeforeEach(func() {
					promise = promiseFromFile(promiseWithOnlyDepsPath)
					promiseName = types.NamespacedName{
						Name:      promise.GetName(),
						Namespace: promise.GetNamespace(),
					}
					promiseCommonLabels = map[string]string{
						"kratix-promise-id": promise.GetName(),
					}
					promiseResourcesName = types.NamespacedName{
						Name:      promise.GetName() + "-promise-pipeline",
						Namespace: "kratix-platform-system",
					}

					Expect(fakeK8sClient.Create(ctx, promise)).To(Succeed())
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					promise.UID = types.UID("1234abcd")
					Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())
				})

				It("re-reconciles until completetion", func() {
					result, err := t.reconcileUntilCompletion(reconciler, promise)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					crds, err := fakeApiExtensionsClient.CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
					Expect(err).NotTo(HaveOccurred())
					Expect(crds.Items).To(HaveLen(0))
					works := &v1alpha1.WorkList{}
					Expect(fakeK8sClient.List(ctx, works)).To(Succeed())
					Expect(works.Items).To(HaveLen(1))
					Expect(works.Items[0].Spec.WorkloadGroups[0].Workloads).To(HaveLen(1))
					Expect(works.Items[0].Spec.WorkloadGroups[0].Workloads[0].Content).To(ContainSubstring("redisoperator"))
					Expect(works.Items[0].Spec.WorkloadGroups[0].DestinationSelectors).To(HaveLen(1))
					Expect(works.Items[0].Spec.WorkloadGroups[0].DestinationSelectors[0]).To(Equal(
						v1alpha1.WorkloadGroupScheduling{
							MatchLabels: map[string]string{
								"environment": "dev",
							},
							Source: "promise",
						},
					))
				})
			})
		})

		When("the promise is being deleted", func() {
			When("the Promise has no delete workflow", func() {
				BeforeEach(func() {
					promise = promiseFromFile(promiseWithWorkflowPath)
					promiseName = types.NamespacedName{
						Name:      promise.GetName(),
						Namespace: promise.GetNamespace(),
					}
					promiseCommonLabels = map[string]string{
						"kratix-promise-id": promise.GetName(),
					}

					Expect(fakeK8sClient.Create(ctx, promise)).To(Succeed())
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					promise.UID = types.UID("1234abcd")
					Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())
					result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
						funcs: []func(client.Object) error{
							autoMarkCRDAsEstablished,
							autoCompleteJobAndCreateWork(promiseCommonLabels, promise.GetName()),
						},
					})

					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))
				})

				It("sets the finalizers on the Promise", func() {
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					Expect(promise.Finalizers).To(ConsistOf(
						"kratix.io/dynamic-controller-dependant-resources-cleanup",
						"kratix.io/dependencies-cleanup",
						"kratix.io/resource-request-cleanup",
						"kratix.io/api-crd-cleanup",
						"kratix.io/workflows-cleanup",
					))
				})

				It("deletes all resources for the promise workflow and dynamic controller", func() {
					//check resources all exist before deletion
					crds, err := fakeApiExtensionsClient.CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
					Expect(err).NotTo(HaveOccurred())
					Expect(crds.Items).To(HaveLen(1))

					clusterRoles := &rbacv1.ClusterRoleList{}
					clusterRoleBindings := &rbacv1.ClusterRoleBindingList{}
					jobs := &batchv1.JobList{}
					works := &v1alpha1.WorkList{}
					configMaps := &v1.ConfigMapList{}
					serviceAccounts := &v1.ServiceAccountList{}
					Expect(fakeK8sClient.List(ctx, clusterRoles)).To(Succeed())
					Expect(fakeK8sClient.List(ctx, clusterRoleBindings)).To(Succeed())
					Expect(fakeK8sClient.List(ctx, works)).To(Succeed())
					Expect(fakeK8sClient.List(ctx, jobs)).To(Succeed())
					Expect(fakeK8sClient.List(ctx, serviceAccounts)).To(Succeed())
					Expect(clusterRoles.Items).To(HaveLen(2))
					Expect(clusterRoleBindings.Items).To(HaveLen(2))
					Expect(works.Items).To(HaveLen(1))
					Expect(jobs.Items).To(HaveLen(1))
					Expect(serviceAccounts.Items).To(HaveLen(1))

					//Delete
					Expect(fakeK8sClient.Delete(ctx, promise)).To(Succeed())
					result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{errorBudget: 5})
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))
					Expect(managerRestarted).To(BeTrue())

					//Check they are all gone
					Expect(fakeK8sClient.List(ctx, jobs)).To(Succeed())
					Expect(jobs.Items).To(HaveLen(0))

					crds, err = fakeApiExtensionsClient.CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
					Expect(err).NotTo(HaveOccurred())
					Expect(crds.Items).To(HaveLen(0))
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(MatchError(ContainSubstring("not found")))
					Expect(fakeK8sClient.List(ctx, clusterRoles)).To(Succeed())
					Expect(fakeK8sClient.List(ctx, clusterRoleBindings)).To(Succeed())
					Expect(fakeK8sClient.List(ctx, works)).To(Succeed())
					Expect(fakeK8sClient.List(ctx, configMaps)).To(Succeed())
					Expect(fakeK8sClient.List(ctx, serviceAccounts)).To(Succeed())
					Expect(clusterRoles.Items).To(HaveLen(0))
					Expect(clusterRoleBindings.Items).To(HaveLen(0))
					Expect(works.Items).To(HaveLen(0))
					Expect(configMaps.Items).To(HaveLen(0))
					Expect(serviceAccounts.Items).To(HaveLen(0))
				})

				When("a resource request exists", func() {
					It("deletes them", func() {
						yamlFile, err := os.ReadFile(resourceRequestPath)
						Expect(err).ToNot(HaveOccurred())

						requestedResource := &unstructured.Unstructured{}
						Expect(yaml.Unmarshal(yamlFile, requestedResource)).To(Succeed())
						Expect(fakeK8sClient.Create(ctx, requestedResource)).To(Succeed())
						resNameNamespacedName := types.NamespacedName{
							Name:      requestedResource.GetName(),
							Namespace: requestedResource.GetNamespace(),
						}
						Expect(fakeK8sClient.Get(ctx, resNameNamespacedName, requestedResource)).To(Succeed())

						Expect(fakeK8sClient.Delete(ctx, promise)).To(Succeed())
						result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{errorBudget: 5})
						Expect(err).NotTo(HaveOccurred())
						Expect(result).To(Equal(ctrl.Result{}))
						Expect(managerRestarted).To(BeTrue())
						Expect(fakeK8sClient.Get(ctx, resNameNamespacedName, requestedResource)).To(MatchError(ContainSubstring("not found")))
					})
				})
			})

			When("the Promise has a delete workflow", func() {
				var err error

				BeforeEach(func() {
					promise = promiseFromFile(promiseWithDeleteWorkflowPath)
					promiseName = types.NamespacedName{
						Name:      promise.GetName(),
						Namespace: promise.GetNamespace(),
					}
					promiseCommonLabels = map[string]string{
						"kratix-promise-id": promise.GetName(),
					}

					promise.UID = types.UID("1234abcd")
					Expect(fakeK8sClient.Create(ctx, promise)).To(Succeed())

					result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
						funcs: []func(client.Object) error{
							autoMarkCRDAsEstablished,
							autoCompleteJobAndCreateWork(promiseCommonLabels, promise.GetName()),
						},
					})

					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))
				})

				It("sets the delete-workflows finalizer", func() {
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					Expect(promise.Finalizers).To(ConsistOf(
						"kratix.io/dynamic-controller-dependant-resources-cleanup",
						"kratix.io/dependencies-cleanup",
						"kratix.io/resource-request-cleanup",
						"kratix.io/api-crd-cleanup",
						"kratix.io/workflows-cleanup",
						"kratix.io/delete-workflows",
					))
				})

				It("requeues forever until the delete job finishes", func() {
					Expect(fakeK8sClient.Delete(ctx, promise)).To(Succeed())
					_, err = t.reconcileUntilCompletion(reconciler, promise)

					Expect(err).To(MatchError("reconcile loop detected"))
					jobs := &batchv1.JobList{}
					Expect(fakeK8sClient.List(ctx, jobs)).To(Succeed())
					Expect(jobs.Items).To(HaveLen(1))
					Expect([]string{
						//Configure have initcontainer[0] = reader, initcontainer[1] = users pipeline
						jobs.Items[0].Spec.Template.Spec.Containers[0].Image,
					}).To(ConsistOf(
						"syntasso/promise-with-workflow-delete:v0.1.0",
					))
				})

				It("finishes the deletion once the job is finished", func() {
					Expect(fakeK8sClient.Delete(ctx, promise)).To(Succeed())
					_, err = t.reconcileUntilCompletion(reconciler, promise)

					result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
						funcs: []func(client.Object) error{
							autoCompleteJobAndCreateWork(promiseCommonLabels, promise.GetName()),
						},
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					jobs := &batchv1.JobList{}
					works := &v1alpha1.WorkList{}
					Expect(fakeK8sClient.List(ctx, works)).To(Succeed())
					Expect(fakeK8sClient.List(ctx, jobs)).To(Succeed())
					Expect(works.Items).To(HaveLen(0))
					Expect(jobs.Items).To(HaveLen(0))
				})

				When("the Promise is updated to no longer have a delete workflow", func() {
					BeforeEach(func() {
						Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
						promise.Spec.Workflows.Promise.Delete = nil
						Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())

						result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
							funcs: []func(client.Object) error{
								autoCompleteJobAndCreateWork(promiseCommonLabels, promise.GetName()),
							},
						})
						Expect(err).NotTo(HaveOccurred())
						Expect(result).To(Equal(ctrl.Result{}))
					})

					It("removes the delete-workflows finalizer", func() {
						Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
						Expect(promise.Finalizers).To(ConsistOf(
							"kratix.io/dynamic-controller-dependant-resources-cleanup",
							"kratix.io/dependencies-cleanup",
							"kratix.io/resource-request-cleanup",
							"kratix.io/api-crd-cleanup",
							"kratix.io/workflows-cleanup",
						))
					})
				})
			})

			//TODO
			// - test https://github.com/syntasso/kratix/blob/dev/controllers/promise_controller_test.go#L65
		})

		When("the promise is being updated", func() {
			When("it contains a workflow", func() {
				BeforeEach(func() {
					promise = promiseFromFile(promiseWithWorkflowPath)
					promiseName = types.NamespacedName{
						Name:      promise.GetName(),
						Namespace: promise.GetNamespace(),
					}
					promiseCommonLabels = map[string]string{
						"kratix-promise-id": promise.GetName(),
					}
					promiseResourcesName = types.NamespacedName{
						Name:      promise.GetName() + "-promise-pipeline",
						Namespace: "kratix-platform-system",
					}

					Expect(fakeK8sClient.Create(ctx, promise)).To(Succeed())
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					promise.UID = types.UID("1234abcd")
					Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())
					result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
						funcs: []func(client.Object) error{autoMarkCRDAsEstablished, autoCompleteJobAndCreateWork(promiseCommonLabels, promise.GetName())},
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))
				})

				It("re-reconciles until completion", func() {
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					updatedPromise := promiseFromFile(promiseWithWorkflowUpdatedPath)
					promise.Spec = updatedPromise.Spec
					Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())

					result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
						funcs: []func(client.Object) error{autoMarkCRDAsEstablished, autoCompleteJobAndCreateWork(promiseCommonLabels, promise.GetName())},
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					By("creating a new job with the updated image", func() {
						jobs := &batchv1.JobList{}
						Expect(fakeK8sClient.List(ctx, jobs)).To(Succeed())
						Expect(jobs.Items).To(HaveLen(2))
						//return order is random
						Expect([]string{
							jobs.Items[0].Spec.Template.Spec.InitContainers[1].Image,
							jobs.Items[1].Spec.Template.Spec.InitContainers[1].Image,
						}).To(ContainElements("syntasso/promise-with-workflow:v0.1.0", "syntasso/promise-with-workflow:v0.2.0"))
					})

					By("updating the CRD", func() {
						crds, err := fakeApiExtensionsClient.CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(crds.Items).To(HaveLen(1))
						Expect(crds.Items[0].Spec.Versions[0].Name).To(Equal("v1alpha2"))
					})
				})
			})
			When("it contains static dependencies", func() {
				BeforeEach(func() {
					promise = promiseFromFile(promiseWithOnlyDepsPath)
					promiseName = types.NamespacedName{
						Name:      promise.GetName(),
						Namespace: promise.GetNamespace(),
					}
					promiseCommonLabels = map[string]string{
						"kratix-promise-id": promise.GetName(),
					}
					promiseResourcesName = types.NamespacedName{
						Name:      promise.GetName() + "-promise-pipeline",
						Namespace: "kratix-platform-system",
					}

					Expect(fakeK8sClient.Create(ctx, promise)).To(Succeed())
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					promise.UID = types.UID("1234abcd")
					Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())
					result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
						funcs: []func(client.Object) error{autoMarkCRDAsEstablished, autoCompleteJobAndCreateWork(promiseCommonLabels, promise.GetName())},
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))
				})

				It("re-reconciles until completetion", func() {
					Expect(fakeK8sClient.Get(ctx, promiseName, promise)).To(Succeed())
					updatedPromise := promiseFromFile(promiseWithOnlyDepsUpdatedPath)
					promise.Spec = updatedPromise.Spec
					Expect(fakeK8sClient.Update(ctx, promise)).To(Succeed())

					result, err := t.reconcileUntilCompletion(reconciler, promise, &opts{
						funcs: []func(client.Object) error{autoMarkCRDAsEstablished, autoCompleteJobAndCreateWork(promiseCommonLabels, promise.GetName())},
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					By("updating the work", func() {
						works := &v1alpha1.WorkList{}
						Expect(fakeK8sClient.List(ctx, works)).To(Succeed())
						Expect(works.Items).To(HaveLen(1))
						Expect(works.Items[0].Spec.WorkloadGroups[0].Workloads).To(HaveLen(1))
						Expect(works.Items[0].Spec.WorkloadGroups[0].Workloads[0].Content).To(ContainSubstring("postgresoperator"))
						Expect(works.Items[0].Spec.WorkloadGroups[0].DestinationSelectors).To(HaveLen(1))
						Expect(works.Items[0].Spec.WorkloadGroups[0].DestinationSelectors[0]).To(Equal(
							v1alpha1.WorkloadGroupScheduling{
								MatchLabels: map[string]string{
									"environment": "prod",
								},
								Source: "promise",
							},
						))
					})
				})
			})

		})
	})
})

func autoMarkCRDAsEstablished(obj client.Object) error {
	crd, err := fakeApiExtensionsClient.CustomResourceDefinitions().Get(context.Background(), expectedCRDName, metav1.GetOptions{})

	if err != nil {
		return err
	}
	crd.Status.Conditions = append(crd.Status.Conditions, apiextensionsv1.CustomResourceDefinitionCondition{
		Type:               apiextensionsv1.Established,
		Status:             apiextensionsv1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
	})
	_, err = fakeApiExtensionsClient.CustomResourceDefinitions().Update(context.Background(), crd, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}
