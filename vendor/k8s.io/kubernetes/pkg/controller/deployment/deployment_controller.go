/*
Copyright 2015 The Kubernetes Authors.

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

// Package deployment contains all the logic for handling Kubernetes Deployments.
// It implements a set of strategies (rolling, recreate) for deploying an application,
// the means to rollback to previous versions, proportional scaling for mitigating
// risk, cleanup policy, and other useful features of Deployments.
package deployment

import (
	"fmt"
	"reflect"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/client/cache"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	unversionedcore "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/core/unversioned"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/deployment/util"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/metrics"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/util/workqueue"
	"k8s.io/kubernetes/pkg/watch"
)

const (
	// FullDeploymentResyncPeriod means we'll attempt to recompute the required replicas
	// of all deployments.
	// This recomputation happens based on contents in the local caches.
	FullDeploymentResyncPeriod = 30 * time.Second
	// We must avoid creating new replica set / counting pods until the replica set / pods store has synced.
	// If it hasn't synced, to avoid a hot loop, we'll wait this long between checks.
	StoreSyncedPollPeriod = 100 * time.Millisecond
	// MaxRetries is the number of times a deployment will be retried before it is dropped out of the queue.
	MaxRetries = 5
)

// DeploymentController is responsible for synchronizing Deployment objects stored
// in the system with actual running replica sets and pods.
type DeploymentController struct {
	client        clientset.Interface
	eventRecorder record.EventRecorder

	// To allow injection of syncDeployment for testing.
	syncHandler func(dKey string) error

	// A store of deployments, populated by the dController
	dStore cache.StoreToDeploymentLister
	// Watches changes to all deployments
	dController *framework.Controller
	// A store of ReplicaSets, populated by the rsController
	rsStore cache.StoreToReplicaSetLister
	// Watches changes to all ReplicaSets
	rsController *framework.Controller
	// A store of pods, populated by the podController
	podStore cache.StoreToPodLister
	// Watches changes to all pods
	podController *framework.Controller

	// dStoreSynced returns true if the Deployment store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	dStoreSynced func() bool
	// rsStoreSynced returns true if the ReplicaSet store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	rsStoreSynced func() bool
	// podStoreSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	podStoreSynced func() bool

	// Deployments that need to be synced
	queue workqueue.RateLimitingInterface
}

// NewDeploymentController creates a new DeploymentController.
func NewDeploymentController(client clientset.Interface, resyncPeriod controller.ResyncPeriodFunc) *DeploymentController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	// TODO: remove the wrapper when every clients have moved to use the clientset.
	eventBroadcaster.StartRecordingToSink(&unversionedcore.EventSinkImpl{Interface: client.Core().Events("")})

	if client != nil && client.Core().GetRESTClient().GetRateLimiter() != nil {
		metrics.RegisterMetricAndTrackRateLimiterUsage("deployment_controller", client.Core().GetRESTClient().GetRateLimiter())
	}
	dc := &DeploymentController{
		client:        client,
		eventRecorder: eventBroadcaster.NewRecorder(api.EventSource{Component: "deployment-controller"}),
		queue:         workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}

	dc.dStore.Store, dc.dController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return dc.client.Extensions().Deployments(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return dc.client.Extensions().Deployments(api.NamespaceAll).Watch(options)
			},
		},
		&extensions.Deployment{},
		FullDeploymentResyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    dc.addDeploymentNotification,
			UpdateFunc: dc.updateDeploymentNotification,
			// This will enter the sync loop and no-op, because the deployment has been deleted from the store.
			DeleteFunc: dc.deleteDeploymentNotification,
		},
	)

	dc.rsStore.Store, dc.rsController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return dc.client.Extensions().ReplicaSets(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return dc.client.Extensions().ReplicaSets(api.NamespaceAll).Watch(options)
			},
		},
		&extensions.ReplicaSet{},
		resyncPeriod(),
		framework.ResourceEventHandlerFuncs{
			AddFunc:    dc.addReplicaSet,
			UpdateFunc: dc.updateReplicaSet,
			DeleteFunc: dc.deleteReplicaSet,
		},
	)

	dc.podStore.Indexer, dc.podController = framework.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return dc.client.Core().Pods(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return dc.client.Core().Pods(api.NamespaceAll).Watch(options)
			},
		},
		&api.Pod{},
		resyncPeriod(),
		framework.ResourceEventHandlerFuncs{
			AddFunc:    dc.addPod,
			UpdateFunc: dc.updatePod,
			DeleteFunc: dc.deletePod,
		},
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	dc.syncHandler = dc.syncDeployment
	dc.dStoreSynced = dc.dController.HasSynced
	dc.rsStoreSynced = dc.rsController.HasSynced
	dc.podStoreSynced = dc.podController.HasSynced
	return dc
}

// Run begins watching and syncing.
func (dc *DeploymentController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	go dc.dController.Run(stopCh)
	go dc.rsController.Run(stopCh)
	go dc.podController.Run(stopCh)

	// Wait for the rc and dc stores to sync before starting any work in this controller.
	ready := make(chan struct{})
	go dc.waitForSyncedStores(ready, stopCh)
	select {
	case <-ready:
	case <-stopCh:
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(dc.worker, time.Second, stopCh)
	}

	<-stopCh
	glog.Infof("Shutting down deployment controller")
	dc.queue.ShutDown()
}

func (dc *DeploymentController) waitForSyncedStores(ready chan<- struct{}, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	for !dc.dStoreSynced() || !dc.rsStoreSynced() || !dc.podStoreSynced() {
		select {
		case <-time.After(StoreSyncedPollPeriod):
		case <-stopCh:
			return
		}
	}

	close(ready)
}

func (dc *DeploymentController) addDeploymentNotification(obj interface{}) {
	d := obj.(*extensions.Deployment)
	glog.V(4).Infof("Adding deployment %s", d.Name)
	dc.enqueueDeployment(d)
}

func (dc *DeploymentController) updateDeploymentNotification(old, cur interface{}) {
	oldD := old.(*extensions.Deployment)
	glog.V(4).Infof("Updating deployment %s", oldD.Name)
	// Resync on deployment object relist.
	dc.enqueueDeployment(cur.(*extensions.Deployment))
}

func (dc *DeploymentController) deleteDeploymentNotification(obj interface{}) {
	d, ok := obj.(*extensions.Deployment)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("Couldn't get object from tombstone %+v", obj)
			return
		}
		d, ok = tombstone.Obj.(*extensions.Deployment)
		if !ok {
			glog.Errorf("Tombstone contained object that is not a Deployment %+v", obj)
			return
		}
	}
	glog.V(4).Infof("Deleting deployment %s", d.Name)
	dc.enqueueDeployment(d)
}

// addReplicaSet enqueues the deployment that manages a ReplicaSet when the ReplicaSet is created.
func (dc *DeploymentController) addReplicaSet(obj interface{}) {
	rs := obj.(*extensions.ReplicaSet)
	glog.V(4).Infof("ReplicaSet %s added.", rs.Name)
	if d := dc.getDeploymentForReplicaSet(rs); d != nil {
		dc.enqueueDeployment(d)
	}
}

// getDeploymentForReplicaSet returns the deployment managing the given ReplicaSet.
// TODO: Surface that we are ignoring multiple deployments for a given ReplicaSet.
func (dc *DeploymentController) getDeploymentForReplicaSet(rs *extensions.ReplicaSet) *extensions.Deployment {
	deployments, err := dc.dStore.GetDeploymentsForReplicaSet(rs)
	if err != nil || len(deployments) == 0 {
		glog.V(4).Infof("Error: %v. No deployment found for ReplicaSet %v, deployment controller will avoid syncing.", err, rs.Name)
		return nil
	}
	// Because all ReplicaSet's belonging to a deployment should have a unique label key,
	// there should never be more than one deployment returned by the above method.
	// If that happens we should probably dynamically repair the situation by ultimately
	// trying to clean up one of the controllers, for now we just return one of the two,
	// likely randomly.
	return &deployments[0]
}

// updateReplicaSet figures out what deployment(s) manage a ReplicaSet when the ReplicaSet
// is updated and wake them up. If the anything of the ReplicaSets have changed, we need to
// awaken both the old and new deployments. old and cur must be *extensions.ReplicaSet
// types.
func (dc *DeploymentController) updateReplicaSet(old, cur interface{}) {
	if api.Semantic.DeepEqual(old, cur) {
		// A periodic relist will send update events for all known controllers.
		return
	}
	// TODO: Write a unittest for this case
	curRS := cur.(*extensions.ReplicaSet)
	glog.V(4).Infof("ReplicaSet %s updated.", curRS.Name)
	if d := dc.getDeploymentForReplicaSet(curRS); d != nil {
		dc.enqueueDeployment(d)
	}
	// A number of things could affect the old deployment: labels changing,
	// pod template changing, etc.
	oldRS := old.(*extensions.ReplicaSet)
	if !api.Semantic.DeepEqual(oldRS, curRS) {
		if oldD := dc.getDeploymentForReplicaSet(oldRS); oldD != nil {
			dc.enqueueDeployment(oldD)
		}
	}
}

// deleteReplicaSet enqueues the deployment that manages a ReplicaSet when
// the ReplicaSet is deleted. obj could be an *extensions.ReplicaSet, or
// a DeletionFinalStateUnknown marker item.
func (dc *DeploymentController) deleteReplicaSet(obj interface{}) {
	rs, ok := obj.(*extensions.ReplicaSet)

	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the ReplicaSet
	// changed labels the new deployment will not be woken up till the periodic resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("Couldn't get object from tombstone %+v, could take up to %v before a deployment recreates/updates replicasets", obj, FullDeploymentResyncPeriod)
			return
		}
		rs, ok = tombstone.Obj.(*extensions.ReplicaSet)
		if !ok {
			glog.Errorf("Tombstone contained object that is not a ReplicaSet %+v, could take up to %v before a deployment recreates/updates replicasets", obj, FullDeploymentResyncPeriod)
			return
		}
	}
	glog.V(4).Infof("ReplicaSet %s deleted.", rs.Name)
	if d := dc.getDeploymentForReplicaSet(rs); d != nil {
		dc.enqueueDeployment(d)
	}
}

// getDeploymentForPod returns the deployment managing the ReplicaSet that manages the given Pod.
// TODO: Surface that we are ignoring multiple deployments for a given Pod.
func (dc *DeploymentController) getDeploymentForPod(pod *api.Pod) *extensions.Deployment {
	rss, err := dc.rsStore.GetPodReplicaSets(pod)
	if err != nil {
		glog.V(4).Infof("Error: %v. No ReplicaSets found for pod %v, deployment controller will avoid syncing.", err, pod.Name)
		return nil
	}
	for _, rs := range rss {
		deployments, err := dc.dStore.GetDeploymentsForReplicaSet(&rs)
		if err == nil && len(deployments) > 0 {
			return &deployments[0]
		}
	}
	glog.V(4).Infof("No deployments found for pod %v, deployment controller will avoid syncing.", pod.Name)
	return nil
}

// When a pod is created, ensure its controller syncs
func (dc *DeploymentController) addPod(obj interface{}) {
	pod, ok := obj.(*api.Pod)
	if !ok {
		return
	}
	glog.V(4).Infof("Pod %s created: %+v.", pod.Name, pod)
	if d := dc.getDeploymentForPod(pod); d != nil {
		dc.enqueueDeployment(d)
	}
}

// updatePod figures out what deployment(s) manage the ReplicaSet that manages the Pod when the Pod
// is updated and wake them up. If anything of the Pods have changed, we need to awaken both
// the old and new deployments. old and cur must be *api.Pod types.
func (dc *DeploymentController) updatePod(old, cur interface{}) {
	if api.Semantic.DeepEqual(old, cur) {
		return
	}
	curPod := cur.(*api.Pod)
	oldPod := old.(*api.Pod)
	glog.V(4).Infof("Pod %s updated %#v -> %#v.", curPod.Name, oldPod, curPod)
	if d := dc.getDeploymentForPod(curPod); d != nil {
		dc.enqueueDeployment(d)
	}
	if !api.Semantic.DeepEqual(oldPod, curPod) {
		if oldD := dc.getDeploymentForPod(oldPod); oldD != nil {
			dc.enqueueDeployment(oldD)
		}
	}
}

// When a pod is deleted, ensure its controller syncs.
// obj could be an *api.Pod, or a DeletionFinalStateUnknown marker item.
func (dc *DeploymentController) deletePod(obj interface{}) {
	pod, ok := obj.(*api.Pod)
	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the pod
	// changed labels the new ReplicaSet will not be woken up till the periodic
	// resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("Couldn't get object from tombstone %+v", obj)
			return
		}
		pod, ok = tombstone.Obj.(*api.Pod)
		if !ok {
			glog.Errorf("Tombstone contained object that is not a pod %+v", obj)
			return
		}
	}
	glog.V(4).Infof("Pod %s deleted: %+v.", pod.Name, pod)
	if d := dc.getDeploymentForPod(pod); d != nil {
		dc.enqueueDeployment(d)
	}
}

func (dc *DeploymentController) enqueueDeployment(deployment *extensions.Deployment) {
	key, err := controller.KeyFunc(deployment)
	if err != nil {
		glog.Errorf("Couldn't get key for object %+v: %v", deployment, err)
		return
	}

	// TODO: Handle overlapping deployments better. Either disallow them at admission time or
	// deterministically avoid syncing deployments that fight over ReplicaSet's. Currently, we
	// only ensure that the same deployment is synced for a given ReplicaSet. When we
	// periodically relist all deployments there will still be some ReplicaSet instability. One
	//  way to handle this is by querying the store for all deployments that this deployment
	// overlaps, as well as all deployments that overlap this deployments, and sorting them.
	dc.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (dc *DeploymentController) worker() {
	work := func() bool {
		key, quit := dc.queue.Get()
		if quit {
			return true
		}
		defer dc.queue.Done(key)

		err := dc.syncHandler(key.(string))
		dc.handleErr(err, key)

		return false
	}

	for {
		if quit := work(); quit {
			return
		}
	}
}

func (dc *DeploymentController) handleErr(err error, key interface{}) {
	if err == nil {
		dc.queue.Forget(key)
		return
	}

	if dc.queue.NumRequeues(key) < MaxRetries {
		glog.V(2).Infof("Error syncing deployment %v: %v", key, err)
		dc.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	dc.queue.Forget(key)
}

// syncDeployment will sync the deployment with the given key.
// This function is not meant to be invoked concurrently with the same key.
func (dc *DeploymentController) syncDeployment(key string) error {
	startTime := time.Now()
	defer func() {
		glog.V(4).Infof("Finished syncing deployment %q (%v)", key, time.Now().Sub(startTime))
	}()

	obj, exists, err := dc.dStore.Store.GetByKey(key)
	if err != nil {
		glog.Infof("Unable to retrieve deployment %v from store: %v", key, err)
		return err
	}
	if !exists {
		glog.Infof("Deployment has been deleted %v", key)
		return nil
	}

	deployment := obj.(*extensions.Deployment)
	everything := unversioned.LabelSelector{}
	if reflect.DeepEqual(deployment.Spec.Selector, &everything) {
		dc.eventRecorder.Eventf(deployment, api.EventTypeWarning, "SelectingAll", "This deployment is selecting all pods. A non-empty selector is required.")
		return nil
	}

	// Deep-copy otherwise we are mutating our cache.
	// TODO: Deep-copy only when needed.
	d, err := util.DeploymentDeepCopy(deployment)
	if err != nil {
		return err
	}

	if d.DeletionTimestamp != nil {
		return dc.syncStatusOnly(d)
	}

	if d.Spec.Paused {
		return dc.sync(d)
	}

	if d.Spec.RollbackTo != nil {
		revision := d.Spec.RollbackTo.Revision
		if _, err = dc.rollback(d, &revision); err != nil {
			return err
		}
	}

	if dc.isScalingEvent(d) {
		return dc.sync(d)
	}

	switch d.Spec.Strategy.Type {
	case extensions.RecreateDeploymentStrategyType:
		return dc.rolloutRecreate(d)
	case extensions.RollingUpdateDeploymentStrategyType:
		return dc.rolloutRolling(d)
	}
	return fmt.Errorf("unexpected deployment strategy type: %s", d.Spec.Strategy.Type)
}
