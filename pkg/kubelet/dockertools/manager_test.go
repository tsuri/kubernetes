/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package dockertools

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/testapi"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/record"
	kubecontainer "github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/container"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/network"
	kubeprober "github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/prober"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/probe"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/types"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	uexec "github.com/GoogleCloudPlatform/kubernetes/pkg/util/exec"
	docker "github.com/fsouza/go-dockerclient"
)

type fakeHTTP struct {
	url string
	err error
}

func (f *fakeHTTP) Get(url string) (*http.Response, error) {
	f.url = url
	return nil, f.err
}

// TODO: Find a better way to mock the runtime hooks so that we don't have to
// duplicate the code here.
type fakeRuntimeHooks struct {
	recorder record.EventRecorder
}

var _ kubecontainer.RuntimeHooks = &fakeRuntimeHooks{}

func newFakeRuntimeHooks(recorder record.EventRecorder) kubecontainer.RuntimeHooks {
	return &fakeRuntimeHooks{
		recorder: recorder,
	}
}

func (fr *fakeRuntimeHooks) ShouldPullImage(pod *api.Pod, container *api.Container, imagePresent bool) bool {
	if container.ImagePullPolicy == api.PullNever {
		return false
	}
	if container.ImagePullPolicy == api.PullAlways ||
		(container.ImagePullPolicy == api.PullIfNotPresent && (!imagePresent)) {
		return true
	}

	return false
}

func (fr *fakeRuntimeHooks) ReportImagePull(pod *api.Pod, container *api.Container, pullError error) {
}

type fakeOptionGenerator struct{}

var _ kubecontainer.RunContainerOptionsGenerator = &fakeOptionGenerator{}

func (*fakeOptionGenerator) GenerateRunContainerOptions(pod *api.Pod, container *api.Container) (*kubecontainer.RunContainerOptions, error) {
	return &kubecontainer.RunContainerOptions{}, nil
}

func newTestDockerManagerWithHTTPClient(fakeHTTPClient *fakeHTTP) (*DockerManager, *FakeDockerClient) {
	fakeDocker := &FakeDockerClient{VersionInfo: docker.Env{"Version=1.1.3", "ApiVersion=1.15"}, Errors: make(map[string]error), RemovedImages: util.StringSet{}}
	fakeRecorder := &record.FakeRecorder{}
	readinessManager := kubecontainer.NewReadinessManager()
	containerRefManager := kubecontainer.NewRefManager()
	networkPlugin, _ := network.InitNetworkPlugin([]network.NetworkPlugin{}, "", network.NewFakeHost(nil))
	runtimeHooks := newFakeRuntimeHooks(fakeRecorder)
	optionGenerator := &fakeOptionGenerator{}
	dockerManager := NewFakeDockerManager(
		fakeDocker,
		fakeRecorder,
		readinessManager,
		containerRefManager,
		PodInfraContainerImage,
		0, 0, "",
		kubecontainer.FakeOS{},
		networkPlugin,
		optionGenerator,
		fakeHTTPClient,
		runtimeHooks)

	return dockerManager, fakeDocker
}

func newTestDockerManager() (*DockerManager, *FakeDockerClient) {
	return newTestDockerManagerWithHTTPClient(&fakeHTTP{})
}

func matchString(t *testing.T, pattern, str string) bool {
	match, err := regexp.MatchString(pattern, str)
	if err != nil {
		t.Logf("unexpected error: %v", err)
	}
	return match
}

func TestSetEntrypointAndCommand(t *testing.T) {
	cases := []struct {
		name      string
		container *api.Container
		envs      []kubecontainer.EnvVar
		expected  *docker.CreateContainerOptions
	}{
		{
			name:      "none",
			container: &api.Container{},
			expected: &docker.CreateContainerOptions{
				Config: &docker.Config{},
			},
		},
		{
			name: "command",
			container: &api.Container{
				Command: []string{"foo", "bar"},
			},
			expected: &docker.CreateContainerOptions{
				Config: &docker.Config{
					Entrypoint: []string{"foo", "bar"},
				},
			},
		},
		{
			name: "command expanded",
			container: &api.Container{
				Command: []string{"foo", "$(VAR_TEST)", "$(VAR_TEST2)"},
			},
			envs: []kubecontainer.EnvVar{
				{
					Name:  "VAR_TEST",
					Value: "zoo",
				},
				{
					Name:  "VAR_TEST2",
					Value: "boo",
				},
			},
			expected: &docker.CreateContainerOptions{
				Config: &docker.Config{
					Entrypoint: []string{"foo", "zoo", "boo"},
				},
			},
		},
		{
			name: "args",
			container: &api.Container{
				Args: []string{"foo", "bar"},
			},
			expected: &docker.CreateContainerOptions{
				Config: &docker.Config{
					Cmd: []string{"foo", "bar"},
				},
			},
		},
		{
			name: "args expanded",
			container: &api.Container{
				Args: []string{"zap", "$(VAR_TEST)", "$(VAR_TEST2)"},
			},
			envs: []kubecontainer.EnvVar{
				{
					Name:  "VAR_TEST",
					Value: "hap",
				},
				{
					Name:  "VAR_TEST2",
					Value: "trap",
				},
			},
			expected: &docker.CreateContainerOptions{
				Config: &docker.Config{
					Cmd: []string{"zap", "hap", "trap"},
				},
			},
		},
		{
			name: "both",
			container: &api.Container{
				Command: []string{"foo"},
				Args:    []string{"bar", "baz"},
			},
			expected: &docker.CreateContainerOptions{
				Config: &docker.Config{
					Entrypoint: []string{"foo"},
					Cmd:        []string{"bar", "baz"},
				},
			},
		},
		{
			name: "both expanded",
			container: &api.Container{
				Command: []string{"$(VAR_TEST2)--$(VAR_TEST)", "foo", "$(VAR_TEST3)"},
				Args:    []string{"foo", "$(VAR_TEST)", "$(VAR_TEST2)"},
			},
			envs: []kubecontainer.EnvVar{
				{
					Name:  "VAR_TEST",
					Value: "zoo",
				},
				{
					Name:  "VAR_TEST2",
					Value: "boo",
				},
				{
					Name:  "VAR_TEST3",
					Value: "roo",
				},
			},
			expected: &docker.CreateContainerOptions{
				Config: &docker.Config{
					Entrypoint: []string{"boo--zoo", "foo", "roo"},
					Cmd:        []string{"foo", "zoo", "boo"},
				},
			},
		},
	}

	for _, tc := range cases {
		opts := &kubecontainer.RunContainerOptions{
			Envs: tc.envs,
		}

		actualOpts := &docker.CreateContainerOptions{
			Config: &docker.Config{},
		}
		setEntrypointAndCommand(tc.container, opts, actualOpts)

		if e, a := tc.expected.Config.Entrypoint, actualOpts.Config.Entrypoint; !api.Semantic.DeepEqual(e, a) {
			t.Errorf("%v: unexpected entrypoint: expected %v, got %v", tc.name, e, a)
		}
		if e, a := tc.expected.Config.Cmd, actualOpts.Config.Cmd; !api.Semantic.DeepEqual(e, a) {
			t.Errorf("%v: unexpected command: expected %v, got %v", tc.name, e, a)
		}
	}
}

