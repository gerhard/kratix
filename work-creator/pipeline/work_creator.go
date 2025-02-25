package pipeline

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"

	goerr "errors"

	"slices"

	ctrl "sigs.k8s.io/controller-runtime"

	platformv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/hash"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type WorkCreator struct {
	K8sClient client.Client
}

func (w *WorkCreator) Execute(rootDirectory, promiseName, namespace, resourceName, workflowType string) error {
	identifier := fmt.Sprintf("%s-%s", promiseName, resourceName)

	if namespace == "" {
		namespace = "kratix-platform-system"
	}

	var logger = ctrl.Log.WithName("work-creator").
		WithValues("identifier", identifier).
		WithValues("workName", identifier).
		WithValues("namespace", namespace).
		WithValues("resourceName", resourceName).
		WithValues("promiseName", promiseName)

	workflowScheduling, err := w.getWorkflowScheduling(rootDirectory)
	if err != nil {
		return err
	}

	var workloadGroups []platformv1alpha1.WorkloadGroup
	var directoriesToIgnoreForTheBaseScheduling []string
	var defaultDestinationSelectors map[string]string
	pipelineOutputDir := filepath.Join(rootDirectory, "input")
	for _, workflowDestinationSelector := range workflowScheduling {
		directory := workflowDestinationSelector.Directory
		if !isRootDirectory(directory) {
			directoriesToIgnoreForTheBaseScheduling = append(directoriesToIgnoreForTheBaseScheduling, directory)

			workloads, err := w.getWorkloadsFromDir(pipelineOutputDir, filepath.Join(pipelineOutputDir, directory), nil)

			if err != nil {
				return err
			}

			workloadGroups = append(workloadGroups, platformv1alpha1.WorkloadGroup{
				Workloads: workloads,
				Directory: directory,
				ID:        fmt.Sprintf("%x", md5.Sum([]byte(directory))),
				DestinationSelectors: []platformv1alpha1.WorkloadGroupScheduling{
					{
						MatchLabels: workflowDestinationSelector.MatchLabels,
						Source:      workflowType + "-" + "workflow",
					},
				},
			})
		} else {
			defaultDestinationSelectors = workflowDestinationSelector.MatchLabels
		}
	}

	workloads, err := w.getWorkloadsFromDir(pipelineOutputDir, pipelineOutputDir, directoriesToIgnoreForTheBaseScheduling)
	if err != nil {
		return err
	}

	if len(workloads) > 0 {
		defaultWorkloadGroup := platformv1alpha1.WorkloadGroup{
			Workloads: workloads,
			Directory: platformv1alpha1.DefaultWorkloadGroupDirectory,
			ID:        hash.ComputeHash(platformv1alpha1.DefaultWorkloadGroupDirectory),
		}

		if defaultDestinationSelectors != nil {
			defaultWorkloadGroup.DestinationSelectors = []platformv1alpha1.WorkloadGroupScheduling{
				{
					MatchLabels: defaultDestinationSelectors,
					Source:      workflowType + "-" + "workflow",
				},
			}
		}

		destinationSelectors, err := w.getPromiseScheduling(rootDirectory)
		if err != nil {
			return err
		}
		if len(destinationSelectors) > 0 {
			p := []platformv1alpha1.PromiseScheduling{}
			pw := []platformv1alpha1.PromiseScheduling{}
			for _, selector := range destinationSelectors {
				switch selector.Source {
				case "promise":
					p = append(p, platformv1alpha1.PromiseScheduling{
						MatchLabels: selector.MatchLabels,
					})
				case "promise-workflow":
					pw = append(pw, platformv1alpha1.PromiseScheduling{
						MatchLabels: selector.MatchLabels,
					})
				}
			}

			if len(pw) > 0 {
				defaultWorkloadGroup.DestinationSelectors = append(defaultWorkloadGroup.DestinationSelectors, platformv1alpha1.WorkloadGroupScheduling{
					MatchLabels: platformv1alpha1.SquashPromiseScheduling(pw),
					Source:      "promise-workflow",
				})
			}

			if len(p) > 0 {
				defaultWorkloadGroup.DestinationSelectors = append(
					defaultWorkloadGroup.DestinationSelectors,
					platformv1alpha1.WorkloadGroupScheduling{
						MatchLabels: platformv1alpha1.SquashPromiseScheduling(p),
						Source:      "promise",
					},
				)
			}
		}

		workloadGroups = append(workloadGroups, defaultWorkloadGroup)
	}

	work := &platformv1alpha1.Work{}

	work.Name = identifier
	work.Namespace = namespace
	work.Spec.Replicas = platformv1alpha1.ResourceRequestReplicas
	work.Spec.WorkloadGroups = workloadGroups
	work.Spec.PromiseName = promiseName
	work.Spec.ResourceName = resourceName

	if workflowType == platformv1alpha1.KratixWorkflowTypePromise {
		work.Name = promiseName
		work.Namespace = platformv1alpha1.KratixSystemNamespace
		work.Spec.Replicas = platformv1alpha1.DependencyReplicas
		work.Spec.ResourceName = ""
		work.Labels = platformv1alpha1.GenerateSharedLabelsForPromise(promiseName)
	}

	err = w.K8sClient.Create(context.Background(), work)

	if errors.IsAlreadyExists(err) {
		logger.Info("Work already exists, will update")
		currentWork := platformv1alpha1.Work{}
		key := client.ObjectKeyFromObject(work)

		err := w.K8sClient.Get(context.Background(), key, &currentWork)
		if err != nil {
			logger.Error(err, "Error retrieving Work")
		}

		currentWork.Spec = work.Spec
		err = w.K8sClient.Update(context.Background(), &currentWork)

		if err != nil {
			logger.Error(err, "Error updating Work")
		}
		logger.Info("Work updated")
		return nil
	} else if err != nil {
		return err
	} else {
		logger.Info("Work created")
		return nil
	}
}

