/*
 Licensed to the Apache Software Foundation (ASF) under one
 or more contributor license agreements.  See the NOTICE file
 distributed with this work for additional information
 regarding copyright ownership.  The ASF licenses this file
 to you under the Apache License, Version 2.0 (the
 "License"); you may not use this file except in compliance
 with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package preemption_test

import (
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"

	"github.com/apache/yunikorn-core/pkg/common/configs"
	"github.com/apache/yunikorn-k8shim/pkg/common/constants"
	tests "github.com/apache/yunikorn-k8shim/test/e2e"
	"github.com/apache/yunikorn-k8shim/test/e2e/framework/helpers/common"
	"github.com/apache/yunikorn-k8shim/test/e2e/framework/helpers/k8s"
	"github.com/apache/yunikorn-k8shim/test/e2e/framework/helpers/yunikorn"
)

var kClient k8s.KubeCtl
var restClient yunikorn.RClient
var ns *v1.Namespace
var dev = "dev" + common.RandSeq(5)
var oldConfigMap = new(v1.ConfigMap)
var annotation = "ann-" + common.RandSeq(10)

// Nodes
var Worker = ""
var WorkerMemRes int64
var sleepPodMemLimit int64
var taintKey = "e2e_test_preemption"
var nodesToTaint []string

var _ = ginkgo.BeforeSuite(func() {
	// Initializing kubectl client
	kClient = k8s.KubeCtl{}
	Ω(kClient.SetClient()).To(gomega.BeNil())
	// Initializing rest client
	restClient = yunikorn.RClient{}
	Ω(restClient).NotTo(gomega.BeNil())

	yunikorn.EnsureYuniKornConfigsPresent()

	ginkgo.By("Port-forward the scheduler pod")
	var err = kClient.PortForwardYkSchedulerPod()
	Ω(err).NotTo(gomega.HaveOccurred())

	ginkgo.By("create development namespace")
	ns, err = kClient.CreateNamespace(dev, nil)
	gomega.Ω(err).NotTo(gomega.HaveOccurred())
	gomega.Ω(ns.Status.Phase).To(gomega.Equal(v1.NamespaceActive))

	var nodes *v1.NodeList
	nodes, err = kClient.GetNodes()
	Ω(err).NotTo(gomega.HaveOccurred())
	Ω(len(nodes.Items)).NotTo(gomega.BeZero(), "Nodes cant be empty")

	// Extract node allocatable resources
	for _, node := range nodes.Items {
		// skip master if it's marked as such
		node := node
		if k8s.IsMasterNode(&node) || !k8s.IsComputeNode(&node) {
			continue
		}
		if Worker == "" {
			Worker = node.Name
		} else {
			nodesToTaint = append(nodesToTaint, node.Name)
		}
	}
	Ω(Worker).NotTo(gomega.BeEmpty(), "Worker node not found")

	ginkgo.By("Tainting some nodes..")
	err = kClient.TaintNodes(nodesToTaint, taintKey, "value", v1.TaintEffectNoSchedule)
	Ω(err).NotTo(gomega.HaveOccurred())

	nodesDAOInfo, err := restClient.GetNodes(constants.DefaultPartition)
	Ω(err).NotTo(gomega.HaveOccurred())
	Ω(nodesDAOInfo).NotTo(gomega.BeNil())

	for _, node := range *nodesDAOInfo {
		if node.NodeID == Worker {
			WorkerMemRes = node.Available["memory"]
		}
	}
	WorkerMemRes /= (1000 * 1000) // change to M
	fmt.Fprintf(ginkgo.GinkgoWriter, "Worker node %s available memory %dM\n", Worker, WorkerMemRes)

	sleepPodMemLimit = int64(float64(WorkerMemRes) / 3)
	Ω(sleepPodMemLimit).NotTo(gomega.BeZero(), "Sleep pod memory limit cannot be zero")
	fmt.Fprintf(ginkgo.GinkgoWriter, "Sleep pod limit memory %dM\n", sleepPodMemLimit)
})

var _ = ginkgo.AfterSuite(func() {

	ginkgo.By("Untainting some nodes")
	err := kClient.UntaintNodes(nodesToTaint, taintKey)
	Ω(err).NotTo(gomega.HaveOccurred(), "Could not remove taint from nodes "+strings.Join(nodesToTaint, ","))

	ginkgo.By("Check Yunikorn's health")
	checks, err := yunikorn.GetFailedHealthChecks()
	Ω(err).NotTo(gomega.HaveOccurred())
	Ω(checks).To(gomega.Equal(""), checks)

	testDescription := ginkgo.CurrentSpecReport()
	if testDescription.Failed() {
		tests.LogTestClusterInfoWrapper(testDescription.FailureMessage(), []string{ns.Name})
		tests.LogYunikornContainer(testDescription.FailureMessage())
	}
	ginkgo.By("Tearing down namespace: " + ns.Name)
	err = kClient.TearDownNamespace(ns.Name)
	Ω(err).NotTo(gomega.HaveOccurred())
})

var _ = ginkgo.Describe("Preemption", func() {
	ginkgo.It("Verify_basic_preemption", func() {
		ginkgo.By("A queue uses resource more than the guaranteed value even after removing one of the pods. The cluster doesn't have enough resource to deploy a pod in another queue which uses resource less than the guaranteed value.")
		// update config
		ginkgo.By(fmt.Sprintf("Update root.sandbox1 and root.sandbox2 with guaranteed memory %dM", sleepPodMemLimit))
		annotation = "ann-" + common.RandSeq(10)
		yunikorn.UpdateCustomConfigMapWrapper(oldConfigMap, "", annotation, func(sc *configs.SchedulerConfig) error {
			// remove placement rules so we can control queue
			sc.Partitions[0].PlacementRules = nil

			var err error
			if err = common.AddQueue(sc, "default", "root", configs.QueueConfig{
				Name:       "sandbox1",
				Resources:  configs.Resources{Guaranteed: map[string]string{"memory": fmt.Sprintf("%dM", sleepPodMemLimit)}},
				Properties: map[string]string{"preemption.delay": "1s"},
			}); err != nil {
				return err
			}

			if err = common.AddQueue(sc, "default", "root", configs.QueueConfig{
				Name:       "sandbox2",
				Resources:  configs.Resources{Guaranteed: map[string]string{"memory": fmt.Sprintf("%dM", sleepPodMemLimit)}},
				Properties: map[string]string{"preemption.delay": "1s"},
			}); err != nil {
				return err
			}
			return nil
		})

		// Define sleepPod
		sleepPodConfigs := createSandbox1SleepPodCofigs(3, 600)
		sleepPod4Config := k8s.SleepPodConfig{Name: "sleepjob4", NS: dev, Mem: sleepPodMemLimit, Time: 600, Optedout: true, Labels: map[string]string{"queue": "root.sandbox2"}}
		sleepPodConfigs = append(sleepPodConfigs, sleepPod4Config)

		for _, config := range sleepPodConfigs {
			ginkgo.By("Deploy the sleep pod " + config.Name + " to the development namespace")
			sleepObj, podErr := k8s.InitSleepPod(config)
			Ω(podErr).NotTo(gomega.HaveOccurred())
			sleepRespPod, podErr := kClient.CreatePod(sleepObj, dev)
			gomega.Ω(podErr).NotTo(gomega.HaveOccurred())

			// Wait for pod to move to running state
			podErr = kClient.WaitForPodBySelectorRunning(dev,
				fmt.Sprintf("app=%s", sleepRespPod.ObjectMeta.Labels["app"]),
				60)
			gomega.Ω(podErr).NotTo(gomega.HaveOccurred())
		}

		// assert one of the pods in root.sandbox1 is preempted
		ginkgo.By("One of the pods in root.sanbox1 is preempted")
		sandbox1RunningPodsCnt := 0
		pods, err := kClient.ListPodsByLabelSelector(dev, "queue=root.sandbox1")
		gomega.Ω(err).NotTo(gomega.HaveOccurred())
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			if pod.Status.Phase == v1.PodRunning {
				sandbox1RunningPodsCnt++
			}
		}
		Ω(sandbox1RunningPodsCnt).To(gomega.Equal(2), "One of the pods in root.sandbox1 should be preempted")
	})

	ginkgo.It("Verify_no_preemption_on_resources_less_than_guaranteed_value", func() {
		ginkgo.By("A queue uses resource less than the guaranteed value can't be preempted.")
		// update config
		ginkgo.By(fmt.Sprintf("Update root.sandbox1 and root.sandbox2 with guaranteed memory %dM", WorkerMemRes))
		annotation = "ann-" + common.RandSeq(10)
		yunikorn.UpdateCustomConfigMapWrapper(oldConfigMap, "", annotation, func(sc *configs.SchedulerConfig) error {
			// remove placement rules so we can control queue
			sc.Partitions[0].PlacementRules = nil

			var err error
			if err = common.AddQueue(sc, "default", "root", configs.QueueConfig{
				Name:       "sandbox1",
				Resources:  configs.Resources{Guaranteed: map[string]string{"memory": fmt.Sprintf("%dM", WorkerMemRes)}},
				Properties: map[string]string{"preemption.delay": "1s"},
			}); err != nil {
				return err
			}

			if err = common.AddQueue(sc, "default", "root", configs.QueueConfig{
				Name:       "sandbox2",
				Resources:  configs.Resources{Guaranteed: map[string]string{"memory": fmt.Sprintf("%dM", WorkerMemRes)}},
				Properties: map[string]string{"preemption.delay": "1s"},
			}); err != nil {
				return err
			}
			return nil
		})

		// Define sleepPod
		sandbox1SleepPodConfigs := createSandbox1SleepPodCofigs(3, 30)
		sleepPod4Config := k8s.SleepPodConfig{Name: "sleepjob4", NS: dev, Mem: sleepPodMemLimit, Time: 30, Optedout: true, Labels: map[string]string{"queue": "root.sandbox2"}}

		// Deploy pods in root.sandbox1
		for _, config := range sandbox1SleepPodConfigs {
			ginkgo.By("Deploy the sleep pod " + config.Name + " to the development namespace")
			sleepObj, podErr := k8s.InitSleepPod(config)
			Ω(podErr).NotTo(gomega.HaveOccurred())
			sleepRespPod, podErr := kClient.CreatePod(sleepObj, dev)
			gomega.Ω(podErr).NotTo(gomega.HaveOccurred())

			// Wait for pod to move to running state
			podErr = kClient.WaitForPodBySelectorRunning(dev,
				fmt.Sprintf("app=%s", sleepRespPod.ObjectMeta.Labels["app"]),
				30)
			gomega.Ω(podErr).NotTo(gomega.HaveOccurred())
		}

		// Deploy sleepjob4 pod in root.sandbox2
		ginkgo.By("Deploy the sleep pod " + sleepPod4Config.Name + " to the development namespace")
		sleepObj, podErr := k8s.InitSleepPod(sleepPod4Config)
		Ω(podErr).NotTo(gomega.HaveOccurred())
		sleepRespPod4, err := kClient.CreatePod(sleepObj, dev)
		gomega.Ω(err).NotTo(gomega.HaveOccurred())

		// sleepjob4 pod can't be scheduled before pods in root.sandbox1 are succeeded
		ginkgo.By("The sleep pod " + sleepPod4Config.Name + " can't be scheduled")
		err = kClient.WaitForPodUnschedulable(sleepRespPod4, 60*time.Second)
		gomega.Ω(err).NotTo(gomega.HaveOccurred())

		// pods in root.sandbox1 can be succeeded
		ginkgo.By("The pods in root.sandbox1 can be succeeded")
		for _, config := range sandbox1SleepPodConfigs {
			err = kClient.WaitForPodSucceeded(dev, config.Name, 30*time.Second)
			gomega.Ω(err).NotTo(gomega.HaveOccurred())
		}
	})

	ginkgo.It("Verify_no_preemption_outside_fence", func() {
		ginkgo.By("The preemption can't go outside the fence.")
		// update config
		ginkgo.By(fmt.Sprintf("Update root.sandbox1 and root.sandbox2 with guaranteed memory %dM. The root.sandbox2 has fence preemption policy.", sleepPodMemLimit))
		annotation = "ann-" + common.RandSeq(10)
		yunikorn.UpdateCustomConfigMapWrapper(oldConfigMap, "", annotation, func(sc *configs.SchedulerConfig) error {
			// remove placement rules so we can control queue
			sc.Partitions[0].PlacementRules = nil

			var err error
			if err = common.AddQueue(sc, "default", "root", configs.QueueConfig{
				Name:       "sandbox1",
				Resources:  configs.Resources{Guaranteed: map[string]string{"memory": fmt.Sprintf("%dM", sleepPodMemLimit)}},
				Properties: map[string]string{"preemption.delay": "1s"},
			}); err != nil {
				return err
			}

			if err = common.AddQueue(sc, "default", "root", configs.QueueConfig{
				Name:       "sandbox2",
				Resources:  configs.Resources{Guaranteed: map[string]string{"memory": fmt.Sprintf("%dM", sleepPodMemLimit)}},
				Properties: map[string]string{"preemption.delay": "1s", "preemption.policy": "fence"},
			}); err != nil {
				return err
			}
			return nil
		})

		// Define sleepPod
		sandbox1SleepPodConfigs := createSandbox1SleepPodCofigs(3, 30)
		sleepPod4Config := k8s.SleepPodConfig{Name: "sleepjob4", NS: dev, Mem: sleepPodMemLimit, Time: 30, Optedout: true, Labels: map[string]string{"queue": "root.sandbox2"}}

		// Deploy pods in root.sandbox1
		for _, config := range sandbox1SleepPodConfigs {
			ginkgo.By("Deploy the sleep pod " + config.Name + " to the development namespace")
			sleepObj, podErr := k8s.InitSleepPod(config)
			Ω(podErr).NotTo(gomega.HaveOccurred())
			sleepRespPod, podErr := kClient.CreatePod(sleepObj, dev)
			gomega.Ω(podErr).NotTo(gomega.HaveOccurred())

			// Wait for pod to move to running state
			podErr = kClient.WaitForPodBySelectorRunning(dev,
				fmt.Sprintf("app=%s", sleepRespPod.ObjectMeta.Labels["app"]),
				30)
			gomega.Ω(podErr).NotTo(gomega.HaveOccurred())
		}

		// Deploy sleepjob4 pod in root.sandbox2
		ginkgo.By("Deploy the sleep pod " + sleepPod4Config.Name + " to the development namespace")
		sleepObj, podErr := k8s.InitSleepPod(sleepPod4Config)
		Ω(podErr).NotTo(gomega.HaveOccurred())
		sleepRespPod4, err := kClient.CreatePod(sleepObj, dev)
		gomega.Ω(err).NotTo(gomega.HaveOccurred())

		// sleepjob4 pod can't be scheduled before pods in root.sandbox1 are succeeded
		ginkgo.By("The sleep pod " + sleepPod4Config.Name + " can't be scheduled")
		err = kClient.WaitForPodUnschedulable(sleepRespPod4, 60*time.Second)
		gomega.Ω(err).NotTo(gomega.HaveOccurred())

		// pods in root.sandbox1 can be succeeded
		ginkgo.By("The pods in root.sandbox1 can be succeeded")
		for _, config := range sandbox1SleepPodConfigs {
			err = kClient.WaitForPodSucceeded(dev, config.Name, 30*time.Second)
			gomega.Ω(err).NotTo(gomega.HaveOccurred())
		}
	})

	ginkgo.AfterEach(func() {

		// Delete all sleep pods
		ginkgo.By("Delete all sleep pods")
		err := kClient.DeletePods(ns.Name)
		if err != nil {
			fmt.Fprintf(ginkgo.GinkgoWriter, "Failed to delete pods in namespace %s - reason is %s\n", ns.Name, err.Error())
		}

		// reset config
		ginkgo.By("Restoring YuniKorn configuration")
		yunikorn.RestoreConfigMapWrapper(oldConfigMap, annotation)
	})
})

func createSandbox1SleepPodCofigs(cnt, time int) []k8s.SleepPodConfig {
	sandbox1Configs := make([]k8s.SleepPodConfig, 0, cnt)
	for i := 0; i < cnt; i++ {
		sandbox1Configs = append(sandbox1Configs, k8s.SleepPodConfig{Name: fmt.Sprintf("sleepjob%d", i+1), NS: dev, Mem: sleepPodMemLimit, Time: time, Optedout: true, Labels: map[string]string{"queue": "root.sandbox1"}})
	}
	return sandbox1Configs
}