// verifyPods returns true if the two pod slices are equal.
func verifyPods(a, b []*kubecontainer.Pod) bool {
	if len(a) != len(b) {
		return false
	}

	// Sort the containers within a pod.
	for i := range a {
		sort.Sort(containersByID(a[i].Containers))
	}
	for i := range b {
		sort.Sort(containersByID(b[i].Containers))
	}

	// Sort the pods by UID.
	sort.Sort(podsByID(a))
	sort.Sort(podsByID(b))

	return reflect.DeepEqual(a, b)
}

func TestGetPods(t *testing.T) {
	manager, fakeDocker := newTestDockerManager()
	dockerContainers := []docker.APIContainers{
		{
			ID:    "1111",
			Names: []string{"/k8s_foo_qux_new_1234_42"},
		},
		{
			ID:    "2222",
			Names: []string{"/k8s_bar_qux_new_1234_42"},
		},
		{
			ID:    "3333",
			Names: []string{"/k8s_bar_jlk_wen_5678_42"},
		},
	}

	// Convert the docker containers. This does not affect the test coverage
	// because the conversion is tested separately in convert_test.go
	containers := make([]*kubecontainer.Container, len(dockerContainers))
	for i := range containers {
		c, err := toRuntimeContainer(&dockerContainers[i])
		if err != nil {
			t.Fatalf("unexpected error %v", err)
		}
		containers[i] = c
	}

	expected := []*kubecontainer.Pod{
		{
			ID:         types.UID("1234"),
			Name:       "qux",
			Namespace:  "new",
			Containers: []*kubecontainer.Container{containers[0], containers[1]},
		},
		{
			ID:         types.UID("5678"),
			Name:       "jlk",
			Namespace:  "wen",
			Containers: []*kubecontainer.Container{containers[2]},
		},
	}

	fakeDocker.ContainerList = dockerContainers
	actual, err := manager.GetPods(false)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if !verifyPods(expected, actual) {
		t.Errorf("expected %#v, got %#v", expected, actual)
	}
}

func TestListImages(t *testing.T) {
	manager, fakeDocker := newTestDockerManager()
	dockerImages := []docker.APIImages{{ID: "1111"}, {ID: "2222"}, {ID: "3333"}}
	expected := util.NewStringSet([]string{"1111", "2222", "3333"}...)

	fakeDocker.Images = dockerImages
	actualImages, err := manager.ListImages()
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	actual := util.NewStringSet()
	for _, i := range actualImages {
		actual.Insert(i.ID)
	}
	// We can compare the two sets directly because util.StringSet.List()
	// returns a "sorted" list.
	if !reflect.DeepEqual(expected.List(), actual.List()) {
		t.Errorf("expected %#v, got %#v", expected.List(), actual.List())
	}
}

func apiContainerToContainer(c docker.APIContainers) kubecontainer.Container {
	dockerName, hash, err := ParseDockerName(c.Names[0])
	if err != nil {
		return kubecontainer.Container{}
	}
	return kubecontainer.Container{
		ID:   types.UID(c.ID),
		Name: dockerName.ContainerName,
		Hash: hash,
	}
}

func dockerContainersToPod(containers DockerContainers) kubecontainer.Pod {
	var pod kubecontainer.Pod
	for _, c := range containers {
		dockerName, hash, err := ParseDockerName(c.Names[0])
		if err != nil {
			continue
		}
		pod.Containers = append(pod.Containers, &kubecontainer.Container{
			ID:    types.UID(c.ID),
			Name:  dockerName.ContainerName,
			Hash:  hash,
			Image: c.Image,
		})
		// TODO(yifan): Only one evaluation is enough.
		pod.ID = dockerName.PodUID
		name, namespace, _ := kubecontainer.ParsePodFullName(dockerName.PodFullName)
		pod.Name = name
		pod.Namespace = namespace
	}
	return pod
}

func TestKillContainerInPod(t *testing.T) {
	manager, fakeDocker := newTestDockerManager()

	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "qux",
			Namespace: "new",
		},
		Spec: api.PodSpec{Containers: []api.Container{{Name: "foo"}, {Name: "bar"}}},
	}
	containers := []docker.APIContainers{
		{
			ID:    "1111",
			Names: []string{"/k8s_foo_qux_new_1234_42"},
		},
		{
			ID:    "2222",
			Names: []string{"/k8s_bar_qux_new_1234_42"},
		},
	}
	containerToKill := &containers[0]
	containerToSpare := &containers[1]
	fakeDocker.ContainerList = containers
	// Set all containers to ready.
	for _, c := range fakeDocker.ContainerList {
		manager.readinessManager.SetReadiness(c.ID, true)
	}

	if err := manager.KillContainerInPod(pod.Spec.Containers[0], pod); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// Assert the container has been stopped.
	if err := fakeDocker.AssertStopped([]string{containerToKill.ID}); err != nil {
		t.Errorf("container was not stopped correctly: %v", err)
	}

	// Verify that the readiness has been removed for the stopped container.
	if ready := manager.readinessManager.GetReadiness(containerToKill.ID); ready {
		t.Errorf("exepcted container entry ID '%v' to not be found. states: %+v", containerToKill.ID, ready)
	}
	if ready := manager.readinessManager.GetReadiness(containerToSpare.ID); !ready {
		t.Errorf("exepcted container entry ID '%v' to be found. states: %+v", containerToSpare.ID, ready)
	}
}

