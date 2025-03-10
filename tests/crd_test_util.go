// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"

	"github.com/pingcap/advanced-statefulset/client/apis/apps/v1/helper"
	asclientset "github.com/pingcap/advanced-statefulset/client/client/clientset/versioned"
	"github.com/pingcap/tidb-operator/pkg/apis/label"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/client/clientset/versioned"
	"github.com/pingcap/tidb-operator/pkg/controller"
	utilstatefulset "github.com/pingcap/tidb-operator/tests/e2e/util/statefulset"
	"github.com/pingcap/tidb-operator/tests/slack"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	typedappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/kubernetes/test/e2e/framework/log"
)

type pumpStatus struct {
	StatusMap map[string]*nodeStatus `json:"StatusMap"`
}

type nodeStatus struct {
	State string `json:"state"`
}

type CrdTestUtil struct {
	cli         versioned.Interface
	kubeCli     kubernetes.Interface
	tcStsGetter typedappsv1.StatefulSetsGetter
	asCli       asclientset.Interface
}

func NewCrdTestUtil(cli versioned.Interface, kubeCli kubernetes.Interface, asCli asclientset.Interface, stsGetter typedappsv1.StatefulSetsGetter) *CrdTestUtil {
	return &CrdTestUtil{
		cli:         cli,
		kubeCli:     kubeCli,
		tcStsGetter: stsGetter,
		asCli:       asCli,
	}
}

func (ctu *CrdTestUtil) GetTidbClusterOrDie(name, namespace string) *v1alpha1.TidbCluster {
	tc, err := ctu.cli.PingcapV1alpha1().TidbClusters(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		slack.NotifyAndPanic(err)
	}
	return tc
}

func (ctu *CrdTestUtil) CreateTidbClusterOrDie(tc *v1alpha1.TidbCluster) {
	_, err := ctu.cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Create(context.TODO(), tc, metav1.CreateOptions{})
	if err != nil {
		slack.NotifyAndPanic(err)
	}
}

