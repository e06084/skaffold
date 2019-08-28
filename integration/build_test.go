/*
Copyright 2019 The Skaffold Authors

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

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes"
	corev1 "k8s.io/api/core/v1"

	"github.com/docker/docker/pkg/fileutils"

	"4d63.com/tz"
	"github.com/GoogleContainerTools/skaffold/integration/skaffold"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/docker"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/runner/runcontext"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/docker/docker/api/types"
	"github.com/sirupsen/logrus"
)

const imageName = "simple-build:"

func TestBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if ShouldRunGCPOnlyTests() {
		t.Skip("skipping test that is not gcp only")
	}

	tests := []struct {
		description string
		dir         string
		args        []string
		expectImage string
		setup       func(t *testing.T, workdir string) (teardown func())
	}{
		{
			description: "docker build",
			dir:         "testdata/build",
		},
		{
			description: "git tagger",
			dir:         "testdata/tagPolicy",
			args:        []string{"-p", "gitCommit"},
			setup:       setupGitRepo,
			expectImage: imageName + "corev1",
		},
		{
			description: "sha256 tagger",
			dir:         "testdata/tagPolicy",
			args:        []string{"-p", "sha256"},
			expectImage: imageName + "latest",
		},
		{
			description: "dateTime tagger",
			dir:         "testdata/tagPolicy",
			args:        []string{"-p", "dateTime"},
			// around midnight this test might fail, if the tests above run slowly
			expectImage: imageName + nowInChicago(),
		},
		{
			description: "envTemplate tagger",
			dir:         "testdata/tagPolicy",
			args:        []string{"-p", "envTemplate"},
			expectImage: imageName + "tag",
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			if test.setup != nil {
				teardown := test.setup(t, test.dir)
				defer teardown()
			}

			// Run without artifact caching
			removeImage(t, test.expectImage)
			skaffold.Build(append(test.args, "--cache-artifacts=false")...).InDir(test.dir).RunOrFail(t)
			checkImageExists(t, test.expectImage)

			// Run with artifact caching
			removeImage(t, test.expectImage)
			skaffold.Build(append(test.args, "--cache-artifacts=true")...).InDir(test.dir).RunOrFail(t)
			checkImageExists(t, test.expectImage)

			// Run a second time with artifact caching
			out := skaffold.Build(append(test.args, "--cache-artifacts=true")...).InDir(test.dir).RunOrFailOutput(t)
			if strings.Contains(string(out), "Not found. Building") {
				t.Errorf("images were expected to be found in cache: %s", out)
			}
			checkImageExists(t, test.expectImage)
		})
	}
}

//see integration/testdata/README.md for details
func TestBuildInCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if !ShouldRunGCPOnlyTests() {
		t.Skip("skipping test that is gcp only")
	}
	if written, err := fileutils.CopyFile("../out/skaffold-linux-amd64", "./testdata/skaffold-in-cluster/skaffold"); written <= 0 || err != nil {
		t.Errorf("failed to copy skaffold binary for test case: %s", err)
	}
	ns, nsClient, deleteNs := SetupNamespace(t)
	defer deleteNs()

	bindings := nsClient.client.RbacV1().ClusterRoleBindings()
	binding, err := bindings.Create(&rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "skaffold-in-cluster-test:",
		},
		RoleRef: rbacv1.RoleRef{
			Name:     "cluster-admin",
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "default",
				Namespace: ns.Name,
			},
		},
	})
	if err != nil {
		t.Errorf("failed to create role binding: %s", err)
	}
	defer func() {
		if err := bindings.Delete(binding.Name, &metav1.DeleteOptions{}); err != nil {
			t.Errorf("failed to cleanup role binding: %s", err)
		}
	}()

	skaffold.Run("-p", "create-build-step").InDir("./testdata/skaffold-in-cluster").InNs(ns.Name).RunOrFail(t)
	defer func() {
		if output, err := skaffold.Delete("-p", "create-build-step").InNs(ns.Name).InDir("./testdata/skaffold-in-cluster").RunWithCombinedOutput(t); err != nil {
			t.Logf("failed to cleanup skaffold-in-cluster: %s, output: %s", err, output)
		}
	}()

	podsClient := nsClient.client.CoreV1().Pods(ns.Name)

	if err := kubernetes.WaitForPodSucceeded(context.TODO(), podsClient, "skaffold-in-cluster", 2*time.Minute); err != nil {
		t.Errorf("in-cluster build pod failed: %s", err)
		logs, err := podsClient.GetLogs("skaffold-in-cluster", &corev1.PodLogOptions{}).DoRaw()
		if err != nil {
			t.Errorf("error getting logs for pod: %s", err)
			t.Fail()
			return
		}

		t.Errorf("logs: %s", logs)
		t.Fail()
	}
}

// removeImage removes the given image if present.
func removeImage(t *testing.T, image string) {
	t.Helper()

	if image == "" {
		return
	}

	client, err := docker.NewAPIClient(&runcontext.RunContext{})
	failNowIfError(t, err)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Second))
	defer cancel()
	_, _ = client.ImageRemove(ctx, image, types.ImageRemoveOptions{
		Force:         true,
		PruneChildren: true,
	})
}

// checkImageExists asserts that the given image is present
func checkImageExists(t *testing.T, image string) {
	t.Helper()

	if image == "" {
		return
	}

	client, err := docker.NewAPIClient(&runcontext.RunContext{})
	failNowIfError(t, err)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Second))
	defer cancel()
	if !client.ImageExists(ctx, image) {
		t.Errorf("expected image '%s' not present", image)
	}
}

// setupGitRepo sets up a clean repo with tag corev1
func setupGitRepo(t *testing.T, dir string) func() {
	gitArgs := [][]string{
		{"init"},
		{"config", "user.email", "john@doe.org"},
		{"config", "user.name", "John Doe"},
		{"add", "."},
		{"commit", "-m", "Initial commit"},
		{"tag", "corev1"},
	}

	for _, args := range gitArgs {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if buf, err := util.RunCmdOut(cmd); err != nil {
			t.Logf(string(buf))
			t.Fatal(err)
		}
	}

	return func() {
		os.RemoveAll(dir + "/.git")
	}
}

// nowInChicago returns the dateTime string as generated by the dateTime tagger
func nowInChicago() string {
	loc, _ := tz.LoadLocation("America/Chicago")
	return time.Now().In(loc).Format("2006-01-02")
}

func failNowIfError(t *testing.T, err error) {
	if err != nil {
		t.Fatal(err)
	}
}

// TestExpectedBuildFailures verifies that `skaffold build` fails in expected ways
func TestExpectedBuildFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if ShouldRunGCPOnlyTests() {
		t.Skip("skipping test that is not gcp only")
	}

	tests := []struct {
		description string
		dir         string
		args        []string
		expected    string
	}{
		{
			description: "jib is too old",
			dir:         "testdata/jib",
			args:        []string{"-p", "old-jib"},
			expected:    "Could not find goal '_skaffold-fail-if-jib-out-of-date' in plugin com.google.cloud.tools:jib-maven-plugin:1.3.0",
			// test string will need to be updated for the jib.requiredVersion error text when moving to Jib > 1.4.0
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			if out, err := skaffold.Build(test.args...).InDir(test.dir).RunWithCombinedOutput(t); err == nil {
				t.Fatal("expected build to fail")
			} else if !strings.Contains(string(out), test.expected) {
				logrus.Info("build output: ", string(out))
				t.Fatalf("build failed but for wrong reason")
			}
		})
	}
}