func TestKillContainerInPodWithPreStop(t *testing.T) {
	manager, fakeDocker := newTestDockerManager()
	fakeDocker.ExecInspect = &docker.ExecInspect{
		Running:  false,
		ExitCode: 0,
	}
	expectedCmd := []string{"foo.sh", "bar"}
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "qux",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{
					Name: "foo",
					Lifecycle: &api.Lifecycle{
						PreStop: &api.Handler{
							Exec: &api.ExecAction{
								Command: expectedCmd,
							},
						},
					},
				},
				{Name: "bar"}}},
	}
	podString, err := testapi.Codec().Encode(pod)
	if err != nil {
		t.Errorf("unexpected error: %v")
	}
	containers := []docker.APIContainers{
		{
			ID:    "1111",
			Names: []string{"/k8s_foo_qux_new_1234_42"},
		},
		{
			ID:    "2222",
			Names: []string{"/k8s_bar_qux_new_1234_42"},
		},
	}
	containerToKill := &containers[0]
	fakeDocker.ContainerList = containers
	fakeDocker.Container = &docker.Container{
		Config: &docker.Config{
			Labels: map[string]string{
				kubernetesPodLabel:       string(podString),
				kubernetesContainerLabel: "foo",
			},
		},
	}
	// Set all containers to ready.
	for _, c := range fakeDocker.ContainerList {
		manager.readinessManager.SetReadiness(c.ID, true)
	}

	if err := manager.KillContainerInPod(pod.Spec.Containers[0], pod); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// Assert the container has been stopped.
	if err := fakeDocker.AssertStopped([]string{containerToKill.ID}); err != nil {
		t.Errorf("container was not stopped correctly: %v", err)
	}
	verifyCalls(t, fakeDocker, []string{"list", "inspect_container", "create_exec", "start_exec", "stop"})
	if !reflect.DeepEqual(expectedCmd, fakeDocker.execCmd) {
		t.Errorf("expected: %v, got %v", expectedCmd, fakeDocker.execCmd)
	}
}

func TestKillContainerInPodWithError(t *testing.T) {
	manager, fakeDocker := newTestDockerManager()

	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "qux",
			Namespace: "new",
		},
		Spec: api.PodSpec{Containers: []api.Container{{Name: "foo"}, {Name: "bar"}}},
	}
	containers := []docker.APIContainers{
		{
			ID:    "1111",
			Names: []string{"/k8s_foo_qux_new_1234_42"},
		},
		{
			ID:    "2222",
			Names: []string{"/k8s_bar_qux_new_1234_42"},
		},
	}
	containerToKill := &containers[0]
	containerToSpare := &containers[1]
	fakeDocker.ContainerList = containers
	fakeDocker.Errors["stop"] = fmt.Errorf("sample error")

	// Set all containers to ready.
	for _, c := range fakeDocker.ContainerList {
		manager.readinessManager.SetReadiness(c.ID, true)
	}

	if err := manager.KillContainerInPod(pod.Spec.Containers[0], pod); err == nil {
		t.Errorf("expected error, found nil")
	}

	// Verify that the readiness has been removed even though the stop failed.
	if ready := manager.readinessManager.GetReadiness(containerToKill.ID); ready {
		t.Errorf("exepcted container entry ID '%v' to not be found. states: %+v", containerToKill.ID, ready)
	}
	if ready := manager.readinessManager.GetReadiness(containerToSpare.ID); !ready {
		t.Errorf("exepcted container entry ID '%v' to be found. states: %+v", containerToSpare.ID, ready)
	}
}

type fakeExecProber struct {
	result probe.Result
	output string
	err    error
}

func (p fakeExecProber) Probe(_ uexec.Cmd) (probe.Result, string, error) {
	return p.result, p.output, p.err
}

func replaceProber(dm *DockerManager, result probe.Result, err error) {
	fakeExec := fakeExecProber{
		result: result,
		err:    err,
	}

	dm.prober = kubeprober.NewTestProber(fakeExec, dm.readinessManager, dm.containerRefManager, &record.FakeRecorder{})
	return
}

