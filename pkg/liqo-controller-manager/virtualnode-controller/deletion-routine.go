// Copyright 2019-2023 The Liqo Authors
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

// Package virtualnodectrl contains VirtualNode Controller logic and some functions for managing NamespaceMap lifecycle.
// There are also some tests for VirtualNode Controller
package virtualnodectrl

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	virtualkubeletv1alpha1 "github.com/liqotech/liqo/apis/virtualkubelet/v1alpha1"
	"github.com/liqotech/liqo/pkg/utils/getters"
)

var (
	deletionRoutineRunning = false
)

// DeletionRoutine is responsible for deleting a virtual node.
type DeletionRoutine struct {
	vnr                  *VirtualNodeReconciler
	wq                   workqueue.DelayingInterface
	virtualNodesDeleting map[string]interface{}
}

// RunDeletionRoutine starts the deletion routine.
func RunDeletionRoutine(ctx context.Context, r *VirtualNodeReconciler) (*DeletionRoutine, error) {
	if deletionRoutineRunning {
		return nil, fmt.Errorf("deletion routine already running")
	}
	deletionRoutineRunning = true
	dr := &DeletionRoutine{
		vnr:                  r,
		wq:                   workqueue.NewDelayingQueue(),
		virtualNodesDeleting: make(map[string]interface{}),
	}
	go dr.run(ctx)
	return dr, nil
}

func (dr *DeletionRoutine) run(ctx context.Context) {
	for {
		vni, _ := dr.wq.Get()
		vn := vni.(*virtualkubeletv1alpha1.VirtualNode)
		klog.Infof("Deletion routine started for virtual node %s", vn.Name)

		if err := UpdateCondition(ctx, dr.vnr.Client, vn,
			VkConditionMap{
				virtualkubeletv1alpha1.NodeConditionType: VkCondition{
					Status: virtualkubeletv1alpha1.DrainingConditionStatusType,
				},
			}); err != nil {
			dr.reEnqueueVirtualNode(vn, fmt.Errorf("error updating condition: %w", err))
			continue
		}

		node, err := getters.GetNodeFromVirtualNode(ctx, dr.vnr.Client, vn)
		if client.IgnoreNotFound(err) != nil {
			dr.reEnqueueVirtualNode(vn, fmt.Errorf("error getting node: %w", err))
			continue
		}

			if err := UpdateCondition(ctx, dr.vnr.Client, vn,
				VkConditionMap{
					virtualkubeletv1alpha1.NodeConditionType: VkCondition{
						Status: virtualkubeletv1alpha1.DeletingConditionStatusType,
					},
				}); err != nil {
				return dr.reEnqueueVirtualNode(vn, fmt.Errorf("error updating condition: %w", err))
			}
			if err := client.IgnoreNotFound(dr.vnr.Client.Delete(ctx, node, &client.DeleteOptions{})); err != nil {
				return dr.reEnqueueVirtualNode(vn, fmt.Errorf("error deleting node: %w", err))
			}

			klog.Infof("Node %s deleted", node.Name)
		}

		if !vn.DeletionTimestamp.IsZero() {
			if err := dr.vnr.ensureNamespaceMapAbsence(ctx, vn); err != nil {
				dr.reEnqueueVirtualNode(vn, fmt.Errorf("error deleting namespace map: %w", err))
				continue
			}
			err := dr.vnr.removeVirtualNodeFinalizer(ctx, vn)
			if err != nil {
				dr.reEnqueueVirtualNode(vn, fmt.Errorf("error removing finalizer: %w", err))
				continue
			}
		}

		delete(dr.virtualNodesDeleting, vn.Name)
		klog.Infof("Deletion routine completed for virtual node %s", vn.Name)
		dr.wq.Done(vn)
	}
}

// reEnqueueVirtualNode re-enqueues a virtual node in the deletion queue.
func (dr *DeletionRoutine) reEnqueueVirtualNode(vn *virtualkubeletv1alpha1.VirtualNode, err error) {
	if err != nil {
		klog.Error(err)
	}
	dr.wq.Done(vn)
	dr.wq.AddAfter(vn, 5*time.Second)
}

// EnsureNodeAbsence adds a virtual node to the deletion queue.
func (dr *DeletionRoutine) EnsureNodeAbsence(vn *virtualkubeletv1alpha1.VirtualNode) {
	if _, ok := dr.virtualNodesDeleting[vn.Name]; ok {
		return
	}
	dr.virtualNodesDeleting[vn.Name] = struct{}{}
	dr.wq.Add(vn)
}
