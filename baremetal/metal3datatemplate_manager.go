/*
Copyright 2020 The Kubernetes Authors.

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

package baremetal

import (
	"context"
	"strconv"

	"github.com/go-logr/logr"

	capm3 "github.com/metal3-io/cluster-api-provider-metal3/api/v1alpha4"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/pointer"
	capi "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DataTemplateManagerInterface is an interface for a DataTemplateManager
type DataTemplateManagerInterface interface {
	SetFinalizer()
	UnsetFinalizer()
	RecreateStatusConditionally(context.Context) error
	DeleteDatas(context.Context) error
	CreateDatas(context.Context) error
	DeleteReady() (bool, error)
}

// DataTemplateManager is responsible for performing machine reconciliation
type DataTemplateManager struct {
	client       client.Client
	DataTemplate *capm3.Metal3DataTemplate
	Log          logr.Logger
}

// NewDataTemplateManager returns a new helper for managing a dataTemplate object
func NewDataTemplateManager(client client.Client,
	dataTemplate *capm3.Metal3DataTemplate, dataTemplateLog logr.Logger) (*DataTemplateManager, error) {

	return &DataTemplateManager{
		client:       client,
		DataTemplate: dataTemplate,
		Log:          dataTemplateLog,
	}, nil
}

// SetFinalizer sets finalizer
func (m *DataTemplateManager) SetFinalizer() {
	// If the Metal3Machine doesn't have finalizer, add it.
	if !Contains(m.DataTemplate.Finalizers, capm3.DataTemplateFinalizer) {
		m.DataTemplate.Finalizers = append(m.DataTemplate.Finalizers,
			capm3.DataTemplateFinalizer,
		)
	}
}

// UnsetFinalizer unsets finalizer
func (m *DataTemplateManager) UnsetFinalizer() {
	// Remove the finalizer.
	m.DataTemplate.Finalizers = Filter(m.DataTemplate.Finalizers,
		capm3.DataTemplateFinalizer,
	)
}

// RecreateStatusConditionally recreates the status if empty
func (m *DataTemplateManager) RecreateStatusConditionally(ctx context.Context) error {
	// If the status is empty (lastUpdated not set), then either the object is new
	// or has been moved. In both case, Recreating the status will set LastUpdated
	// so we won't recreate afterwards.
	if m.DataTemplate.Status.LastUpdated.IsZero() {
		return m.RecreateStatus(ctx)
	}
	return nil
}

// RecreateStatus recreates the status if empty
func (m *DataTemplateManager) RecreateStatus(ctx context.Context) error {

	m.Log.Info("Recreating the Metal3DataTemplate status")

	//start from empty maps
	m.DataTemplate.Status.Indexes = make(map[string]string)
	m.DataTemplate.Status.DataNames = make(map[string]string)

	// get list of Metal3Data objects
	dataObjects := capm3.Metal3DataList{}
	// without this ListOption, all namespaces would be including in the listing
	opts := &client.ListOptions{
		Namespace: m.DataTemplate.Namespace,
	}

	err := m.client.List(ctx, &dataObjects, opts)
	if err != nil {
		return err
	}

	// Iterate over the Metal3Data objects to find all indexes and objects
	for _, dataObject := range dataObjects.Items {

		// If DataTemplate does not point to this object, discard
		if dataObject.Spec.DataTemplate == nil {
			continue
		}
		if dataObject.Spec.DataTemplate.Name != m.DataTemplate.Name {
			continue
		}

		// Get the machine Name, if unset use empty string, to still record the
		// index being used, to avoid conflicts
		machineName := ""
		if dataObject.Spec.Metal3Machine != nil {
			machineName = dataObject.Spec.Metal3Machine.Name
		}
		m.DataTemplate.Status.Indexes[strconv.Itoa(dataObject.Spec.Index)] = machineName
		m.DataTemplate.Status.DataNames[machineName] = dataObject.Name
	}

	m.updateStatusTimestamp()
	return nil
}

func (m *DataTemplateManager) updateStatusTimestamp() {
	now := metav1.Now()
	m.DataTemplate.Status.LastUpdated = &now
}

// CreateDatas creates the missing secrets
func (m *DataTemplateManager) CreateDatas(ctx context.Context) error {
	requeueNeeded := false

	// Iterate over all ownerReferences
	for _, curOwnerRef := range m.DataTemplate.ObjectMeta.OwnerReferences {
		curOwnerRefGV, err := schema.ParseGroupVersion(curOwnerRef.APIVersion)
		if err != nil {
			return err
		}

		// If the owner is not a Metal3Machine of infrastructure.cluster.x-k8s.io
		// then discard
		if curOwnerRef.Kind != "Metal3Machine" ||
			curOwnerRefGV.Group != capm3.GroupVersion.Group {
			continue
		}

		// If the owner already has an entry, discard
		if _, ok := m.DataTemplate.Status.DataNames[curOwnerRef.Name]; ok {
			continue
		}

		m.Log.Info("Verifying the owner", "Metal3machine", curOwnerRef.Name)

		// Verify that we have an owner ref machine that points to this DataTemplate
		m3Machine, err := getM3Machine(ctx, m.client, m.Log,
			curOwnerRef.Name, m.DataTemplate.Namespace, m.DataTemplate, false,
		)
		if err != nil {
			return err
		}
		if m3Machine == nil {
			continue
		}

		// Get the cluster name
		clusterName, ok := m.DataTemplate.Labels[capi.ClusterLabelName]
		if !ok {
			return errors.New("No cluster name found on Metal3DataTemplate object")
		}

		if m.DataTemplate.Status.Indexes == nil {
			m.DataTemplate.Status.Indexes = make(map[string]string)
		}
		if m.DataTemplate.Status.DataNames == nil {
			m.DataTemplate.Status.DataNames = make(map[string]string)
		}
		// Get a new index for this machine
		m.Log.Info("Getting index", "Metal3machine", curOwnerRef.Name)
		machineIndexInt := len(m.DataTemplate.Status.Indexes)
		// The length of the map might be smaller than the highest index stored,
		// this means we have a gap to find
		for index := 0; index < len(m.DataTemplate.Status.Indexes); index++ {
			if _, ok := m.DataTemplate.Status.Indexes[strconv.Itoa(index)]; !ok {
				if machineIndexInt == len(m.DataTemplate.Status.Indexes) {
					machineIndexInt = index
					break
				}
			}
		}

		// Set the index and Metal3Data names
		machineIndex := strconv.Itoa(machineIndexInt)
		dataName := m.DataTemplate.Name + "-" + machineIndex
		m.DataTemplate.Status.Indexes[machineIndex] = curOwnerRef.Name
		m.DataTemplate.Status.DataNames[curOwnerRef.Name] = dataName

		m.Log.Info("Index", "Metal3machine", curOwnerRef.Name, "index", machineIndex)

		// Create the Metal3Data object, with an Owner ref to the Metal3Machine
		// (curOwnerRef) and to the Metal3DataTemplate
		dataObject := &capm3.Metal3Data{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Metal3Data",
				APIVersion: capm3.GroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      dataName,
				Namespace: m.DataTemplate.Namespace,
				Labels: map[string]string{
					capi.ClusterLabelName: clusterName,
				},
				OwnerReferences: []metav1.OwnerReference{
					curOwnerRef, metav1.OwnerReference{
						Controller: pointer.BoolPtr(true),
						APIVersion: m.DataTemplate.APIVersion,
						Kind:       m.DataTemplate.Kind,
						Name:       m.DataTemplate.Name,
						UID:        m.DataTemplate.UID,
					},
				},
			},
			Spec: capm3.Metal3DataSpec{
				Index: machineIndexInt,
				DataTemplate: &corev1.ObjectReference{
					Name:      m.DataTemplate.Name,
					Namespace: m.DataTemplate.Namespace,
				},
				Metal3Machine: &corev1.ObjectReference{
					Name:      curOwnerRef.Name,
					Namespace: m.DataTemplate.Namespace,
				},
			},
		}

		// Create the Metal3Data object. If we get a conflict (that will set
		// HasRequeueAfterError), then recreate the status because we are missing
		// an index, then requeue to retrigger the reconciliation with the new state
		if err := createObject(m.client, ctx, dataObject); err != nil {
			if _, ok := errors.Cause(err).(HasRequeueAfterError); ok {
				if err := m.RecreateStatus(ctx); err != nil {
					return err
				}
				requeueNeeded = true
				continue
			} else {
				return err
			}
		}
	}
	m.updateStatusTimestamp()
	if requeueNeeded {
		return &RequeueAfterError{}
	}
	return nil
}

// DeleteDatas deletes old secrets
func (m *DataTemplateManager) DeleteDatas(ctx context.Context) error {

	// Iterate over the Metal3Data objects
	for machineName, dataName := range m.DataTemplate.Status.DataNames {
		present := false
		// Iterate over the owner Refs
		for _, curOwnerRef := range m.DataTemplate.ObjectMeta.OwnerReferences {
			curOwnerRefGV, err := schema.ParseGroupVersion(curOwnerRef.APIVersion)
			if err != nil {
				return err
			}

			// If the owner ref is not a Metal3Machine, discard
			if curOwnerRef.Kind != "Metal3Machine" ||
				curOwnerRefGV.Group != capm3.GroupVersion.Group {
				continue
			}

			// If the names match, the Metal3Data should be preserved
			if machineName == curOwnerRef.Name {
				present = true
				break
			}
		}

		// Do not delete Metal3Data in use.
		if present {
			continue
		}

		m.Log.Info("Deleting Metal3data", "Metal3machine", machineName)

		// Try to get the Metal3Data. if it succeeds, delete it
		tmpM3Data := &capm3.Metal3Data{}
		key := client.ObjectKey{
			Name:      dataName,
			Namespace: m.DataTemplate.Namespace,
		}
		err := m.client.Get(ctx, key, tmpM3Data)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		} else if err == nil {
			// Delete the secret with metadata
			err = m.client.Delete(ctx, tmpM3Data)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}

		deletionIndex := ""
		for index, name := range m.DataTemplate.Status.Indexes {
			if name == machineName {
				deletionIndex = index
			}
		}
		delete(m.DataTemplate.Status.Indexes, deletionIndex)
		delete(m.DataTemplate.Status.DataNames, machineName)
		m.Log.Info("DataTemplate deleted", "Metal3machine", machineName)
	}
	m.updateStatusTimestamp()
	return nil
}

// DeleteRead returns true if the object is unreferenced (does not have
// Metal3Machine owner references)
func (m *DataTemplateManager) DeleteReady() (bool, error) {
	for _, curOwnerRef := range m.DataTemplate.ObjectMeta.OwnerReferences {
		curOwnerRefGV, err := schema.ParseGroupVersion(curOwnerRef.APIVersion)
		if err != nil {
			return false, err
		}

		// If we still have a Metal3Machine owning this, do not delete
		if curOwnerRef.Kind == "Metal3Machine" ||
			curOwnerRefGV.Group == capm3.GroupVersion.Group {
			return false, nil
		}
	}
	m.Log.Info("Metal3DataTemplate ready for deletion")
	return true, nil
}