// /kratix/output/     /kratix/output/   "bar"
func (w *WorkCreator) getWorkloadsFromDir(prefixToTrimFromWorkloadFilepath, rootDir string, directoriesToIgnoreAtTheRootLevel []string) ([]platformv1alpha1.Workload, error) {
	filesAndDirs, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, err
	}

	workloads := []platformv1alpha1.Workload{}

	for _, info := range filesAndDirs {
		// TODO: currently we assume everything is a file or a dir, we don't handle
		// more advanced scenarios, e.g. symlinks, file sizes, file permissions etc
		if info.IsDir() {
			if !slices.Contains(directoriesToIgnoreAtTheRootLevel, info.Name()) {
				dir := filepath.Join(rootDir, info.Name())
				newWorkloads, err := w.getWorkloadsFromDir(prefixToTrimFromWorkloadFilepath, dir, nil)
				if err != nil {
					return nil, err
				}
				workloads = append(workloads, newWorkloads...)
			}
		} else {
			filePath := filepath.Join(rootDir, info.Name())
			file, err := os.Open(filePath)
			if err != nil {
				return nil, err
			}
			byteValue, err := io.ReadAll(file)
			if err != nil {
				return nil, err
			}

			// trim /kratix/output/ from the filepath
			path, err := filepath.Rel(prefixToTrimFromWorkloadFilepath, filePath)
			if err != nil {
				return nil, err
			}

			workload := platformv1alpha1.Workload{
				Content:  string(byteValue),
				Filepath: path,
			}

			workloads = append(workloads, workload)
		}
	}
	return workloads, nil
}

func (w *WorkCreator) getWorkflowScheduling(rootDirectory string) ([]platformv1alpha1.WorkflowDestinationSelectors, error) {
	metadataDirectory := filepath.Join(rootDirectory, "metadata")
	return getSelectorsFromFile(filepath.Join(metadataDirectory, "destination-selectors.yaml"))
}

func (w *WorkCreator) getPromiseScheduling(rootDirectory string) ([]platformv1alpha1.WorkloadGroupScheduling, error) {
	kratixSystemDirectory := filepath.Join(rootDirectory, "kratix-system")
	file := filepath.Join(kratixSystemDirectory, "promise-scheduling")
	fileContents, err := os.ReadFile(file)
	if err != nil {
		if goerr.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var schedulingConfig []platformv1alpha1.WorkloadGroupScheduling
	err = yaml.Unmarshal(fileContents, &schedulingConfig)

	if err != nil {
		return nil, err
	}

	return schedulingConfig, nil
}

func getSelectorsFromFile(file string) ([]platformv1alpha1.WorkflowDestinationSelectors, error) {
	fileContents, err := os.ReadFile(file)
	if err != nil {
		if goerr.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var schedulingConfig []platformv1alpha1.WorkflowDestinationSelectors
	err = yaml.Unmarshal(fileContents, &schedulingConfig)

	if err != nil {
		return nil, err
	}
	for i := range schedulingConfig {
		schedulingConfig[i].Directory = filepath.Clean(schedulingConfig[i].Directory)
	}

	if containsDuplicateScheduling(schedulingConfig) {
		err = fmt.Errorf("duplicate entries in destination-selectors.yaml: \n%v", schedulingConfig)
		return nil, err
	}

	if path, found := containsNonRootDirectory(schedulingConfig); found {
		return nil, fmt.Errorf("invalid directory in destination-selectors.yaml: %s, directory must be top-level", path)
	}

	return schedulingConfig, nil
}

func containsNonRootDirectory(schedulingConfig []platformv1alpha1.WorkflowDestinationSelectors) (string, bool) {
	for _, selector := range schedulingConfig {
		directory := selector.Directory
		if filepath.Base(directory) != directory {
			return directory, true
		}
	}

	return "", false
}

func containsDuplicateScheduling(schedulingConfig []platformv1alpha1.WorkflowDestinationSelectors) bool {
	directoriesSeen := []string{}

	for _, selector := range schedulingConfig {
		if slices.Contains(directoriesSeen, selector.Directory) {
			return true
		}

		directoriesSeen = append(directoriesSeen, selector.Directory)
	}

	return false
}

// Assumes Dir has already been filepath.Clean'd
func isRootDirectory(dir string) bool {
	return dir == platformv1alpha1.DefaultWorkloadGroupDirectory
}