// TestProbeContainer tests the functionality of probeContainer.
// Test cases are:
//
// No probe.
// Only LivenessProbe.
// Only ReadinessProbe.
// Both probes.
//
// Also, for each probe, there will be several cases covering whether the initial
// delay has passed, whether the probe handler will return Success, Failure,
// Unknown or error.
//
// PLEASE READ THE PROBE DOCS BEFORE CHANGING THIS TEST IF YOU ARE UNSURE HOW PROBES ARE SUPPOSED TO WORK:
// (See https://github.com/GoogleCloudPlatform/kubernetes/blob/master/docs/pod-states.md#pod-conditions)
func TestProbeContainer(t *testing.T) {
	manager, _ := newTestDockerManager()
	dc := &docker.APIContainers{
		ID:      "foobar",
		Created: time.Now().Unix(),
	}
	tests := []struct {
		testContainer     api.Container
		expectError       bool
		expectedResult    probe.Result
		expectedReadiness bool
	}{
		// No probes.
		{
			testContainer:     api.Container{},
			expectedResult:    probe.Success,
			expectedReadiness: true,
		},
		// Only LivenessProbe. expectedReadiness should always be true here.
		{
			testContainer: api.Container{
				LivenessProbe: &api.Probe{InitialDelaySeconds: 100},
			},
			expectedResult:    probe.Success,
			expectedReadiness: true,
		},
		{
			testContainer: api.Container{
				LivenessProbe: &api.Probe{InitialDelaySeconds: -100},
			},
			expectedResult:    probe.Unknown,
			expectedReadiness: true,
		},
		{
			testContainer: api.Container{
				LivenessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
			},
			expectedResult:    probe.Failure,
			expectedReadiness: true,
		},
		{
			testContainer: api.Container{
				LivenessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
			},
			expectedResult:    probe.Success,
			expectedReadiness: true,
		},
		{
			testContainer: api.Container{
				LivenessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
			},
			expectedResult:    probe.Unknown,
			expectedReadiness: true,
		},
		{
			testContainer: api.Container{
				LivenessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
			},
			expectError:       true,
			expectedResult:    probe.Unknown,
			expectedReadiness: true,
		},
		// // Only ReadinessProbe. expectedResult should always be probe.Success here.
		{
			testContainer: api.Container{
				ReadinessProbe: &api.Probe{InitialDelaySeconds: 100},
			},
			expectedResult:    probe.Success,
			expectedReadiness: false,
		},
		{
			testContainer: api.Container{
				ReadinessProbe: &api.Probe{InitialDelaySeconds: -100},
			},
			expectedResult:    probe.Success,
			expectedReadiness: false,
		},
		{
			testContainer: api.Container{
				ReadinessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
			},
			expectedResult:    probe.Success,
			expectedReadiness: true,
		},
		{
			testContainer: api.Container{
				ReadinessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
			},
			expectedResult:    probe.Success,
			expectedReadiness: true,
		},
		{
			testContainer: api.Container{
				ReadinessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
			},
			expectedResult:    probe.Success,
			expectedReadiness: true,
		},
		{
			testContainer: api.Container{
				ReadinessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
			},
			expectError:       false,
			expectedResult:    probe.Success,
			expectedReadiness: true,
		},
		// Both LivenessProbe and ReadinessProbe.
		{
			testContainer: api.Container{
				LivenessProbe:  &api.Probe{InitialDelaySeconds: 100},
				ReadinessProbe: &api.Probe{InitialDelaySeconds: 100},
			},
			expectedResult:    probe.Success,
			expectedReadiness: false,
		},
		{
			testContainer: api.Container{
				LivenessProbe:  &api.Probe{InitialDelaySeconds: 100},
				ReadinessProbe: &api.Probe{InitialDelaySeconds: -100},
			},
			expectedResult:    probe.Success,
			expectedReadiness: false,
		},
		{
			testContainer: api.Container{
				LivenessProbe:  &api.Probe{InitialDelaySeconds: -100},
				ReadinessProbe: &api.Probe{InitialDelaySeconds: 100},
			},
			expectedResult:    probe.Unknown,
			expectedReadiness: false,
		},
		{
			testContainer: api.Container{
				LivenessProbe:  &api.Probe{InitialDelaySeconds: -100},
				ReadinessProbe: &api.Probe{InitialDelaySeconds: -100},
			},
			expectedResult:    probe.Unknown,
			expectedReadiness: false,
		},
		{
			testContainer: api.Container{
				LivenessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
				ReadinessProbe: &api.Probe{InitialDelaySeconds: -100},
			},
			expectedResult:    probe.Unknown,
			expectedReadiness: false,
		},
		{
			testContainer: api.Container{
				LivenessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
				ReadinessProbe: &api.Probe{InitialDelaySeconds: -100},
			},
			expectedResult:    probe.Failure,
			expectedReadiness: false,
		},
		{
			testContainer: api.Container{
				LivenessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
				ReadinessProbe: &api.Probe{
					InitialDelaySeconds: -100,
					Handler: api.Handler{
						Exec: &api.ExecAction{},
					},
				},
			},
			expectedResult:    probe.Success,
			expectedReadiness: true,
		},
	}

	for i, test := range tests {
		if test.expectError {
			replaceProber(manager, test.expectedResult, errors.New("error"))
		} else {
			replaceProber(manager, test.expectedResult, nil)
		}
		result, err := manager.prober.Probe(&api.Pod{}, api.PodStatus{}, test.testContainer, dc.ID, dc.Created)
		if test.expectError && err == nil {
			t.Error("[%d] Expected error but no error was returned.", i)
		}
		if !test.expectError && err != nil {
			t.Errorf("[%d] Didn't expect error but got: %v", i, err)
		}
		if test.expectedResult != result {
			t.Errorf("[%d] Expected result to be %v but was %v", i, test.expectedResult, result)
		}
		if test.expectedReadiness != manager.readinessManager.GetReadiness(dc.ID) {
			t.Errorf("[%d] Expected readiness to be %v but was %v", i, test.expectedReadiness, manager.readinessManager.GetReadiness(dc.ID))
		}
	}
}

func TestIsAExitError(t *testing.T) {
	var err error
	err = &dockerExitError{nil}
	_, ok := err.(uexec.ExitError)
	if !ok {
		t.Error("couldn't cast dockerExitError to exec.ExitError")
	}
}

func generatePodInfraContainerHash(pod *api.Pod) uint64 {
	var ports []api.ContainerPort
	if !pod.Spec.HostNetwork {
		for _, container := range pod.Spec.Containers {
			ports = append(ports, container.Ports...)
		}
	}

	container := &api.Container{
		Name:  PodInfraContainerName,
		Image: PodInfraContainerImage,
		Ports: ports,
	}
	return kubecontainer.HashContainer(container)
}

