/*
Copyright 2018 The Knative Authors

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

// Package pod provides methods to convert a Build CRD to a k8s Pod
// resource.
package resources

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	duckv1alpha1 "github.com/knative/pkg/apis/duck/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	v1alpha1 "github.com/knative/build/pkg/apis/build/v1alpha1"
	"github.com/knative/build/pkg/credentials"
	"github.com/knative/build/pkg/credentials/dockercreds"
	"github.com/knative/build/pkg/credentials/gitcreds"
)

const workspaceDir = "/workspace"

// These are effectively const, but Go doesn't have such an annotation.
var (
	emptyVolumeSource = corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{},
	}
	// These are injected into all of the source/step containers.
	implicitEnvVars = []corev1.EnvVar{{
		Name:  "HOME",
		Value: "/builder/home",
	}}
	implicitVolumeMounts = []corev1.VolumeMount{{
		Name:      "workspace",
		MountPath: workspaceDir,
	}, {
		Name:      "home",
		MountPath: "/builder/home",
	}}
	implicitVolumes = []corev1.Volume{{
		Name:         "workspace",
		VolumeSource: emptyVolumeSource,
	}, {
		Name:         "home",
		VolumeSource: emptyVolumeSource,
	}}

	// Names of implicit steps added during FromCRD. Statuses for these
	// steps are ignored when populating a BuildStatus from a PodStatus.
	implicitStepNames = map[string]bool{
		initContainerPrefix + credsInit:    true,
		initContainerPrefix + gcsSource:    true,
		initContainerPrefix + gitSource:    true,
		initContainerPrefix + customSource: true,
	}
)

const (
	// Prefixes to add to the name of the init containers.
	// IMPORTANT: Changing these values without changing fluentd collection configuration
	// will break log collection for init containers.
	initContainerPrefix        = "build-step-"
	unnamedInitContainerPrefix = "build-step-unnamed-"
	// A label with the following is added to the pod to identify the pods belonging to a build.
	buildNameLabelKey = "build.knative.dev/buildName"
	// Name of the credential initialization container.
	credsInit = "credential-initializer"
	// Names for source containers.
	gitSource    = "git-source"
	gcsSource    = "gcs-source"
	customSource = "custom-source"
)

var (
	// The container used to initialize credentials before the build runs.
	credsImage = flag.String("creds-image", "override-with-creds:latest",
		"The container image for preparing our Build's credentials.")
	// The container with Git that we use to implement the Git source step.
	gitImage = flag.String("git-image", "override-with-git:latest",
		"The container image containing our Git binary.")
	// The container that just prints build successful.
	nopImage = flag.String("nop-image", "override-with-nop:latest",
		"The container image run at the end of the build to log build success")
	gcsFetcherImage = flag.String("gcs-fetcher-image", "gcr.io/cloud-builders/gcs-fetcher:latest",
		"The container image containing our GCS fetcher binary.")
)

// TODO(mattmoor): Should we move this somewhere common, because of the flag?
func gitToContainer(git *v1alpha1.GitSourceSpec) (*corev1.Container, error) {
	if git.Url == "" {
		return nil, newValidationError("MissingUrl", "git sources are expected to specify a Url, got: %v", git)
	}
	if git.Revision == "" {
		return nil, newValidationError("MissingRevision", "git sources are expected to specify a Revision, got: %v", git)
	}
	return &corev1.Container{
		Name:  initContainerPrefix + gitSource,
		Image: *gitImage,
		Args: []string{
			"-url", git.Url,
			"-revision", git.Revision,
		},
		VolumeMounts: implicitVolumeMounts,
		WorkingDir:   workspaceDir,
		Env:          implicitEnvVars,
	}, nil
}

func gcsToContainer(gcs *v1alpha1.GCSSourceSpec) (*corev1.Container, error) {
	if gcs.Location == "" {
		return nil, newValidationError("MissingLocation", "gcs sources are expected to specify a Location, got: %v", gcs)
	}
	return &corev1.Container{
		Name:         initContainerPrefix + gcsSource,
		Image:        *gcsFetcherImage,
		Args:         []string{"--type", string(gcs.Type), "--location", gcs.Location},
		VolumeMounts: implicitVolumeMounts,
		WorkingDir:   workspaceDir,
		Env:          implicitEnvVars,
	}, nil
}

func customToContainer(source *corev1.Container) (*corev1.Container, error) {
	if source.Name != "" {
		return nil, newValidationError("OmitName", "custom source containers are expected to omit Name, got: %v", source.Name)
	}
	custom := source.DeepCopy()
	custom.Name = customSource
	return custom, nil
}

func makeCredentialInitializer(build *v1alpha1.Build, kubeclient kubernetes.Interface) (*corev1.Container, []corev1.Volume, error) {
	serviceAccountName := build.Spec.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = "default"
	}

	sa, err := kubeclient.CoreV1().ServiceAccounts(build.Namespace).Get(serviceAccountName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}

	builders := []credentials.Builder{dockercreds.NewBuilder(), gitcreds.NewBuilder()}

	// Collect the volume declarations, there mounts into the cred-init container, and the arguments to it.
	volumes := []corev1.Volume{}
	volumeMounts := implicitVolumeMounts
	args := []string{}
	for _, secretEntry := range sa.Secrets {
		secret, err := kubeclient.CoreV1().Secrets(build.Namespace).Get(secretEntry.Name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}

		matched := false
		for _, b := range builders {
			if sa := b.MatchingAnnotations(secret); len(sa) > 0 {
				matched = true
				args = append(args, sa...)
			}
		}

		if matched {
			name := fmt.Sprintf("secret-volume-%s", secret.Name)
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      name,
				MountPath: credentials.VolumeName(secret.Name),
			})
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: secret.Name,
					},
				},
			})
		}
	}

	return &corev1.Container{
		Name:         initContainerPrefix + credsInit,
		Image:        *credsImage,
		Args:         args,
		VolumeMounts: volumeMounts,
		Env:          implicitEnvVars,
		WorkingDir:   workspaceDir,
	}, volumes, nil
}

// FromCRD converts a Build object to a Pod which implements the build specified
// by the supplied CRD.
func FromCRD(build *v1alpha1.Build, kubeclient kubernetes.Interface) (*corev1.Pod, error) {
	build = build.DeepCopy()

	cred, secrets, err := makeCredentialInitializer(build, kubeclient)
	if err != nil {
		return nil, err
	}

	initContainers := []corev1.Container{*cred}
	workspaceSubPath := ""
	if source := build.Spec.Source; source != nil {
		switch {
		case source.Git != nil:
			git, err := gitToContainer(source.Git)
			if err != nil {
				return nil, err
			}
			initContainers = append(initContainers, *git)
		case source.GCS != nil:
			gcs, err := gcsToContainer(source.GCS)
			if err != nil {
				return nil, err
			}
			initContainers = append(initContainers, *gcs)
		case source.Custom != nil:
			cust, err := customToContainer(source.Custom)
			if err != nil {
				return nil, err
			}
			// Prepend the custom container to the steps, to be
			// augmented later with env, volume mounts, etc.
			build.Spec.Steps = append([]corev1.Container{*cust}, build.Spec.Steps...)
		}

		workspaceSubPath = build.Spec.Source.SubPath
	}

	for i, step := range build.Spec.Steps {
		step.Env = append(implicitEnvVars, step.Env...)
		// TODO(mattmoor): Check that volumeMounts match volumes.

		// Add implicit volume mounts, unless the user has requested
		// their own volume mount at that path.
		requestedVolumeMounts := map[string]bool{}
		for _, vm := range step.VolumeMounts {
			requestedVolumeMounts[filepath.Clean(vm.MountPath)] = true
		}
		for _, imp := range implicitVolumeMounts {
			if !requestedVolumeMounts[filepath.Clean(imp.MountPath)] {
				// If the build's source specifies a subpath,
				// use that in the implicit workspace volume
				// mount.
				if workspaceSubPath != "" && imp.Name == "workspace" {
					imp.SubPath = workspaceSubPath
				}
				step.VolumeMounts = append(step.VolumeMounts, imp)
			}
		}

		if step.WorkingDir == "" {
			step.WorkingDir = workspaceDir
		}
		if step.Name == "" {
			step.Name = fmt.Sprintf("%v%d", unnamedInitContainerPrefix, i)
		} else {
			step.Name = fmt.Sprintf("%v%v", initContainerPrefix, step.Name)
		}

		initContainers = append(initContainers, step)
	}

	// Add our implicit volumes and any volumes needed for secrets to the explicitly
	// declared user volumes.
	volumes := append(build.Spec.Volumes, implicitVolumes...)
	volumes = append(volumes, secrets...)
	if err := v1alpha1.ValidateVolumes(volumes); err != nil {
		return nil, err
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// We execute the build's pod in the same namespace as where the build was
			// created so that it can access colocated resources.
			Namespace: build.Namespace,
			Name:      fmt.Sprintf("pod-for-%s", build.Name), // TODO: Use GenerateName.
			// Ensure our Pod gets a unique name.
			//GenerateName: fmt.Sprintf("%s-", build.Name),
			// If our parent Build is deleted, then we should be as well.
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(build, schema.GroupVersionKind{
					Group:   v1alpha1.SchemeGroupVersion.Group,
					Version: v1alpha1.SchemeGroupVersion.Version,
					Kind:    "Build",
				}),
			},
			Annotations: map[string]string{
				"sidecar.istio.io/inject": "false",
			},
			Labels: map[string]string{
				buildNameLabelKey: build.Name,
			},
		},
		Spec: corev1.PodSpec{
			// If the build fails, don't restart it.
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: initContainers,
			Containers: []corev1.Container{{
				Name:  "nop",
				Image: *nopImage,
			}},
			ServiceAccountName: build.Spec.ServiceAccountName,
			Volumes:            volumes,
			NodeSelector:       build.Spec.NodeSelector,
			Affinity:           build.Spec.Affinity,
		},
	}, nil
}

func isImplicitEnvVar(ev corev1.EnvVar) bool {
	for _, iev := range implicitEnvVars {
		if ev.Name == iev.Name {
			return true
		}
	}
	return false
}

func filterImplicitEnvVars(evs []corev1.EnvVar) []corev1.EnvVar {
	var envs []corev1.EnvVar
	for _, ev := range evs {
		if isImplicitEnvVar(ev) {
			continue
		}
		envs = append(envs, ev)
	}
	return envs
}

func isImplicitVolumeMount(vm corev1.VolumeMount) bool {
	for _, ivm := range implicitVolumeMounts {
		if vm.Name == ivm.Name {
			return true
		}
	}
	return false
}

func filterImplicitVolumeMounts(vms []corev1.VolumeMount) []corev1.VolumeMount {
	var volumes []corev1.VolumeMount
	for _, vm := range vms {
		if isImplicitVolumeMount(vm) {
			continue
		}
		volumes = append(volumes, vm)
	}
	return volumes
}

func isImplicitVolume(v corev1.Volume) bool {
	for _, iv := range implicitVolumes {
		if v.Name == iv.Name {
			return true
		}
	}
	if strings.HasPrefix(v.Name, "secret-volume-") {
		return true
	}
	return false
}

func filterImplicitVolumes(vs []corev1.Volume) []corev1.Volume {
	var volumes []corev1.Volume
	for _, v := range vs {
		if isImplicitVolume(v) {
			continue
		}
		volumes = append(volumes, v)
	}
	return volumes
}

// StatusFromPod returns a BuildStatus based on the status of the given Pod.
func StatusFromPod(pod *corev1.Pod) (*v1alpha1.BuildStatus, error) {
	status := &v1alpha1.BuildStatus{
		Builder: v1alpha1.ClusterBuildProvider,
		Cluster: &v1alpha1.ClusterSpec{
			Namespace: pod.Namespace,
			PodName:   pod.Name,
		},
	}

	if pod.Status.StartTime != nil {
		status.StartTime = *pod.Status.StartTime
	}

	for _, ics := range pod.Status.InitContainerStatuses {
		// Ignore statuses for implicit steps added by FromCRD (e.g., creds init, source fetching).
		if implicitStepNames[ics.Name] {
			continue
		}

		if ics.State.Terminated != nil {
			status.StepsCompleted = append(status.StepsCompleted, ics.Name)
		}
		status.StepStates = append(status.StepStates, ics.State)
	}

	switch pod.Status.Phase {
	case corev1.PodFailed:
		status.SetCondition(&duckv1alpha1.Condition{
			Type:    v1alpha1.BuildSucceeded,
			Status:  corev1.ConditionFalse,
			Message: getFailureMessage(pod),
		})
	case corev1.PodPending:
		status.SetCondition(&duckv1alpha1.Condition{
			Type:    v1alpha1.BuildSucceeded,
			Status:  corev1.ConditionUnknown,
			Message: "Pending",
			Reason:  getWaitingMessage(pod),
		})
	case corev1.PodSucceeded:
		status.SetCondition(&duckv1alpha1.Condition{
			Type:   v1alpha1.BuildSucceeded,
			Status: corev1.ConditionTrue,
		})
	default:
		status.SetCondition(&duckv1alpha1.Condition{
			Type:   v1alpha1.BuildSucceeded,
			Status: corev1.ConditionUnknown,
		})
	}

	return status, nil
}

func getWaitingMessage(pod *corev1.Pod) string {
	// First, try to surface reason for pending/unknown about the actual build step.
	for _, status := range pod.Status.InitContainerStatuses {
		wait := status.State.Waiting
		if wait != nil && wait.Message != "" {
			return fmt.Sprintf("build step %q is pending with reason %q",
				status.Name, wait.Message)
		}
	}
	// Try to surface underlying reason by inspecting pod's recent status if condition is not true
	for i, podStatus := range pod.Status.Conditions {
		if podStatus.Status != corev1.ConditionTrue {
			return fmt.Sprintf("pod status %q:%q; message: %q",
				pod.Status.Conditions[i].Type,
				pod.Status.Conditions[i].Status,
				pod.Status.Conditions[i].Message)
		}
	}
	// Next, return the Pod's status message if it has one.
	if pod.Status.Message != "" {
		return pod.Status.Message
	}

	// Lastly fall back on a generic pending message.
	return "Pending"
}

func getFailureMessage(pod *corev1.Pod) string {
	// First, try to surface an error about the actual build step that failed.
	for _, status := range pod.Status.InitContainerStatuses {
		term := status.State.Terminated
		if term != nil && term.ExitCode != 0 {
			return fmt.Sprintf("build step %q exited with code %d (image: %q); for logs run: kubectl -n %s logs %s -c %s",
				status.Name, term.ExitCode, status.ImageID,
				pod.Namespace, pod.Name, status.Name)
		}
	}
	// Next, return the Pod's status message if it has one.
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	// Lastly fall back on a generic error message.
	return "build failed for unspecified reasons."
}

type validationError struct {
	Reason  string
	Message string
}

func (ve *validationError) Error() string {
	return fmt.Sprintf("%s: %s", ve.Reason, ve.Message)
}

// validationError returns a new validation error.
func newValidationError(reason, format string, fmtArgs ...interface{}) error {
	return &validationError{
		Reason:  reason,
		Message: fmt.Sprintf(format, fmtArgs...),
	}
}