func (ctu *CrdTestUtil) UpdateTidbClusterOrDie(tc *v1alpha1.TidbCluster) {
	err := wait.Poll(5*time.Second, 3*time.Minute, func() (done bool, err error) {
		latestTC, err := ctu.cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Get(context.TODO(), tc.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		latestTC.Spec = tc.Spec
		_, err = ctu.cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Update(context.TODO(), latestTC, metav1.UpdateOptions{})
		if err != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		slack.NotifyAndPanic(err)
	}
}

func (ctu *CrdTestUtil) CheckDisasterToleranceOrDie(tc *v1alpha1.TidbCluster) {
	err := ctu.CheckDisasterTolerance(tc)
	if err != nil {
		slack.NotifyAndPanic(err)
	}
}

func (ctu *CrdTestUtil) CheckDisasterTolerance(cluster *v1alpha1.TidbCluster) error {
	pds, err := ctu.kubeCli.CoreV1().Pods(cluster.Namespace).List(context.TODO(),
		metav1.ListOptions{LabelSelector: labels.SelectorFromSet(
			label.New().Instance(cluster.Name).PD().Labels(),
		).String()})
	if err != nil {
		return err
	}
	err = checkPodsAffinity(pds.Items)
	if err != nil {
		return err
	}

	tikvs, err := ctu.kubeCli.CoreV1().Pods(cluster.Namespace).List(context.TODO(),
		metav1.ListOptions{LabelSelector: labels.SelectorFromSet(
			label.New().Instance(cluster.Name).TiKV().Labels(),
		).String()})
	if err != nil {
		return err
	}
	err = checkPodsAffinity(tikvs.Items)
	if err != nil {
		return err
	}

	tidbs, err := ctu.kubeCli.CoreV1().Pods(cluster.Namespace).List(context.TODO(),
		metav1.ListOptions{LabelSelector: labels.SelectorFromSet(
			label.New().Instance(cluster.Name).TiDB().Labels(),
		).String()})
	if err != nil {
		return err
	}
	return checkPodsAffinity(tidbs.Items)
}

func checkPodsAffinity(allPods []corev1.Pod) error {
	for _, pod := range allPods {
		if pod.Spec.Affinity == nil {
			return fmt.Errorf("the pod:[%s/%s] has not Affinity", pod.Namespace, pod.Name)
		}
		if pod.Spec.Affinity.PodAntiAffinity == nil {
			return fmt.Errorf("the pod:[%s/%s] has not Affinity.PodAntiAffinity", pod.Namespace, pod.Name)
		}
		if len(pod.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
			return fmt.Errorf("the pod:[%s/%s] has not PreferredDuringSchedulingIgnoredDuringExecution", pod.Namespace, pod.Name)
		}
		for _, prefer := range pod.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			if prefer.PodAffinityTerm.TopologyKey != RackLabel {
				return fmt.Errorf("the pod:[%s/%s] topology key is not %s", pod.Namespace, pod.Name, RackLabel)
			}
		}
	}
	return nil
}

func (ctu *CrdTestUtil) DeleteTidbClusterOrDie(tc *v1alpha1.TidbCluster) {
	err := ctu.cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Delete(context.TODO(), tc.Name, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		slack.NotifyAndPanic(err)
	}
}

func (ctu *CrdTestUtil) WaitTidbClusterReadyOrDie(tc *v1alpha1.TidbCluster, timeout time.Duration) {
	err := ctu.WaitForTidbClusterReady(tc, timeout, 5*time.Second)
	if err != nil {
		slack.NotifyAndPanic(err)
	}
}

// WaitForTidbClusterReady waits for tidb components ready, or timeout
func (ctu *CrdTestUtil) WaitForTidbClusterReady(tc *v1alpha1.TidbCluster, timeout, pollInterval time.Duration) error {
	if tc == nil {
		return fmt.Errorf("tidbcluster is nil, cannot call WaitForTidbClusterReady")
	}
	return wait.PollImmediate(pollInterval, timeout, func() (bool, error) {
		var local *v1alpha1.TidbCluster
		var err error
		if local, err = ctu.cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Get(context.TODO(), tc.Name, metav1.GetOptions{}); err != nil {
			log.Logf("ERROR: failed to get tidbcluster: %s/%s, %v", tc.Namespace, tc.Name, err)
			return false, nil
		}

		if b, err := ctu.pdMembersReadyFn(local); !b && err == nil {
			log.Logf("pd members are not ready for tc %q", tc.Name)
			return false, nil
		}
		log.Logf("pd members are ready for tc %q", tc.Name)

		if b, err := ctu.tikvMembersReadyFn(local); !b && err == nil {
			log.Logf("tikv members are not ready for tc %q", tc.Name)
			return false, nil
		}
		log.Logf("tikv members are ready for tc %q", tc.Name)

		if b, err := ctu.tidbMembersReadyFn(local); !b && err == nil {
			log.Logf("tidb members are not ready for tc %q", tc.Name)
			return false, nil
		}
		log.Logf("tidb members are ready for tc %q", tc.Name)

		if tc.Spec.TiFlash != nil && tc.Spec.TiFlash.Replicas > int32(0) {
			if b, err := ctu.tiflashMembersReadyFn(local); !b && err == nil {
				log.Logf("tiflash members are not ready for tc %q", tc.Name)
				return false, nil
			}
			log.Logf("tiflash members are ready for tc %q", tc.Name)
		} else {
			log.Logf("no tiflash in tc spec")
		}

		if tc.Spec.Pump != nil {
			if b, err := ctu.pumpMembersReadyFn(local); !b && err == nil {
				log.Logf("pump members are not ready for tc %q", tc.Name)
				return false, nil
			}
			log.Logf("pump members are ready for tc %q", tc.Name)
		} else {
			log.Logf("no pump in tc spec")
		}

		log.Logf("TidbCluster is ready")
		return true, nil
	})
}

func (ctu *CrdTestUtil) pdMembersReadyFn(tc *v1alpha1.TidbCluster) (bool, error) {
	tcName := tc.GetName()
	ns := tc.GetNamespace()
	pdSetName := controller.PDMemberName(tcName)

	pdSet, err := ctu.tcStsGetter.StatefulSets(ns).Get(context.TODO(), pdSetName, metav1.GetOptions{})
	if err != nil {
		log.Logf("ERROR: failed to get statefulset: %s/%s, %v", ns, pdSetName, err)
		return false, nil
	}

	if pdSet.Status.CurrentRevision != pdSet.Status.UpdateRevision {
		log.Logf("pd sts .Status.CurrentRevision (%s) != .Status.UpdateRevision (%s)", pdSet.Status.CurrentRevision, pdSet.Status.UpdateRevision)
		return false, nil
	}

	if !utilstatefulset.IsAllDesiredPodsRunningAndReady(helper.NewHijackClient(ctu.kubeCli, ctu.asCli), pdSet) {
		return false, nil
	}

	if tc.Status.PD.StatefulSet == nil {
		log.Logf("tidbcluster: %s/%s .status.PD.StatefulSet is nil", ns, tcName)
		return false, nil
	}
	failureCount := len(tc.Status.PD.FailureMembers)
	replicas := tc.Spec.PD.Replicas + int32(failureCount)
	if *pdSet.Spec.Replicas != replicas {
		log.Logf("statefulset: %s/%s .spec.Replicas(%d) != %d",
			ns, pdSetName, *pdSet.Spec.Replicas, replicas)
		return false, nil
	}
	if pdSet.Status.ReadyReplicas != tc.Spec.PD.Replicas {
		log.Logf("statefulset: %s/%s .status.ReadyReplicas(%d) != %d",
			ns, pdSetName, pdSet.Status.ReadyReplicas, tc.Spec.PD.Replicas)
		return false, nil
	}
	if len(tc.Status.PD.Members) != int(tc.Spec.PD.Replicas) {
		log.Logf("tidbcluster: %s/%s .status.PD.Members count(%d) != %d",
			ns, tcName, len(tc.Status.PD.Members), tc.Spec.PD.Replicas)
		return false, nil
	}
	if pdSet.Status.ReadyReplicas != pdSet.Status.Replicas {
		log.Logf("statefulset: %s/%s .status.ReadyReplicas(%d) != .status.Replicas(%d)",
			ns, pdSetName, pdSet.Status.ReadyReplicas, pdSet.Status.Replicas)
		return false, nil
	}

	expectedImage := tc.PDImage()
	containers, err := utilstatefulset.GetMemberContainersFromSts(ctu.kubeCli, ctu.tcStsGetter, ns, pdSetName, v1alpha1.PDMemberType)
	if err != nil {
		log.Logf("statefulset: %s/%s not found containers[name=pd] or pod %s-0",
			ns, pdSetName, pdSetName)
		return false, nil
	}

	for _, container := range containers {
		if container.Image != expectedImage {
			log.Logf("statefulset: %s/%s .spec.template.spec.containers[name=pd].image(%s) != %s",
				ns, pdSetName, container.Image, tc.PDImage())
			return false, nil
		}
	}

	for _, member := range tc.Status.PD.Members {
		if !member.Health {
			log.Logf("tidbcluster: %s/%s pd member(%s/%s) is not health",
				ns, tcName, member.ID, member.Name)
			return false, nil
		}
	}

	pdServiceName := controller.PDMemberName(tcName)
	pdPeerServiceName := controller.PDPeerMemberName(tcName)
	if _, err := ctu.kubeCli.CoreV1().Services(ns).Get(context.TODO(), pdServiceName, metav1.GetOptions{}); err != nil {
		log.Logf("ERROR: failed to get service: %s/%s", ns, pdServiceName)
		return false, nil
	}
	if _, err := ctu.kubeCli.CoreV1().Services(ns).Get(context.TODO(), pdPeerServiceName, metav1.GetOptions{}); err != nil {
		log.Logf("ERROR: failed to get peer service: %s/%s", ns, pdPeerServiceName)
		return false, nil
	}

	return true, nil
}

func (ctu *CrdTestUtil) tikvMembersReadyFn(obj runtime.Object) (bool, error) {
	meta, ok := obj.(metav1.Object)
	if !ok {
		return false, fmt.Errorf("failed to convert to meta.Object")
	}
	name := meta.GetName()
	ns := meta.GetNamespace()
	var tikvSetName string
	if tc, ok := obj.(*v1alpha1.TidbCluster); ok {
		tikvSetName = controller.TiKVMemberName(tc.Name)
	} else {
		return false, fmt.Errorf("failed to parse obj to TidbCluster")
	}

	tikvSet, err := ctu.tcStsGetter.StatefulSets(ns).Get(context.TODO(), tikvSetName, metav1.GetOptions{})
	if err != nil {
		log.Logf("ERROR: failed to get statefulset: %s/%s, %v", ns, tikvSetName, err)
		return false, nil
	}

	if tikvSet.Status.CurrentRevision != tikvSet.Status.UpdateRevision {
		log.Logf("tikv sts .Status.CurrentRevision (%s) != .Status.UpdateRevision (%s)", tikvSet.Status.CurrentRevision, tikvSet.Status.UpdateRevision)
		return false, nil
	}

	if !utilstatefulset.IsAllDesiredPodsRunningAndReady(helper.NewHijackClient(ctu.kubeCli, ctu.asCli), tikvSet) {
		return false, nil
	}
	var tikvStatus v1alpha1.TiKVStatus
	var replicas int32
	var storeCounts int32
	var image string
	var stores map[string]v1alpha1.TiKVStore
	var tikvPeerServiceName string
	if tc, ok := obj.(*v1alpha1.TidbCluster); ok {
		tikvStatus = tc.Status.TiKV
		replicas = tc.Spec.TiKV.Replicas + int32(len(tc.Status.TiKV.FailureStores))
		storeCounts = int32(len(tc.Status.TiKV.Stores))
		image = tc.TiKVImage()
		stores = tc.Status.TiKV.Stores
		tikvPeerServiceName = controller.TiKVPeerMemberName(tc.GetName())
	}

	if tikvStatus.StatefulSet == nil {
		log.Logf("%s/%s .status.StatefulSet is nil", ns, name)
		return false, nil
	}
	if *tikvSet.Spec.Replicas != replicas {
		log.Logf("statefulset: %s/%s .spec.Replicas(%d) != %d",
			ns, tikvSetName, *tikvSet.Spec.Replicas, replicas)
		return false, nil
	}
	if tikvSet.Status.ReadyReplicas != replicas {
		log.Logf("statefulset: %s/%s .status.ReadyReplicas(%d) != %d",
			ns, tikvSetName, tikvSet.Status.ReadyReplicas, replicas)
		return false, nil
	}
	if storeCounts != replicas {
		log.Logf("%s/%s .status.TiKV.Stores.count(%d) != %d",
			ns, name, storeCounts, replicas)
		return false, nil
	}
	if tikvSet.Status.ReadyReplicas != tikvSet.Status.Replicas {
		log.Logf("statefulset: %s/%s .status.ReadyReplicas(%d) != .status.Replicas(%d)",
			ns, tikvSetName, tikvSet.Status.ReadyReplicas, tikvSet.Status.Replicas)
		return false, nil
	}

	expectedImage := image
	containers, err := utilstatefulset.GetMemberContainersFromSts(ctu.kubeCli, ctu.tcStsGetter, ns, tikvSetName, v1alpha1.TiKVMemberType)
	if err != nil {
		log.Logf("statefulset: %s/%s not found containers[name=tikv] or pod %s-0",
			ns, tikvSetName, tikvSetName)
		return false, nil
	}

	for _, container := range containers {
		if container.Image != expectedImage {
			log.Logf("statefulset: %s/%s .spec.template.spec.containers[name=tikv].image(%s) != %s",
				ns, tikvSetName, container.Image, image)
			return false, nil
		}
	}

	for _, store := range stores {
		if store.State != v1alpha1.TiKVStateUp {
			log.Logf("%s/%s's store(%s) state != %s", ns, name, store.ID, v1alpha1.TiKVStateUp)
			return false, nil
		}
	}
	if _, err := ctu.kubeCli.CoreV1().Services(ns).Get(context.TODO(), tikvPeerServiceName, metav1.GetOptions{}); err != nil {
		log.Logf("ERROR: failed to get peer service: %s/%s", ns, tikvPeerServiceName)
		return false, nil
	}
	return true, nil
}

func (ctu *CrdTestUtil) tidbMembersReadyFn(tc *v1alpha1.TidbCluster) (bool, error) {
	tcName := tc.GetName()
	ns := tc.GetNamespace()
	tidbSetName := controller.TiDBMemberName(tcName)

	tidbSet, err := ctu.tcStsGetter.StatefulSets(ns).Get(context.TODO(), tidbSetName, metav1.GetOptions{})
	if err != nil {
		log.Logf("ERROR: failed to get statefulset: %s/%s, %v", ns, tidbSetName, err)
		return false, nil
	}

	if tidbSet.Status.CurrentRevision != tidbSet.Status.UpdateRevision {
		log.Logf("tidb sts .Status.CurrentRevision (%s) != tidb sts .Status.UpdateRevision (%s)", tidbSet.Status.CurrentRevision, tidbSet.Status.UpdateRevision)
		return false, nil
	}

	if !utilstatefulset.IsAllDesiredPodsRunningAndReady(helper.NewHijackClient(ctu.kubeCli, ctu.asCli), tidbSet) {
		return false, nil
	}

	if tc.Status.TiDB.StatefulSet == nil {
		log.Logf("tidbcluster: %s/%s .status.TiDB.StatefulSet is nil", ns, tcName)
		return false, nil
	}
	failureCount := len(tc.Status.TiDB.FailureMembers)
	replicas := tc.Spec.TiDB.Replicas + int32(failureCount)
	if *tidbSet.Spec.Replicas != replicas {
		log.Logf("statefulset: %s/%s .spec.Replicas(%d) != %d",
			ns, tidbSetName, *tidbSet.Spec.Replicas, replicas)
		return false, nil
	}
	if tidbSet.Status.ReadyReplicas != tc.Spec.TiDB.Replicas {
		log.Logf("statefulset: %s/%s .status.ReadyReplicas(%d) != %d",
			ns, tidbSetName, tidbSet.Status.ReadyReplicas, tc.Spec.TiDB.Replicas)
		return false, nil
	}
	if len(tc.Status.TiDB.Members) != int(tc.Spec.TiDB.Replicas) {
		log.Logf("tidbcluster: %s/%s .status.TiDB.Members count(%d) != %d",
			ns, tcName, len(tc.Status.TiDB.Members), tc.Spec.TiDB.Replicas)
		return false, nil
	}
	if tidbSet.Status.ReadyReplicas != tidbSet.Status.Replicas {
		log.Logf("statefulset: %s/%s .status.ReadyReplicas(%d) != .status.Replicas(%d)",
			ns, tidbSetName, tidbSet.Status.ReadyReplicas, tidbSet.Status.Replicas)
		return false, nil
	}

	expectedImage := tc.TiDBImage()
	containers, err := utilstatefulset.GetMemberContainersFromSts(ctu.kubeCli, ctu.tcStsGetter, ns, tidbSetName, v1alpha1.TiDBMemberType)
	if err != nil {
		log.Logf("statefulset: %s/%s not found containers[name=tidb] or pod %s-0",
			ns, tidbSetName, tidbSetName)
		return false, nil
	}

	for _, container := range containers {
		if container.Image != expectedImage {
			log.Logf("statefulset: %s/%s .spec.template.spec.containers[name=tidb].image(%s) != %s",
				ns, tidbSetName, container.Image, tc.TiDBImage())
			return false, nil
		}
	}

	_, err = ctu.kubeCli.CoreV1().Services(ns).Get(context.TODO(), tidbSetName, metav1.GetOptions{})
	if err != nil {
		log.Logf("ERROR: failed to get service: %s/%s", ns, tidbSetName)
		return false, nil
	}
	_, err = ctu.kubeCli.CoreV1().Services(ns).Get(context.TODO(), controller.TiDBPeerMemberName(tcName), metav1.GetOptions{})
	if err != nil {
		log.Logf("ERROR: failed to get peer service: %s/%s", ns, controller.TiDBPeerMemberName(tcName))
		return false, nil
	}

	return true, nil
}

func (ctu *CrdTestUtil) tiflashMembersReadyFn(tc *v1alpha1.TidbCluster) (bool, error) {
	tcName := tc.GetName()
	ns := tc.GetNamespace()
	tiflashSetName := controller.TiFlashMemberName(tcName)

	tiflashSet, err := ctu.tcStsGetter.StatefulSets(ns).Get(context.TODO(), tiflashSetName, metav1.GetOptions{})
	if err != nil {
		log.Logf("ERROR: failed to get statefulset: %s/%s, %v", ns, tiflashSetName, err)
		return false, nil
	}

	if tiflashSet.Status.CurrentRevision != tiflashSet.Status.UpdateRevision {
		log.Logf("tiflash sts .Status.CurrentRevision (%s) != .Status.UpdateRevision (%s)", tiflashSet.Status.CurrentRevision, tiflashSet.Status.UpdateRevision)
		return false, nil
	}

	if !utilstatefulset.IsAllDesiredPodsRunningAndReady(helper.NewHijackClient(ctu.kubeCli, ctu.asCli), tiflashSet) {
		return false, nil
	}

	if tc.Status.TiFlash.StatefulSet == nil {
		log.Logf("tidbcluster: %s/%s .status.TiFlash.StatefulSet is nil", ns, tcName)
		return false, nil
	}
	failureCount := len(tc.Status.TiFlash.FailureStores)
	replicas := tc.Spec.TiFlash.Replicas + int32(failureCount)
	if *tiflashSet.Spec.Replicas != replicas {
		log.Logf("statefulset: %s/%s .spec.Replicas(%d) != %d",
			ns, tiflashSetName, *tiflashSet.Spec.Replicas, replicas)
		return false, nil
	}
	if tiflashSet.Status.ReadyReplicas != replicas {
		log.Logf("statefulset: %s/%s .status.ReadyReplicas(%d) != %d",
			ns, tiflashSetName, tiflashSet.Status.ReadyReplicas, replicas)
		return false, nil
	}
	if len(tc.Status.TiFlash.Stores) != int(replicas) {
		log.Logf("tidbcluster: %s/%s .status.TiFlash.Stores.count(%d) != %d",
			ns, tcName, len(tc.Status.TiFlash.Stores), replicas)
		return false, nil
	}
	if tiflashSet.Status.ReadyReplicas != tiflashSet.Status.Replicas {
		log.Logf("statefulset: %s/%s .status.ReadyReplicas(%d) != .status.Replicas(%d)",
			ns, tiflashSetName, tiflashSet.Status.ReadyReplicas, tiflashSet.Status.Replicas)
		return false, nil
	}
	expectedImage := tc.TiFlashImage()
	containers, err := utilstatefulset.GetMemberContainersFromSts(ctu.kubeCli, ctu.tcStsGetter, ns, tiflashSetName, v1alpha1.TiFlashMemberType)
	if err != nil {
		log.Logf("statefulset: %s/%s not found containers[name=tiflash] or pod %s-0",
			ns, tiflashSetName, tiflashSetName)
		return false, nil
	}

	for _, container := range containers {
		if container.Image != expectedImage {
			log.Logf("statefulset: %s/%s .spec.template.spec.containers[name=tiflash].image(%s) != %s",
				ns, tiflashSetName, container.Image, tc.TiFlashImage())
			return false, nil
		}
	}

	for _, store := range tc.Status.TiFlash.Stores {
		if store.State != v1alpha1.TiKVStateUp {
			log.Logf("tidbcluster: %s/%s's store(%s) state != %s", ns, tcName, store.ID, v1alpha1.TiKVStateUp)
			return false, nil
		}
	}

	tiflashPeerServiceName := controller.TiFlashPeerMemberName(tcName)
	if _, err := ctu.kubeCli.CoreV1().Services(ns).Get(context.TODO(), tiflashPeerServiceName, metav1.GetOptions{}); err != nil {
		log.Logf("ERROR: failed to get peer service: %s/%s", ns, tiflashPeerServiceName)
		return false, nil
	}

	return true, nil
}

func (ctu *CrdTestUtil) pumpMembersReadyFn(tc *v1alpha1.TidbCluster) (bool, error) {
	log.Logf("begin to check incremental backup cluster[%s] namespace[%s]", tc.Name, tc.Namespace)
	pumpStatefulSetName := fmt.Sprintf("%s-pump", tc.Name)

	pumpStatefulSet, err := ctu.kubeCli.AppsV1().StatefulSets(tc.Namespace).Get(context.TODO(), pumpStatefulSetName, metav1.GetOptions{})
	if err != nil {
		log.Logf("ERROR: failed to get jobs %s ,%v", pumpStatefulSetName, err)
		return false, nil
	}
	if pumpStatefulSet.Status.Replicas != pumpStatefulSet.Status.ReadyReplicas {
		log.Logf("pump replicas is not ready, please wait ! %s ", pumpStatefulSetName)
		return false, nil
	}

	listOps := metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(
			map[string]string{
				label.ComponentLabelKey: "pump",
				label.InstanceLabelKey:  pumpStatefulSet.Labels[label.InstanceLabelKey],
				label.NameLabelKey:      "tidb-cluster",
			},
		).String(),
	}

	pods, err := ctu.kubeCli.CoreV1().Pods(tc.Namespace).List(context.TODO(), listOps)
	if err != nil {
		log.Logf("ERROR: failed to get pods via pump labels %s ,%v", pumpStatefulSetName, err)
		return false, nil
	}

	for _, pod := range pods.Items {
		if !ctu.pumpHealth(tc, pod.Name) {
			log.Logf("ERROR: some pods is not health %s", pumpStatefulSetName)
			return false, nil
		}

		log.Logf("pod.Spec.Affinity: %v", pod.Spec.Affinity)
		if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAntiAffinity == nil || len(pod.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
			return true, fmt.Errorf("pump pod %s/%s should have affinity set", pod.Namespace, pod.Name)
		}
		log.Logf("pod.Spec.Tolerations: %v", pod.Spec.Tolerations)
		foundKey := false
		for _, tor := range pod.Spec.Tolerations {
			if tor.Key == "node-role" {
				foundKey = true
				break
			}
		}
		if !foundKey {
			return true, fmt.Errorf("pump pod %s/%s should have tolerations set", pod.Namespace, pod.Name)
		}
	}
	return true, nil
}