// runSyncPod is a helper function to retrieve the running pods from the fake
// docker client and runs SyncPod for the given pod.
func runSyncPod(t *testing.T, dm *DockerManager, fakeDocker *FakeDockerClient, pod *api.Pod) {
	runningPods, err := dm.GetPods(false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runningPod := kubecontainer.Pods(runningPods).FindPodByID(pod.UID)
	podStatus, err := dm.GetPodStatus(pod)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	fakeDocker.ClearCalls()
	err = dm.SyncPod(pod, runningPod, *podStatus, []api.Secret{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSyncPodCreateNetAndContainer(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	dm.podInfraContainerImage = "custom_image_name"
	fakeDocker.ContainerList = []docker.APIContainers{}
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "bar"},
			},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)
	verifyCalls(t, fakeDocker, []string{
		// Create pod infra container.
		"create", "start", "inspect_container",
		// Create container.
		"create", "start",
	})

	fakeDocker.Lock()

	found := false
	for _, c := range fakeDocker.ContainerList {
		if c.Image == "custom_image_name" && strings.HasPrefix(c.Names[0], "/k8s_POD") {
			found = true
		}
	}
	if !found {
		t.Errorf("Custom pod infra container not found: %v", fakeDocker.ContainerList)
	}

	if len(fakeDocker.Created) != 2 ||
		!matchString(t, "k8s_POD\\.[a-f0-9]+_foo_new_", fakeDocker.Created[0]) ||
		!matchString(t, "k8s_bar\\.[a-f0-9]+_foo_new_", fakeDocker.Created[1]) {
		t.Errorf("Unexpected containers created %v", fakeDocker.Created)
	}
	fakeDocker.Unlock()
}

func TestSyncPodCreatesNetAndContainerPullsImage(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	dm.podInfraContainerImage = "custom_image_name"
	puller := dm.puller.(*FakeDockerPuller)
	puller.HasImages = []string{}
	dm.podInfraContainerImage = "custom_image_name"
	fakeDocker.ContainerList = []docker.APIContainers{}
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "bar", Image: "something", ImagePullPolicy: "IfNotPresent"},
			},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	verifyCalls(t, fakeDocker, []string{
		// Create pod infra container.
		"create", "start", "inspect_container",
		// Create container.
		"create", "start",
	})

	fakeDocker.Lock()

	if !reflect.DeepEqual(puller.ImagesPulled, []string{"custom_image_name", "something"}) {
		t.Errorf("Unexpected pulled containers: %v", puller.ImagesPulled)
	}

	if len(fakeDocker.Created) != 2 ||
		!matchString(t, "k8s_POD\\.[a-f0-9]+_foo_new_", fakeDocker.Created[0]) ||
		!matchString(t, "k8s_bar\\.[a-f0-9]+_foo_new_", fakeDocker.Created[1]) {
		t.Errorf("Unexpected containers created %v", fakeDocker.Created)
	}
	fakeDocker.Unlock()
}

func TestSyncPodWithPodInfraCreatesContainer(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "bar"},
			},
		},
	}
	fakeDocker.ContainerList = []docker.APIContainers{
		{
			// pod infra container
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_0"},
			ID:    "9876",
		},
	}
	fakeDocker.ContainerMap = map[string]*docker.Container{
		"9876": {
			ID:         "9876",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	verifyCalls(t, fakeDocker, []string{
		// Inspect pod infra container (but does not create)"
		"inspect_container",
		// Create container.
		"create", "start",
	})

	fakeDocker.Lock()
	if len(fakeDocker.Created) != 1 ||
		!matchString(t, "k8s_bar\\.[a-f0-9]+_foo_new_", fakeDocker.Created[0]) {
		t.Errorf("Unexpected containers created %v", fakeDocker.Created)
	}
	fakeDocker.Unlock()
}

func TestSyncPodDeletesWithNoPodInfraContainer(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo1",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "bar1"},
			},
		},
	}
	fakeDocker.ContainerList = []docker.APIContainers{
		{
			// format is // k8s_<container-id>_<pod-fullname>_<pod-uid>
			Names: []string{"/k8s_bar1_foo1_new_12345678_0"},
			ID:    "1234",
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	verifyCalls(t, fakeDocker, []string{
		// Kill the container since pod infra container is not running.
		"inspect_container", "stop",
		// Create pod infra container.
		"create", "start", "inspect_container",
		// Create container.
		"create", "start",
	})

	// A map iteration is used to delete containers, so must not depend on
	// order here.
	expectedToStop := map[string]bool{
		"1234": true,
	}
	fakeDocker.Lock()
	if len(fakeDocker.Stopped) != 1 || !expectedToStop[fakeDocker.Stopped[0]] {
		t.Errorf("Wrong containers were stopped: %v", fakeDocker.Stopped)
	}
	fakeDocker.Unlock()
}

func TestSyncPodDeletesDuplicate(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "bar",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "foo"},
			},
		},
	}

	fakeDocker.ContainerList = []docker.APIContainers{
		{
			// the k8s prefix is required for the kubelet to manage the container
			Names: []string{"/k8s_foo_bar_new_12345678_1111"},
			ID:    "1234",
		},
		{
			// pod infra container
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_bar_new_12345678_2222"},
			ID:    "9876",
		},
		{
			// Duplicate for the same container.
			Names: []string{"/k8s_foo_bar_new_12345678_3333"},
			ID:    "4567",
		},
	}
	fakeDocker.ContainerMap = map[string]*docker.Container{
		"1234": {
			ID:         "1234",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
		"9876": {
			ID:         "9876",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
		"4567": {
			ID:         "4567",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	verifyCalls(t, fakeDocker, []string{
		// Check the pod infra container.
		"inspect_container",
		// Kill the duplicated container.
		"inspect_container", "stop",
	})
	// Expect one of the duplicates to be killed.
	if len(fakeDocker.Stopped) != 1 || (fakeDocker.Stopped[0] != "1234" && fakeDocker.Stopped[0] != "4567") {
		t.Errorf("Wrong containers were stopped: %v", fakeDocker.Stopped)
	}
}

func TestSyncPodBadHash(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "bar"},
			},
		},
	}

	fakeDocker.ContainerList = []docker.APIContainers{
		{
			// the k8s prefix is required for the kubelet to manage the container
			Names: []string{"/k8s_bar.1234_foo_new_12345678_42"},
			ID:    "1234",
		},
		{
			// pod infra container
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_42"},
			ID:    "9876",
		},
	}
	fakeDocker.ContainerMap = map[string]*docker.Container{
		"1234": {
			ID:         "1234",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
		"9876": {
			ID:         "9876",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	verifyCalls(t, fakeDocker, []string{
		// Check the pod infra container.
		"inspect_container",
		// Kill and restart the bad hash container.
		"inspect_container", "stop", "create", "start",
	})

	if err := fakeDocker.AssertStopped([]string{"1234"}); err != nil {
		t.Errorf("%v", err)
	}
}

func TestSyncPodsUnhealthy(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "bar",
					LivenessProbe: &api.Probe{
					// Always returns healthy == false
					},
				},
			},
		},
	}

	fakeDocker.ContainerList = []docker.APIContainers{
		{
			// the k8s prefix is required for the kubelet to manage the container
			Names: []string{"/k8s_bar_foo_new_12345678_42"},
			ID:    "1234",
		},
		{
			// pod infra container
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_42"},
			ID:    "9876",
		},
	}
	fakeDocker.ContainerMap = map[string]*docker.Container{
		"1234": {
			ID:         "1234",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
		"9876": {
			ID:         "9876",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	verifyCalls(t, fakeDocker, []string{
		// Check the pod infra container.
		"inspect_container",
		// Kill the unhealthy container.
		"inspect_container", "stop",
		// Restart the unhealthy container.
		"create", "start",
	})

	if err := fakeDocker.AssertStopped([]string{"1234"}); err != nil {
		t.Errorf("%v", err)
	}
}

func TestSyncPodsDoesNothing(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	container := api.Container{Name: "bar"}
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				container,
			},
		},
	}

	fakeDocker.ContainerList = []docker.APIContainers{
		{
			// format is // k8s_<container-id>_<pod-fullname>_<pod-uid>_<random>
			Names: []string{"/k8s_bar." + strconv.FormatUint(kubecontainer.HashContainer(&container), 16) + "_foo_new_12345678_0"},
			ID:    "1234",
		},
		{
			// pod infra container
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_0"},
			ID:    "9876",
		},
	}
	fakeDocker.ContainerMap = map[string]*docker.Container{
		"1234": {
			ID:         "1234",
			HostConfig: &docker.HostConfig{},
			Config:     &docker.Config{},
		},
		"9876": {
			ID:         "9876",
			HostConfig: &docker.HostConfig{},
			Config:     &docker.Config{},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	verifyCalls(t, fakeDocker, []string{
		// Check the pod infra contianer.
		"inspect_container",
	})
}

func TestSyncPodWithPullPolicy(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	puller := dm.puller.(*FakeDockerPuller)
	puller.HasImages = []string{"existing_one", "want:latest"}
	dm.podInfraContainerImage = "custom_image_name"
	fakeDocker.ContainerList = []docker.APIContainers{}

	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "bar", Image: "pull_always_image", ImagePullPolicy: api.PullAlways},
				{Name: "bar1", Image: "pull_never_image", ImagePullPolicy: api.PullNever},
				{Name: "bar2", Image: "pull_if_not_present_image", ImagePullPolicy: api.PullIfNotPresent},
				{Name: "bar3", Image: "existing_one", ImagePullPolicy: api.PullIfNotPresent},
				{Name: "bar4", Image: "want:latest", ImagePullPolicy: api.PullIfNotPresent},
			},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	fakeDocker.Lock()

	pulledImageSet := make(map[string]empty)
	for v := range puller.ImagesPulled {
		pulledImageSet[puller.ImagesPulled[v]] = empty{}
	}

	if !reflect.DeepEqual(pulledImageSet, map[string]empty{
		"custom_image_name":         {},
		"pull_always_image":         {},
		"pull_if_not_present_image": {},
	}) {
		t.Errorf("Unexpected pulled containers: %v", puller.ImagesPulled)
	}

	if len(fakeDocker.Created) != 6 {
		t.Errorf("Unexpected containers created %v", fakeDocker.Created)
	}
	fakeDocker.Unlock()
}

