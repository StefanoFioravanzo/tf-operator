// Copyright 2018 The Kubeflow Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package controller provides a Kubernetes controller for a TensorFlow job resource.
package controller

import (
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	mxv1alpha1 "github.com/kubeflow/tf-operator/pkg/apis/mxnet/v1alpha1"
	mxjobclient "github.com/kubeflow/tf-operator/pkg/client/clientset/versioned"
	kubeflowscheme "github.com/kubeflow/tf-operator/pkg/client/clientset/versioned/scheme"
	informers "github.com/kubeflow/tf-operator/pkg/client/informers/externalversions"
	listers "github.com/kubeflow/tf-operator/pkg/client/listers/mxnet/v1alpha1"
	"github.com/kubeflow/tf-operator/pkg/trainer"

	"github.com/sabhiram/go-tracey"
)

var Exit, Enter = tracey.New(nil)

const (
	controllerName = "kubeflow"
)

var (
	ErrVersionOutdated = errors.New("requested version is outdated in apiserver")

	// IndexerInformer uses a delta queue, therefore for deletes we have to use this
	// key function but it should be just fine for non delete events.
	keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc

	// DefaultJobBackOff is the max backoff period, exported for the e2e test
	DefaultJobBackOff = 10 * time.Second
	// MaxJobBackOff is the max backoff period, exported for the e2e test
	MaxJobBackOff = 360 * time.Second
)

type Controller struct {
	KubeClient  kubernetes.Interface
	MXJobClient mxjobclient.Interface

	config mxv1alpha1.ControllerConfig
	jobs   map[string]*trainer.TrainingJob

	MXJobLister listers.MXJobLister
	MXJobSynced cache.InformerSynced

	// WorkQueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	WorkQueue workqueue.RateLimitingInterface

	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder

	syncHandler func(jobKey string) (bool, error)
}

func New(kubeClient kubernetes.Interface, mxJobClient mxjobclient.Interface,
	config mxv1alpha1.ControllerConfig, mxJobInformerFactory informers.SharedInformerFactory) (*Controller, error) {
	defer Exit(Enter("controller.go $FN"))
	mxJobInformer := mxJobInformerFactory.Kubeflow().V1alpha1().MXJobs()

	kubeflowscheme.AddToScheme(scheme.Scheme)  // TODO(stefano): what is a scheme?
	log.Debug("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(log.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: controllerName})

	controller := &Controller{
		KubeClient:  kubeClient,
		MXJobClient: mxJobClient,
		WorkQueue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "MXjobs"),
		recorder:    recorder,
		// TODO(jlewi)): What to do about cluster.Cluster?
		jobs:   make(map[string]*trainer.TrainingJob),
		config: config,
	}

	log.Info("Setting up event handlers")
	// Set up an event handler for when Foo resources change
	mxJobInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *mxv1alpha1.MXJob:
					log.Debugf("filter mxjob name: %v", t.Name)
					return true
				default:
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: controller.enqueueController,
				UpdateFunc: func(oldObj, newObj interface{}) {
					controller.enqueueController(newObj)
				},
				DeleteFunc: controller.enqueueController,
			},
		})

	controller.MXJobLister = mxJobInformer.Lister()
	controller.MXJobSynced = mxJobInformer.Informer().HasSynced
	controller.syncHandler = controller.syncMXJob

	return controller, nil
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer Exit(Enter("controller.go $FN"))
	defer runtime.HandleCrash()
	defer c.WorkQueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	log.Info("Starting MXJob controller")

	// Wait for the caches to be synced before starting workers
	log.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.MXJobSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	log.Infof("Starting %v workers", threadiness)
	// Launch workers to process MXJob resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	log.Info("Started workers")
	<-stopCh
	log.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	defer Exit(Enter("controller.go $FN"))
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	defer Exit(Enter("controller.go $FN"))
	key, quit := c.WorkQueue.Get()
	if quit {
		return false
	}
	defer c.WorkQueue.Done(key)

	forget, err := c.syncHandler(key.(string))  // here call syncMXJob
	if err == nil {
		if forget {
			c.WorkQueue.Forget(key)
		}
		return true
	}

	utilruntime.HandleError(fmt.Errorf("Error syncing job: %v", err))
	c.WorkQueue.AddRateLimited(key)

	return true
}

// syncTFJob will sync the job with the given. This function is not meant to be invoked
// concurrently with the same key.
//
// When a job is completely processed it will return true indicating that its ok to forget about this job since
// no more processing will occur for it.
func (c *Controller) syncMXJob(key string) (bool, error) {
	defer Exit(Enter("controller.go $FN, key: %s", key))
	startTime := time.Now()
	defer func() {
		log.Debugf("Finished syncing job %q (%v)", key, time.Since(startTime))
	}()

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return false, err
	}
	if len(ns) == 0 || len(name) == 0 {
		return false, fmt.Errorf("invalid job key %q: either namespace or name is missing", key)
	}

	mxJob, err := c.MXJobLister.MXJobs(ns).Get(name)

	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debugf("Job has been deleted: %v", key)
			return true, nil
		}
		return false, err
	}

	// Create a new TrainingJob if there is no TrainingJob stored for it in the jobs map or if the UID's don't match.
	// The UID's won't match in the event we deleted the job and then recreated the job with the same name.
	if cJob, ok := c.jobs[key]; !ok || cJob.UID() != mxJob.UID {
		nc, err := trainer.NewJob(c.KubeClient, c.MXJobClient, c.recorder, mxJob, &c.config)

		if err != nil {
			return false, err
		}
		c.jobs[key] = nc
	}

	nc := c.jobs[key]

	if err := nc.Reconcile(&c.config); err != nil {
		return false, err
	}

	mxJob, err = c.MXJobClient.KubeflowV1alpha1().MXJobs(mxJob.ObjectMeta.Namespace).Get(mxJob.ObjectMeta.Name, metav1.GetOptions{})

	if err != nil {
		return false, err
	}

	// TODO(jlewi): This logic will need to change when/if we get rid of phases and move to conditions. At that
	// case we should forget about a job when the appropriate condition is reached.
	if mxJob.Status.Phase == mxv1alpha1.MXJobPhaseCleanUp {
		return true, nil
	} else {
		return false, nil
	}

}

// obj could be an *batch.Job, or a DeletionFinalStateUnknown marker item.
func (c *Controller) enqueueController(obj interface{}) {
	defer Exit(Enter("controller.go $FN"))
	key, err := keyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	c.WorkQueue.AddRateLimited(key)
}
