/*
Copyright © 2018 inwinSTACK.inc

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

package namespace

import (
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/golang/glog"
	inwinv1 "github.com/inwinstack/ipam-operator/pkg/apis/inwinstack/v1"
	inwinclientset "github.com/inwinstack/ipam-operator/pkg/client/clientset/versioned/typed/inwinstack/v1"
	"github.com/inwinstack/ipam-operator/pkg/constants"
	"github.com/inwinstack/ipam-operator/pkg/util/k8sutil"
	"github.com/inwinstack/ipam-operator/pkg/util/slice"
	opkit "github.com/inwinstack/operator-kit"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

var Resource = opkit.CustomResource{
	Name:    "namespace",
	Plural:  "namespaces",
	Version: "v1",
	Kind:    reflect.TypeOf(v1.Namespace{}).Name(),
}

type NamespaceController struct {
	ctx       *opkit.Context
	clientset inwinclientset.InwinstackV1Interface
}

func NewController(ctx *opkit.Context, clientset inwinclientset.InwinstackV1Interface) *NamespaceController {
	return &NamespaceController{ctx: ctx, clientset: clientset}
}

func (c *NamespaceController) StartWatch(namespace string, stopCh chan struct{}) error {
	resourceHandlerFuncs := cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onAdd,
		UpdateFunc: c.onUpdate,
		DeleteFunc: c.onDelete,
	}

	glog.Infof("Start watching namespace resources.")
	watcher := opkit.NewWatcher(Resource, namespace, resourceHandlerFuncs, c.ctx.Clientset.CoreV1().RESTClient())
	go watcher.Watch(&v1.Namespace{}, stopCh)
	return nil
}

func (c *NamespaceController) onAdd(obj interface{}) {
	ns := obj.(*v1.Namespace).DeepCopy()
	glog.V(2).Infof("Namespace %s has added.", ns.Name)

	c.makeAnnotations(ns)

	if ns.Status.Phase != v1.NamespaceTerminating {
		if err := c.createOrDeleteIPs(ns); err != nil {
			glog.Errorf("Failed to create IPs in %s namespace: %s.", ns.Name, err)
		}
	}

	if _, err := c.ctx.Clientset.CoreV1().Namespaces().Update(ns); err != nil {
		glog.Errorf("Failed to update %s namespace: %s.", ns.Name, err)
	}
}

func (c *NamespaceController) onUpdate(oldObj, newObj interface{}) {
	ns := newObj.(*v1.Namespace).DeepCopy()
	glog.V(2).Infof("Received update on Namespace %s.", ns.Name)

	if ns.Status.Phase != v1.NamespaceTerminating {
		if err := c.createOrDeleteIPs(ns); err != nil {
			glog.Errorf("Failed to create IPs in %s namespace: %s.", ns.Name, err)
		}
	}

	_, refresh := ns.Annotations[constants.AllocateRefreshIPs]
	if refresh {
		if err := c.syncIPsToAnnotations(ns); err != nil {
			glog.Errorf("Failed to sync IPs in %s namespace: %s.", ns.Name, err)
		}
	}
}

func (c *NamespaceController) onDelete(obj interface{}) {
	ns := obj.(*v1.Namespace).DeepCopy()
	glog.V(2).Infof("Namespace %s has deleted.", ns.Name)
}

func (c *NamespaceController) makeAnnotations(ns *v1.Namespace) {
	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}

	if ns.Annotations[constants.AllocateNumberOfIP] == "" {
		ns.Annotations[constants.AllocateNumberOfIP] = strconv.Itoa(constants.DefaultNumberOfIP)
	}

	if ns.Annotations[constants.AllocatePoolName] == "" {
		ns.Annotations[constants.AllocatePoolName] = constants.DefaultPoolName
	}
}

func (c *NamespaceController) filterPoolIPs(ips *inwinv1.IPList, pool *inwinv1.Pool) *inwinv1.IPList {
	newIPs := &inwinv1.IPList{}

	for _, ip := range ips.Items {
		if ip.Spec.PoolName == pool.Name {
			newIPs.Items = append(newIPs.Items, ip)
		}
	}
	return newIPs
}

func (c *NamespaceController) createOrDeleteIPs(ns *v1.Namespace) error {
	poolName := ns.Annotations[constants.AllocatePoolName]
	pool, err := c.clientset.Pools().Get(poolName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if slice.Contains(pool.Spec.IgnoreNamespaces, ns.Name) || !pool.Spec.AutoAssignToNamespace {
		return nil
	}

	ips, err := c.clientset.IPs(ns.Name).List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	ips = c.filterPoolIPs(ips, pool)

	ipNumber, err := strconv.Atoi(ns.Annotations[constants.AllocateNumberOfIP])
	if err != nil {
		return err
	}

	// Create IPs
	for i := 0; i < (ipNumber - len(ips.Items)); i++ {
		ip := k8sutil.NewIPWithNamespace(ns, poolName)
		if _, err := c.clientset.IPs(ns.Name).Create(ip); err != nil {
			return err
		}
	}

	// Delete IPs
	for i := 0; i < (len(ips.Items) - ipNumber); i++ {
		ip := ips.Items[len(ips.Items)-(1+i)]
		if err := c.clientset.IPs(ns.Name).Delete(ip.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *NamespaceController) syncIPsToAnnotations(ns *v1.Namespace) error {
	poolName := ns.Annotations[constants.AllocatePoolName]
	pool, err := c.clientset.Pools().Get(poolName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if slice.Contains(pool.Spec.IgnoreNamespaces, ns.Name) || pool.Spec.IgnoreNamespaceAnnotation {
		return nil
	}

	ips, err := c.clientset.IPs(ns.Name).List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	ips = c.filterPoolIPs(ips, pool)

	sort.Slice(ips.Items, func(i, j int) bool {
		return ips.Items[i].Status.LastUpdateTime.Before(&ips.Items[j].Status.LastUpdateTime)
	})

	var newIPs []string
	for _, ip := range ips.Items {
		if ip.Status.Address != "" {
			newIPs = append(newIPs, ip.Status.Address)
		}
	}

	ns.Annotations[constants.AllocatedIPs] = ""
	ns.Annotations[constants.AllocatedLatestIP] = ""
	if len(newIPs) != 0 {
		ns.Annotations[constants.AllocatedIPs] = strings.Join(newIPs, ",")
		ns.Annotations[constants.AllocatedLatestIP] = newIPs[len(newIPs)-1]
	}

	delete(ns.Annotations, constants.AllocateRefreshIPs)
	if _, err := c.ctx.Clientset.CoreV1().Namespaces().Update(ns); err != nil {
		return err
	}
	return nil
}