func TestSyncPodWithRestartPolicy(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	containers := []api.Container{
		{Name: "succeeded"},
		{Name: "failed"},
	}
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: containers,
		},
	}

	runningAPIContainers := []docker.APIContainers{
		{
			// pod infra container
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_0"},
			ID:    "9876",
		},
	}
	exitedAPIContainers := []docker.APIContainers{
		{
			// format is // k8s_<container-id>_<pod-fullname>_<pod-uid>
			Names: []string{"/k8s_succeeded." + strconv.FormatUint(kubecontainer.HashContainer(&containers[0]), 16) + "_foo_new_12345678_0"},
			ID:    "1234",
		},
		{
			// format is // k8s_<container-id>_<pod-fullname>_<pod-uid>
			Names: []string{"/k8s_failed." + strconv.FormatUint(kubecontainer.HashContainer(&containers[1]), 16) + "_foo_new_12345678_0"},
			ID:    "5678",
		},
	}

	containerMap := map[string]*docker.Container{
		"9876": {
			ID:     "9876",
			Name:   "POD",
			Config: &docker.Config{},
			State: docker.State{
				StartedAt: time.Now(),
				Running:   true,
			},
		},
		"1234": {
			ID:     "1234",
			Name:   "succeeded",
			Config: &docker.Config{},
			State: docker.State{
				ExitCode:   0,
				StartedAt:  time.Now(),
				FinishedAt: time.Now(),
			},
		},
		"5678": {
			ID:     "5678",
			Name:   "failed",
			Config: &docker.Config{},
			State: docker.State{
				ExitCode:   42,
				StartedAt:  time.Now(),
				FinishedAt: time.Now(),
			},
		},
	}

	tests := []struct {
		policy  api.RestartPolicy
		calls   []string
		created []string
		stopped []string
	}{
		{
			api.RestartPolicyAlways,
			[]string{
				// Check the pod infra container.
				"inspect_container",
				// Restart both containers.
				"create", "start", "create", "start",
			},
			[]string{"succeeded", "failed"},
			[]string{},
		},
		{
			api.RestartPolicyOnFailure,
			[]string{
				// Check the pod infra container.
				"inspect_container",
				// Restart the failed container.
				"create", "start",
			},
			[]string{"failed"},
			[]string{},
		},
		{
			api.RestartPolicyNever,
			[]string{
				// Check the pod infra container.
				"inspect_container",
				// Stop the last pod infra container.
				"inspect_container", "stop",
			},
			[]string{},
			[]string{"9876"},
		},
	}

	for i, tt := range tests {
		fakeDocker.ContainerList = runningAPIContainers
		fakeDocker.ExitedContainerList = exitedAPIContainers
		fakeDocker.ContainerMap = containerMap
		pod.Spec.RestartPolicy = tt.policy

		runSyncPod(t, dm, fakeDocker, pod)

		// 'stop' is because the pod infra container is killed when no container is running.
		verifyCalls(t, fakeDocker, tt.calls)

		if err := fakeDocker.AssertCreated(tt.created); err != nil {
			t.Errorf("%d: %v", i, err)
		}
		if err := fakeDocker.AssertStopped(tt.stopped); err != nil {
			t.Errorf("%d: %v", i, err)
		}
	}
}

