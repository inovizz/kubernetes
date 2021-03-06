/*
Copyright 2017 The Kubernetes Authors.

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

package e2e_node

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"

	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/test/e2e/framework"
	e2elog "k8s.io/kubernetes/test/e2e/framework/log"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// makePodToVerifyHugePages returns a pod that verifies specified cgroup with hugetlb
func makePodToVerifyHugePages(baseName string, hugePagesLimit resource.Quantity) *v1.Pod {
	// convert the cgroup name to its literal form
	cgroupFsName := ""
	cgroupName := cm.NewCgroupName(cm.RootCgroupName, defaultNodeAllocatableCgroup, baseName)
	if framework.TestContext.KubeletConfig.CgroupDriver == "systemd" {
		cgroupFsName = cgroupName.ToSystemd()
	} else {
		cgroupFsName = cgroupName.ToCgroupfs()
	}

	// this command takes the expected value and compares it against the actual value for the pod cgroup hugetlb.2MB.limit_in_bytes
	command := fmt.Sprintf("expected=%v; actual=$(cat /tmp/hugetlb/%v/hugetlb.2MB.limit_in_bytes); if [ \"$expected\" -ne \"$actual\" ]; then exit 1; fi; ", hugePagesLimit.Value(), cgroupFsName)
	e2elog.Logf("Pod to run command: %v", command)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod" + string(uuid.NewUUID()),
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{
				{
					Image:   busyboxImage,
					Name:    "container" + string(uuid.NewUUID()),
					Command: []string{"sh", "-c", command},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "sysfscgroup",
							MountPath: "/tmp",
						},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "sysfscgroup",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{Path: "/sys/fs/cgroup"},
					},
				},
			},
		},
	}
	return pod
}

// enableHugePagesInKubelet enables hugepages feature for kubelet
func enableHugePagesInKubelet(f *framework.Framework) *kubeletconfig.KubeletConfiguration {
	oldCfg, err := getCurrentKubeletConfig()
	framework.ExpectNoError(err)
	newCfg := oldCfg.DeepCopy()
	if newCfg.FeatureGates == nil {
		newCfg.FeatureGates = make(map[string]bool)
		newCfg.FeatureGates["HugePages"] = true
	}

	// Update the Kubelet configuration.
	framework.ExpectNoError(setKubeletConfiguration(f, newCfg))

	// Wait for the Kubelet to be ready.
	Eventually(func() bool {
		nodeList := framework.GetReadySchedulableNodesOrDie(f.ClientSet)
		return len(nodeList.Items) == 1
	}, time.Minute, time.Second).Should(BeTrue())

	return oldCfg
}

// configureHugePages attempts to allocate 100Mi of 2Mi hugepages for testing purposes
func configureHugePages() error {
	err := exec.Command("/bin/sh", "-c", "echo 50 > /proc/sys/vm/nr_hugepages").Run()
	if err != nil {
		return err
	}
	outData, err := exec.Command("/bin/sh", "-c", "cat /proc/meminfo | grep 'HugePages_Total' | awk '{print $2}'").Output()
	if err != nil {
		return err
	}
	numHugePages, err := strconv.Atoi(strings.TrimSpace(string(outData)))
	if err != nil {
		return err
	}
	e2elog.Logf("HugePages_Total is set to %v", numHugePages)
	if numHugePages == 50 {
		return nil
	}
	return fmt.Errorf("expected hugepages %v, but found %v", 50, numHugePages)
}

// releaseHugePages releases all pre-allocated hugepages
func releaseHugePages() error {
	return exec.Command("/bin/sh", "-c", "echo 0 > /proc/sys/vm/nr_hugepages").Run()
}

// isHugePageSupported returns true if the default hugepagesize on host is 2Mi (i.e. 2048 kB)
func isHugePageSupported() bool {
	outData, err := exec.Command("/bin/sh", "-c", "cat /proc/meminfo | grep 'Hugepagesize:' | awk '{print $2}'").Output()
	framework.ExpectNoError(err)
	pageSize, err := strconv.Atoi(strings.TrimSpace(string(outData)))
	framework.ExpectNoError(err)
	return pageSize == 2048
}

// pollResourceAsString polls for a specified resource and capacity from node
func pollResourceAsString(f *framework.Framework, resourceName string) string {
	node, err := f.ClientSet.CoreV1().Nodes().Get(framework.TestContext.NodeName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	amount := amountOfResourceAsString(node, resourceName)
	e2elog.Logf("amount of %v: %v", resourceName, amount)
	return amount
}

// amountOfResourceAsString returns the amount of resourceName advertised by a node
func amountOfResourceAsString(node *v1.Node, resourceName string) string {
	val, ok := node.Status.Capacity[v1.ResourceName(resourceName)]
	if !ok {
		return ""
	}
	return val.String()
}

func runHugePagesTests(f *framework.Framework) {
	It("should assign hugepages as expected based on the Pod spec", func() {
		By("by running a G pod that requests hugepages")
		pod := f.PodClient().Create(&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod" + string(uuid.NewUUID()),
				Namespace: f.Namespace.Name,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Image: imageutils.GetPauseImageName(),
						Name:  "container" + string(uuid.NewUUID()),
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceName("cpu"):           resource.MustParse("10m"),
								v1.ResourceName("memory"):        resource.MustParse("100Mi"),
								v1.ResourceName("hugepages-2Mi"): resource.MustParse("50Mi"),
							},
						},
					},
				},
			},
		})
		podUID := string(pod.UID)
		By("checking if the expected hugetlb settings were applied")
		verifyPod := makePodToVerifyHugePages("pod"+podUID, resource.MustParse("50Mi"))
		f.PodClient().Create(verifyPod)
		err := e2epod.WaitForPodSuccessInNamespace(f.ClientSet, verifyPod.Name, f.Namespace.Name)
		framework.ExpectNoError(err)
	})
}

// Serial because the test updates kubelet configuration.
var _ = SIGDescribe("HugePages [Serial] [Feature:HugePages][NodeFeature:HugePages]", func() {
	f := framework.NewDefaultFramework("hugepages-test")

	Context("With config updated with hugepages feature enabled", func() {
		var oldCfg *kubeletconfig.KubeletConfiguration

		BeforeEach(func() {
			By("verifying hugepages are supported")
			if !isHugePageSupported() {
				framework.Skipf("skipping test because hugepages are not supported")
				return
			}
			By("configuring the host to reserve a number of pre-allocated hugepages")
			Eventually(func() error {
				err := configureHugePages()
				if err != nil {
					return err
				}
				return nil
			}, 30*time.Second, framework.Poll).Should(BeNil())
			By("enabling hugepages in kubelet")
			oldCfg = enableHugePagesInKubelet(f)
			By("restarting kubelet to pick up pre-allocated hugepages")
			restartKubelet()
			By("by waiting for hugepages resource to become available on the local node")
			Eventually(func() string {
				return pollResourceAsString(f, "hugepages-2Mi")
			}, 30*time.Second, framework.Poll).Should(Equal("100Mi"))
		})

		runHugePagesTests(f)

		AfterEach(func() {
			By("Releasing hugepages")
			Eventually(func() error {
				err := releaseHugePages()
				if err != nil {
					return err
				}
				return nil
			}, 30*time.Second, framework.Poll).Should(BeNil())
			if oldCfg != nil {
				By("Restoring old kubelet config")
				setOldKubeletConfig(f, oldCfg)
			}
			By("restarting kubelet to release hugepages")
			restartKubelet()
			By("by waiting for hugepages resource to not appear available on the local node")
			Eventually(func() string {
				return pollResourceAsString(f, "hugepages-2Mi")
			}, 30*time.Second, framework.Poll).Should(Equal("0"))
		})
	})
})
