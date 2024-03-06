/*
Copyright 2022 The Koordinator Authors.

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

package elasticquota

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"

	"github.com/koordinator-sh/koordinator/apis/extension"
	utilclient "github.com/koordinator-sh/koordinator/pkg/util/client"
)

type quotaTopology struct {
	lock sync.Mutex
	// quotaInfoMap stores all quota information
	quotaInfoMap map[string]*QuotaInfo
	// namespaceMap key: annotationNamespace, val: quotaName
	namespaceToQuotaMap map[string]string
	// quotaHierarchyInfo stores the quota's all children
	quotaHierarchyInfo map[string]map[string]struct{}

	client client.Client
}

func NewQuotaTopology(client client.Client) *quotaTopology {
	topology := &quotaTopology{
		quotaInfoMap:        make(map[string]*QuotaInfo),
		quotaHierarchyInfo:  make(map[string]map[string]struct{}),
		namespaceToQuotaMap: make(map[string]string),
		client:              client,
	}
	topology.quotaHierarchyInfo[extension.RootQuotaName] = make(map[string]struct{})
	return topology
}

func (qt *quotaTopology) ValidAddQuota(quota *v1alpha1.ElasticQuota) error {
	if quota == nil {
		return fmt.Errorf("AddQuota param is nil")
	}

	qt.lock.Lock()
	defer qt.lock.Unlock()

	if _, exist := qt.quotaInfoMap[quota.Name]; exist {
		return fmt.Errorf("AddQuota quota already exist:%v", quota.Name)
	}

	annotationNamespaces := extension.GetAnnotationQuotaNamespaces(quota)
	for _, namespace := range annotationNamespaces {
		if quotaName, exist := qt.namespaceToQuotaMap[namespace]; exist {
			return fmt.Errorf("AddQuota quota %s's annotation namespace %s is already bound to quota %s", quota.Name, namespace, quotaName)
		}
	}

	if err := qt.validateQuotaSelfItem(quota); err != nil {
		return err
	}

	quotaInfo := NewQuotaInfoFromQuota(quota)

	if err := qt.validateQuotaTopology(nil, quotaInfo, nil); err != nil {
		return err
	}

	qt.quotaInfoMap[quotaInfo.Name] = quotaInfo
	qt.quotaHierarchyInfo[quotaInfo.Name] = make(map[string]struct{})
	qt.quotaHierarchyInfo[quotaInfo.ParentName][quotaInfo.Name] = struct{}{}
	for _, namespace := range annotationNamespaces {
		qt.namespaceToQuotaMap[namespace] = quota.Name
	}
	return nil
}

func (qt *quotaTopology) ValidUpdateQuota(oldQuota, newQuota *v1alpha1.ElasticQuota) error {
	if newQuota == nil {
		return fmt.Errorf("UpdateQuota param is nil")
	}

	if oldQuota != nil && reflect.DeepEqual(quotaFieldsCopy(oldQuota), quotaFieldsCopy(newQuota)) {
		return nil
	}

	quotaName := newQuota.Name

	if _, err := extension.IsForbiddenModify(newQuota); err != nil {
		return err
	}

	qt.lock.Lock()
	defer qt.lock.Unlock()

	annotationNamespaces := extension.GetAnnotationQuotaNamespaces(newQuota)
	for _, namespace := range annotationNamespaces {
		if oldQuotaName, exist := qt.namespaceToQuotaMap[namespace]; exist && oldQuotaName != quotaName {
			return fmt.Errorf("UpdadteQuota, quota %s update namespaces, but namespace %s is already bound to quota %s",
				quotaName, namespace, oldQuotaName)
		}
	}

	oldQuotaInfo, exist := qt.quotaInfoMap[quotaName]
	if !exist {
		return fmt.Errorf("UpdateQuota quota not exist in quotaInfoMap:%v", quotaName)
	}

	if err := qt.validateQuotaSelfItem(newQuota); err != nil {
		return err
	}

	oldAnnotationNamespaces := extension.GetAnnotationQuotaNamespaces(oldQuota)
	newQuotaInfo := NewQuotaInfoFromQuota(newQuota)
	if err := qt.validateQuotaTopology(oldQuotaInfo, newQuotaInfo, oldAnnotationNamespaces); err != nil {
		return err
	}

	qt.quotaInfoMap[quotaName] = newQuotaInfo
	if oldQuotaInfo.ParentName != newQuotaInfo.ParentName {
		delete(qt.quotaHierarchyInfo[oldQuotaInfo.ParentName], oldQuotaInfo.Name)
		qt.quotaHierarchyInfo[newQuotaInfo.ParentName][newQuotaInfo.Name] = struct{}{}
	}

	for _, namespace := range oldAnnotationNamespaces {
		delete(qt.namespaceToQuotaMap, namespace)
	}
	for _, namespace := range annotationNamespaces {
		qt.namespaceToQuotaMap[namespace] = quotaName
	}
	return nil
}

func (qt *quotaTopology) ValidDeleteQuota(quota *v1alpha1.ElasticQuota) error {
	qt.lock.Lock()
	defer qt.lock.Unlock()

	quotaName := quota.Name
	if quotaName == extension.SystemQuotaName || quotaName == extension.RootQuotaName || quotaName == extension.DefaultQuotaName {
		return fmt.Errorf("can not delete quotaGroup :%v", quotaName)
	}
	quotaInfo, exist := qt.quotaInfoMap[quotaName]
	if !exist {
		return fmt.Errorf("not found quota:%v", quotaName)
	}

	// check has child quota.
	if childSet, exist := qt.quotaHierarchyInfo[quotaName]; exist {
		if len(childSet) > 0 {
			return fmt.Errorf("delete quota failed, quota%v has child quota", quotaName)
		}
	} else {
		return fmt.Errorf("BUG quotaMap and quotaTree information out of sync, losed :%v", quotaName)
	}

	podList := &corev1.PodList{}
	opts := &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("label.quotaName", quota.Name),
	}
	err := qt.client.List(context.TODO(), podList, opts, utilclient.DisableDeepCopy)
	if err != nil {
		return fmt.Errorf("failed list pods for quota %v, err: %v", quota.Name, err)
	}
	if len(podList.Items) > 0 {
		return fmt.Errorf("delete quota failed, quota %v has child pods", quotaName)
	}

	delete(qt.quotaHierarchyInfo[quotaInfo.ParentName], quotaName)
	delete(qt.quotaHierarchyInfo, quotaName)
	delete(qt.quotaInfoMap, quotaName)
	annotationNamespaces := extension.GetAnnotationQuotaNamespaces(quota)
	for _, namespace := range annotationNamespaces {
		delete(qt.namespaceToQuotaMap, namespace)
	}
	return nil
}

// fillQuotaDefaultInformation fills quota with default information if not configure
func (qt *quotaTopology) fillQuotaDefaultInformation(quota *v1alpha1.ElasticQuota) error {
	if quota.Name == extension.RootQuotaName {
		return nil
	}

	qt.lock.Lock()
	defer qt.lock.Unlock()

	if quota.Labels == nil {
		quota.Labels = make(map[string]string)
	}
	if quota.Annotations == nil {
		quota.Annotations = make(map[string]string)
	}

	if parentName, exist := quota.Labels[extension.LabelQuotaParent]; !exist || len(parentName) == 0 {
		quota.Labels[extension.LabelQuotaParent] = extension.RootQuotaName
		klog.V(5).Infof("fill quota %v parent as root", quota.Name)
	}

	// add tree id, if the parent has tree id
	if quota.Labels[extension.LabelQuotaTreeID] == "" && quota.Labels[extension.LabelQuotaParent] != extension.RootQuotaName {
		parentInfo := qt.quotaInfoMap[quota.Labels[extension.LabelQuotaParent]]
		if parentInfo == nil {
			return fmt.Errorf("fill quota %v failed, parent not exist", quota.Name)
		}

		if parentInfo.TreeID != "" {
			quota.Labels[extension.LabelQuotaTreeID] = parentInfo.TreeID
		}
	}

	maxQuota, err := json.Marshal(&quota.Spec.Max)
	if err != nil {
		return fmt.Errorf("fillDefaultQuotaInfo marshal quota max failed:%v", err)
	}
	if sharedWeight, exist := quota.Annotations[extension.AnnotationSharedWeight]; !exist || len(sharedWeight) == 0 {
		quota.Annotations[extension.AnnotationSharedWeight] = string(maxQuota)
		klog.V(5).Infof("fill quota %v sharedWeight as max", quota.Name)
	}

	return nil
}

type QuotaTopologySummary struct {
	QuotaInfoMap       map[string]*QuotaInfoSummary `json:"quotaInfoMap"`
	QuotaHierarchyInfo map[string][]string          `json:"quotaHierarchyInfo"`
}

func NewQuotaTopologySummary() *QuotaTopologySummary {
	return &QuotaTopologySummary{
		QuotaInfoMap:       make(map[string]*QuotaInfoSummary),
		QuotaHierarchyInfo: make(map[string][]string),
	}
}

func (qt *quotaTopology) getQuotaTopologyInfo() *QuotaTopologySummary {
	result := NewQuotaTopologySummary()

	qt.lock.Lock()
	defer qt.lock.Unlock()

	for key, value := range qt.quotaInfoMap {
		result.QuotaInfoMap[key] = value.GetQuotaSummary()
	}

	for key, value := range qt.quotaHierarchyInfo {
		childQuotas := make([]string, 0, len(value))
		for name := range value {
			childQuotas = append(childQuotas, name)
		}
		result.QuotaHierarchyInfo[key] = childQuotas
	}
	return result
}

func (qt *quotaTopology) getQuotaInfo(name, namespace string) *QuotaInfo {
	qt.lock.Lock()
	defer qt.lock.Unlock()

	info, ok := qt.quotaInfoMap[name]
	if ok {
		return info
	}
	quotaName, ok := qt.namespaceToQuotaMap[namespace]
	if ok {
		return qt.quotaInfoMap[quotaName]
	}
	return nil
}