func TestGetPodStatusWithLastTermination(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	containers := []api.Container{
		{Name: "succeeded"},
		{Name: "failed"},
	}

	exitedAPIContainers := []docker.APIContainers{
		{
			// format is // k8s_<container-id>_<pod-fullname>_<pod-uid>
			Names: []string{"/k8s_succeeded." + strconv.FormatUint(kubecontainer.HashContainer(&containers[0]), 16) + "_foo_new_12345678_0"},
			ID:    "1234",
		},
		{
			// format is // k8s_<container-id>_<pod-fullname>_<pod-uid>
			Names: []string{"/k8s_failed." + strconv.FormatUint(kubecontainer.HashContainer(&containers[1]), 16) + "_foo_new_12345678_0"},
			ID:    "5678",
		},
	}

	containerMap := map[string]*docker.Container{
		"9876": {
			ID:         "9876",
			Name:       "POD",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
			State: docker.State{
				StartedAt:  time.Now(),
				FinishedAt: time.Now(),
				Running:    true,
			},
		},
		"1234": {
			ID:         "1234",
			Name:       "succeeded",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
			State: docker.State{
				ExitCode:   0,
				StartedAt:  time.Now(),
				FinishedAt: time.Now(),
			},
		},
		"5678": {
			ID:         "5678",
			Name:       "failed",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
			State: docker.State{
				ExitCode:   42,
				StartedAt:  time.Now(),
				FinishedAt: time.Now(),
			},
		},
	}

	tests := []struct {
		policy           api.RestartPolicy
		created          []string
		stopped          []string
		lastTerminations []string
	}{
		{
			api.RestartPolicyAlways,
			[]string{"succeeded", "failed"},
			[]string{},
			[]string{"docker://1234", "docker://5678"},
		},
		{
			api.RestartPolicyOnFailure,
			[]string{"failed"},
			[]string{},
			[]string{"docker://5678"},
		},
		{
			api.RestartPolicyNever,
			[]string{},
			[]string{"9876"},
			[]string{},
		},
	}

	for i, tt := range tests {
		fakeDocker.ExitedContainerList = exitedAPIContainers
		fakeDocker.ContainerMap = containerMap
		fakeDocker.ClearCalls()
		pod := &api.Pod{
			ObjectMeta: api.ObjectMeta{
				UID:       "12345678",
				Name:      "foo",
				Namespace: "new",
			},
			Spec: api.PodSpec{
				Containers:    containers,
				RestartPolicy: tt.policy,
			},
		}
		fakeDocker.ContainerList = []docker.APIContainers{
			{
				// pod infra container
				Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_0"},
				ID:    "9876",
			},
		}

		runSyncPod(t, dm, fakeDocker, pod)

		// Check if we can retrieve the pod status.
		status, err := dm.GetPodStatus(pod)
		if err != nil {
			t.Fatalf("unexpected error %v", err)
		}
		terminatedContainers := []string{}
		for _, cs := range status.ContainerStatuses {
			if cs.LastTerminationState.Terminated != nil {
				terminatedContainers = append(terminatedContainers, cs.LastTerminationState.Terminated.ContainerID)
			}
		}
		sort.StringSlice(terminatedContainers).Sort()
		sort.StringSlice(tt.lastTerminations).Sort()
		if !reflect.DeepEqual(terminatedContainers, tt.lastTerminations) {
			t.Errorf("Expected(sorted): %#v, Actual(sorted): %#v", tt.lastTerminations, terminatedContainers)
		}

		if err := fakeDocker.AssertCreated(tt.created); err != nil {
			t.Errorf("%d: %v", i, err)
		}
		if err := fakeDocker.AssertStopped(tt.stopped); err != nil {
			t.Errorf("%d: %v", i, err)
		}
	}
}

func TestGetPodCreationFailureReason(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()

	// Inject the creation failure error to docker.
	failureReason := "creation failure"
	fakeDocker.Errors = map[string]error{
		"create": fmt.Errorf("%s", failureReason),
	}

	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{{Name: "bar"}},
		},
	}

	// Pretend that the pod infra container has already been created, so that
	// we can run the user containers.
	fakeDocker.ContainerList = []docker.APIContainers{
		{
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_0"},
			ID:    "9876",
		},
	}
	fakeDocker.ContainerMap = map[string]*docker.Container{
		"9876": {
			ID:         "9876",
			HostConfig: &docker.HostConfig{},
			Config:     &docker.Config{},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)
	// Check if we can retrieve the pod status.
	status, err := dm.GetPodStatus(pod)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	if len(status.ContainerStatuses) < 1 {
		t.Errorf("expected 1 container status, got %d", len(status.ContainerStatuses))
	} else {
		state := status.ContainerStatuses[0].State
		if state.Waiting == nil {
			t.Errorf("expected waiting state, got %#v", state)
		} else if state.Waiting.Reason != failureReason {
			t.Errorf("expected reason %q, got %q", failureReason, state.Waiting.Reason)
		}
	}
}

func TestGetPodPullImageFailureReason(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	// Initialize the FakeDockerPuller so that it'd try to pull non-existent
	// images.
	puller := dm.puller.(*FakeDockerPuller)
	puller.HasImages = []string{}
	// Inject the pull image failure error.
	failureReason := "pull image faiulre"
	puller.ErrorsToInject = []error{fmt.Errorf("%s", failureReason)}

	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{{Name: "bar", Image: "realImage", ImagePullPolicy: api.PullAlways}},
		},
	}

	// Pretend that the pod infra container has already been created, so that
	// we can run the user containers.
	fakeDocker.ContainerList = []docker.APIContainers{
		{
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_0"},
			ID:    "9876",
		},
	}
	fakeDocker.ContainerMap = map[string]*docker.Container{
		"9876": {
			ID:         "9876",
			HostConfig: &docker.HostConfig{},
			Config:     &docker.Config{},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)
	// Check if we can retrieve the pod status.
	status, err := dm.GetPodStatus(pod)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	if len(status.ContainerStatuses) < 1 {
		t.Errorf("expected 1 container status, got %d", len(status.ContainerStatuses))
	} else {
		state := status.ContainerStatuses[0].State
		if state.Waiting == nil {
			t.Errorf("expected waiting state, got %#v", state)
		} else if state.Waiting.Reason != failureReason {
			t.Errorf("expected reason %q, got %q", failureReason, state.Waiting.Reason)
		}
	}
}

