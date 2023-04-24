/*
Copyright 2021 The Kubernetes Authors.

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

package controller

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/dynamic/dynamiclister"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
)

const (
	populatorContainerName  = "populate"
	populatorPodPrefix      = "populate"
	populatorPodVolumeName  = "target"
	populatorPvcPrefix      = "prime"
	populatedFromAnnoSuffix = "populated-from"
	pvcFinalizerSuffix      = "populate-target-protection"
	annSelectedNode         = "volume.kubernetes.io/selected-node"
	controllerNameSuffix    = "populator"

	reasonPopulatorCreationError   = "PopulatorCreationError"
	reasonPopulatorCreationSuccess = "PopulatorCreated"
	reasonPopulatorFailed          = "PopulatorFailed"
	reasonPopulatorFinished        = "PopulatorFinished"
	reasonPVCCreationError         = "PopulatorPVCCreationError"
)

type empty struct{}

type stringSet struct {
	set map[string]empty
}

type controller struct {
	kubeClient    kubernetes.Interface
	pvcLister     corelisters.PersistentVolumeClaimLister
	pvcSynced     cache.InformerSynced
	pvLister      corelisters.PersistentVolumeLister
	pvSynced      cache.InformerSynced
	scLister      storagelisters.StorageClassLister
	scSynced      cache.InformerSynced
	unstLister    dynamiclister.Lister
	unstSynced    cache.InformerSynced
	mu            sync.Mutex
	notifyMap     map[string]*stringSet
	cleanupMap    map[string]*stringSet
	workqueue     workqueue.RateLimitingInterface
	populatorArgs func(bool, *unstructured.Unstructured) ([]string, error)
	gk            schema.GroupKind
	recorder      record.EventRecorder
}

func RunController(masterURL, kubeconfig string,
	gk schema.GroupKind, gvr schema.GroupVersionResource) {
	klog.Infof("Starting populator controller for %s", gk)

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(stopCh)
		<-sigCh
		os.Exit(1) // second signal. Exit directly.
	}()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to create config: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Failed to create client: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Failed to create dynamic client: %v", err)
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	dynInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, time.Second*30)

	pvcInformer := kubeInformerFactory.Core().V1().PersistentVolumeClaims()
	pvInformer := kubeInformerFactory.Core().V1().PersistentVolumes()
	scInformer := kubeInformerFactory.Storage().V1().StorageClasses()
	unstInformer := dynInformerFactory.ForResource(gvr).Informer()

	c := &controller{
		kubeClient: kubeClient,
		pvcLister:  pvcInformer.Lister(),
		pvcSynced:  pvcInformer.Informer().HasSynced,
		pvLister:   pvInformer.Lister(),
		pvSynced:   pvInformer.Informer().HasSynced,
		scLister:   scInformer.Lister(),
		scSynced:   scInformer.Informer().HasSynced,
		unstLister: dynamiclister.New(unstInformer.GetIndexer(), gvr),
		unstSynced: unstInformer.HasSynced,
		notifyMap:  make(map[string]*stringSet),
		cleanupMap: make(map[string]*stringSet),
		workqueue:  workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		gk:         gk,
		recorder:   getRecorder(kubeClient, controllerNameSuffix),
	}

	pvcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handlePVC,
		UpdateFunc: func(old, new interface{}) {
			newPvc := new.(*corev1.PersistentVolumeClaim)
			oldPvc := old.(*corev1.PersistentVolumeClaim)
			if newPvc.ResourceVersion == oldPvc.ResourceVersion {
				return
			}
			c.handlePVC(new)
		},
		DeleteFunc: c.handlePVC,
	})

	pvInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handlePV,
		UpdateFunc: func(old, new interface{}) {
			newPv := new.(*corev1.PersistentVolume)
			oldPv := old.(*corev1.PersistentVolume)
			if newPv.ResourceVersion == oldPv.ResourceVersion {
				return
			}
			c.handlePV(new)
		},
		DeleteFunc: c.handlePV,
	})

	scInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleSC,
		UpdateFunc: func(old, new interface{}) {
			newSc := new.(*storagev1.StorageClass)
			oldSc := old.(*storagev1.StorageClass)
			if newSc.ResourceVersion == oldSc.ResourceVersion {
				return
			}
			c.handleSC(new)
		},
		DeleteFunc: c.handleSC,
	})

	unstInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleUnstructured,
		UpdateFunc: func(old, new interface{}) {
			newUnstructured := new.(*unstructured.Unstructured)
			oldUnstructured := old.(*unstructured.Unstructured)
			if newUnstructured.GetResourceVersion() == oldUnstructured.GetResourceVersion() {
				return
			}
			c.handleUnstructured(new)
		},
		DeleteFunc: c.handleUnstructured,
	})

	kubeInformerFactory.Start(stopCh)
	dynInformerFactory.Start(stopCh)

	if err = c.run(stopCh); err != nil {
		klog.Fatalf("Failed to run controller: %v", err)
	}
}

func getRecorder(kubeClient kubernetes.Interface, controllerName string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerName})
	return recorder
}

func (c *controller) addNotification(keyToCall, objType, namespace, name string) {
	var key string
	if 0 == len(namespace) {
		key = objType + "/" + name
	} else {
		key = objType + "/" + namespace + "/" + name
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.notifyMap[key]
	if s == nil {
		s = &stringSet{make(map[string]empty)}
		c.notifyMap[key] = s
	}
	s.set[keyToCall] = empty{}
	s = c.cleanupMap[keyToCall]
	if s == nil {
		s = &stringSet{make(map[string]empty)}
		c.cleanupMap[keyToCall] = s
	}
	s.set[key] = empty{}
}

func (c *controller) cleanupNotifications(keyToCall string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.cleanupMap[keyToCall]
	if s == nil {
		return
	}
	for key := range s.set {
		t := c.notifyMap[key]
		if t == nil {
			continue
		}
		delete(t.set, keyToCall)
		if 0 == len(t.set) {
			delete(c.notifyMap, key)
		}
	}
}

func translateObject(obj interface{}) metav1.Object {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return nil
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return nil
		}
	}
	return object
}

func (c *controller) handleMapped(obj interface{}, objType string) {
	object := translateObject(obj)
	if object == nil {
		return
	}
	var key string
	if len(object.GetNamespace()) == 0 {
		key = objType + "/" + object.GetName()
	} else {
		key = objType + "/" + object.GetNamespace() + "/" + object.GetName()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.notifyMap[key]; ok {
		for k := range s.set {
			c.workqueue.Add(k)
		}
	}
}

func (c *controller) handlePVC(obj interface{}) {
	c.handleMapped(obj, "pvc")
	object := translateObject(obj)
	if object == nil {
		return
	}
	c.workqueue.Add("pvc/" + object.GetNamespace() + "/" + object.GetName())
}

func (c *controller) handlePV(obj interface{}) {
	c.handleMapped(obj, "pv")
}

func (c *controller) handleSC(obj interface{}) {
	c.handleMapped(obj, "sc")
}

func (c *controller) handleUnstructured(obj interface{}) {
	c.handleMapped(obj, "unstructured")
}

func (c *controller) run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	ok := cache.WaitForCacheSync(stopCh, c.pvcSynced, c.pvSynced, c.scSynced, c.unstSynced)
	if !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh

	return nil
}

func (c *controller) runWorker() {
	processNextWorkItem := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		var err error
		parts := strings.Split(key, "/")
		switch parts[0] {
		case "pvc":
			if len(parts) != 3 {
				utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
				return nil
			}
			err = c.syncPvc(context.TODO(), key, parts[1], parts[2])
		default:
			utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
			return nil
		}
		if err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(obj)
		return nil
	}

	for {
		obj, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}
		err := processNextWorkItem(obj)
		if err != nil {
			utilruntime.HandleError(err)
		}
	}
}

func (c *controller) syncPvc(ctx context.Context, key, pvcNamespace, pvcName string) error {
	var err error

	var pvc *corev1.PersistentVolumeClaim
	pvc, err = c.pvcLister.PersistentVolumeClaims(pvcNamespace).Get(pvcName)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("pvc '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	dataSourceRef := pvc.Spec.DataSourceRef
	if dataSourceRef == nil {
		// Ignore PVCs without a datasource
		return nil
	}

	apiGroup := ""
	if dataSourceRef.APIGroup != nil {
		apiGroup = *dataSourceRef.APIGroup
	}
	if c.gk.Group != apiGroup || c.gk.Kind != dataSourceRef.Kind || "" == dataSourceRef.Name {
		// Ignore PVCs that aren't for this populator to handle
		fmt.Println("ignore PVCs that aren't for this populator to handle")
		return nil
	}

	replication, err := c.unstLister.Namespace(pvc.Namespace).Get(dataSourceRef.Name)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		c.addNotification(key, "unstructured", pvc.Namespace, dataSourceRef.Name)
		// We'll get called again later when the data source exists
		return nil
	}
	vr := VolumeReplication{}
	r := replication.UnstructuredContent()
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(r, &vr)
	if err != nil {
		fmt.Println("failed to unmarshal replication", replication)
		return err
	}
	var storageClass *storagev1.StorageClass
	if pvc.Spec.StorageClassName != nil {
		storageClassName := *pvc.Spec.StorageClassName

		storageClass, err = c.scLister.Get(storageClassName)
		if err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
			c.addNotification(key, "sc", "", storageClassName)
			// We'll get called again later when the storage class exists
			return nil
		}

		if err := c.checkIntreeStorageClass(pvc, storageClass); err != nil {
			klog.V(2).Infof("Ignoring PVC %s/%s: %s", pvcNamespace, pvcName, err)
			return nil
		}

	}

	// *** Here is the first place we start to create/modify objects ***

	// If the PVC is unbound, we need to perform the population
	if "" == pvc.Spec.VolumeName {

		// // Ensure the PVC has a finalizer on it so we can clean up the stuff we create
		// err = c.ensureFinalizer(ctx, pvc, c.pvcFinalizer, true)
		// if err != nil {
		// 	return err
		// }

		// Need to get the volume Replicaiton object and create PV from it.
		// Get PV
		//var pv *corev1.PersistentVolume

		pv := makePopulatePVSpec(&vr, pvc, storageClass)
		pv, err = c.kubeClient.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			c.recorder.Eventf(pvc, corev1.EventTypeWarning, reasonPopulatorCreationError, "Failed to create populator pv: %s", err)
			return err
		}

	}

	// *** At this point the volume population is done and we're just cleaning up ***
	c.recorder.Eventf(pvc, corev1.EventTypeNormal, reasonPopulatorFinished, "Populator finished")

	// Clean up our internal callback maps
	c.cleanupNotifications(key)

	return nil
}

func makePopulatePVSpec(vr *VolumeReplication, pvc *corev1.PersistentVolumeClaim, sc *storagev1.StorageClass) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: vr.Name,
			Annotations: map[string]string{
				"volume.beta.kubernetes.io/storage-provisioner": sc.Provisioner,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: pvc.Spec.AccessModes,
			Capacity:    pvc.Spec.Resources.Requests,
			ClaimRef: &corev1.ObjectReference{
				Namespace: pvc.Namespace,
				Name:      pvc.Name,
			},
			PersistentVolumeReclaimPolicy: *sc.ReclaimPolicy,
			StorageClassName:              *pvc.Spec.StorageClassName,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           sc.Provisioner,
					VolumeHandle:     vr.Spec.DataDestination.VolumeHandle,
					VolumeAttributes: vr.Spec.DataDestination.Context,
				},
			},
		},
	}
}

func (c *controller) checkIntreeStorageClass(pvc *corev1.PersistentVolumeClaim, sc *storagev1.StorageClass) error {
	if !strings.HasPrefix(sc.Provisioner, "kubernetes.io/") {
		// This is not an in-tree StorageClass
		return nil
	}

	if pvc.Annotations != nil {
		if migrated := pvc.Annotations[volume.AnnMigratedTo]; migrated != "" {
			// The PVC is migrated to CSI
			return nil
		}
	}

	// The SC is in-tree & PVC is not migrated
	return fmt.Errorf("in-tree volume volume plugin %q cannot use volume populator", sc.Provisioner)
}