func (ctu *CrdTestUtil) pumpHealth(tc *v1alpha1.TidbCluster, podName string) bool {
	addr := fmt.Sprintf("%s.%s-pump.%s:8250", podName, tc.Name, tc.Namespace)
	pumpHealthURL := fmt.Sprintf("http://%s/status", addr)
	res, err := http.Get(pumpHealthURL)
	if err != nil {
		log.Logf("ERROR: cluster:[%s] call %s failed,error:%v", tc.Name, pumpHealthURL, err)
		return false
	}
	if res.StatusCode >= 400 {
		log.Logf("ERROR: Error response %v", res.StatusCode)
		return false
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		log.Logf("ERROR: cluster:[%s] read response body failed,error:%v", tc.Name, err)
		return false
	}
	healths := pumpStatus{}
	err = json.Unmarshal(body, &healths)
	if err != nil {
		log.Logf("ERROR: cluster:[%s] unmarshal failed,error:%v", tc.Name, err)
		return false
	}
	for _, status := range healths.StatusMap {
		if status.State != "online" {
			log.Logf("ERROR: cluster:[%s] pump's state is not online", tc.Name)
			return false
		}
	}
	return true
}

func (ctu *CrdTestUtil) CleanResourcesOrDie(resource, namespace string) {
	cmd := fmt.Sprintf("kubectl delete %s --all -n %s", resource, namespace)
	data, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%v, resp: %s", err, data)
		slack.NotifyAndPanic(err)
	}
}

func (ctu *CrdTestUtil) CreateSecretOrDie(secret *corev1.Secret) {
	_, err := ctu.kubeCli.CoreV1().Secrets(secret.Namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	if err != nil {
		slack.NotifyAndPanic(err)
	}
}