func TestGetRestartCount(t *testing.T) {
	dm, fakeDocker := newTestDockerManager()
	containers := []api.Container{
		{Name: "bar"},
	}
	pod := api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: containers,
		},
	}

	// format is // k8s_<container-id>_<pod-fullname>_<pod-uid>
	names := []string{"/k8s_bar." + strconv.FormatUint(kubecontainer.HashContainer(&containers[0]), 16) + "_foo_new_12345678_0"}
	currTime := time.Now()
	containerMap := map[string]*docker.Container{
		"1234": {
			ID:     "1234",
			Name:   "bar",
			Config: &docker.Config{},
			State: docker.State{
				ExitCode:   42,
				StartedAt:  currTime.Add(-60 * time.Second),
				FinishedAt: currTime.Add(-60 * time.Second),
			},
		},
		"5678": {
			ID:     "5678",
			Name:   "bar",
			Config: &docker.Config{},
			State: docker.State{
				ExitCode:   42,
				StartedAt:  currTime.Add(-30 * time.Second),
				FinishedAt: currTime.Add(-30 * time.Second),
			},
		},
		"9101": {
			ID:     "9101",
			Name:   "bar",
			Config: &docker.Config{},
			State: docker.State{
				ExitCode:   42,
				StartedAt:  currTime.Add(30 * time.Minute),
				FinishedAt: currTime.Add(30 * time.Minute),
			},
		},
	}
	fakeDocker.ContainerMap = containerMap

	// Helper function for verifying the restart count.
	verifyRestartCount := func(pod *api.Pod, expectedCount int) api.PodStatus {
		status, err := dm.GetPodStatus(pod)
		if err != nil {
			t.Fatalf("unexpected error %v", err)
		}
		restartCount := status.ContainerStatuses[0].RestartCount
		if restartCount != expectedCount {
			t.Errorf("expected %d restart count, got %d", expectedCount, restartCount)
		}
		return *status
	}

	// Container "bar" has failed twice; create two dead docker containers.
	// TODO: container lists are expected to be sorted reversely by time.
	// We should fix FakeDockerClient to sort the list before returning.
	fakeDocker.ExitedContainerList = []docker.APIContainers{{Names: names, ID: "5678"}, {Names: names, ID: "1234"}}
	pod.Status = verifyRestartCount(&pod, 1)

	// Found a new dead container. The restart count should be incremented.
	fakeDocker.ExitedContainerList = []docker.APIContainers{
		{Names: names, ID: "9101"}, {Names: names, ID: "5678"}, {Names: names, ID: "1234"}}
	pod.Status = verifyRestartCount(&pod, 2)

	// All dead containers have been GC'd. The restart count should persist
	// (i.e., remain the same).
	fakeDocker.ExitedContainerList = []docker.APIContainers{}
	verifyRestartCount(&pod, 2)
}

func TestSyncPodWithPodInfraCreatesContainerCallsHandler(t *testing.T) {
	fakeHTTPClient := &fakeHTTP{}
	dm, fakeDocker := newTestDockerManagerWithHTTPClient(fakeHTTPClient)

	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{
					Name: "bar",
					Lifecycle: &api.Lifecycle{
						PostStart: &api.Handler{
							HTTPGet: &api.HTTPGetAction{
								Host: "foo",
								Port: util.IntOrString{IntVal: 8080, Kind: util.IntstrInt},
								Path: "bar",
							},
						},
					},
				},
			},
		},
	}
	fakeDocker.ContainerList = []docker.APIContainers{
		{
			// pod infra container
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_0"},
			ID:    "9876",
		},
	}
	fakeDocker.ContainerMap = map[string]*docker.Container{
		"9876": {
			ID:         "9876",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	verifyCalls(t, fakeDocker, []string{
		// Check the pod infra container.
		"inspect_container",
		// Create container.
		"create", "start",
	})

	fakeDocker.Lock()
	if len(fakeDocker.Created) != 1 ||
		!matchString(t, "k8s_bar\\.[a-f0-9]+_foo_new_", fakeDocker.Created[0]) {
		t.Errorf("Unexpected containers created %v", fakeDocker.Created)
	}
	fakeDocker.Unlock()
	if fakeHTTPClient.url != "http://foo:8080/bar" {
		t.Errorf("Unexpected handler: %q", fakeHTTPClient.url)
	}
}

func TestSyncPodEventHandlerFails(t *testing.T) {
	// Simulate HTTP failure.
	fakeHTTPClient := &fakeHTTP{err: fmt.Errorf("test error")}
	dm, fakeDocker := newTestDockerManagerWithHTTPClient(fakeHTTPClient)

	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{Name: "bar",
					Lifecycle: &api.Lifecycle{
						PostStart: &api.Handler{
							HTTPGet: &api.HTTPGetAction{
								Host: "does.no.exist",
								Port: util.IntOrString{IntVal: 8080, Kind: util.IntstrInt},
								Path: "bar",
							},
						},
					},
				},
			},
		},
	}

	fakeDocker.ContainerList = []docker.APIContainers{
		{
			// pod infra container
			Names: []string{"/k8s_POD." + strconv.FormatUint(generatePodInfraContainerHash(pod), 16) + "_foo_new_12345678_42"},
			ID:    "9876",
		},
	}
	fakeDocker.ContainerMap = map[string]*docker.Container{
		"9876": {
			ID:         "9876",
			Config:     &docker.Config{},
			HostConfig: &docker.HostConfig{},
		},
	}

	runSyncPod(t, dm, fakeDocker, pod)

	verifyCalls(t, fakeDocker, []string{
		// Check the pod infra container.
		"inspect_container",
		// Create the container.
		"create", "start",
		// Kill the container since event handler fails.
		"inspect_container", "stop",
	})

	// TODO(yifan): Check the stopped container's name.
	if len(fakeDocker.Stopped) != 1 {
		t.Fatalf("Wrong containers were stopped: %v", fakeDocker.Stopped)
	}
	dockerName, _, err := ParseDockerName(fakeDocker.Stopped[0])
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if dockerName.ContainerName != "bar" {
		t.Errorf("Wrong stopped container, expected: bar, get: %q", dockerName.ContainerName)
	}
}
