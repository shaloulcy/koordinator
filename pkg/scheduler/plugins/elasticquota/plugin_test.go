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
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	quotav1 "k8s.io/apiserver/pkg/quota/v1"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	schedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	schedulertesting "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"k8s.io/utils/pointer"

	"github.com/koordinator-sh/koordinator/apis/extension"
	"github.com/koordinator-sh/koordinator/apis/thirdparty/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
	pgclientset "github.com/koordinator-sh/koordinator/apis/thirdparty/scheduler-plugins/pkg/generated/clientset/versioned"
	pgfake "github.com/koordinator-sh/koordinator/apis/thirdparty/scheduler-plugins/pkg/generated/clientset/versioned/fake"
	"github.com/koordinator-sh/koordinator/pkg/client/clientset/versioned/fake"
	koordinatorinformers "github.com/koordinator-sh/koordinator/pkg/client/informers/externalversions"
	"github.com/koordinator-sh/koordinator/pkg/scheduler/apis/config"
	"github.com/koordinator-sh/koordinator/pkg/scheduler/apis/config/v1beta3"
	"github.com/koordinator-sh/koordinator/pkg/scheduler/frameworkext"
	"github.com/koordinator-sh/koordinator/pkg/scheduler/plugins/elasticquota/core"
)

type ElasticQuotaSetAndHandle struct {
	frameworkext.ExtendedHandle
	pgclientset.Interface
}

func mockPodsList(w http.ResponseWriter, r *http.Request) {
	bear := r.Header.Get("Authorization")
	if bear == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	parts := strings.Split(bear, "Bearer")
	if len(parts) != 2 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	http_token := strings.TrimSpace(parts[1])
	if len(http_token) < 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if http_token != token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	podList := new(corev1.PodList)
	b, err := json.Marshal(podList)
	if err != nil {
		log.Printf("codec error %+v", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func parseHostAndPort(rawURL string) (string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "0", err
	}
	return net.SplitHostPort(u.Host)
}

var (
	token string
)

type PluginTestOption func(elasticQuotaArgs *config.ElasticQuotaArgs)

func newPluginTestSuit(t *testing.T, nodes []*corev1.Node, pluginTestOpts ...PluginTestOption) *pluginTestSuit {
	setLoglevel("5")
	var v1beta3args v1beta3.ElasticQuotaArgs
	v1beta3.SetDefaults_ElasticQuotaArgs(&v1beta3args)
	var elasticQuotaArgs config.ElasticQuotaArgs
	err := v1beta3.Convert_v1beta3_ElasticQuotaArgs_To_config_ElasticQuotaArgs(&v1beta3args, &elasticQuotaArgs, nil)
	assert.NoError(t, err)

	for _, pluginTestOpt := range pluginTestOpts {
		pluginTestOpt(&elasticQuotaArgs)
	}

	elasticQuotaPluginConfig := schedulerconfig.PluginConfig{
		Name: Name,
		Args: &elasticQuotaArgs,
	}

	koordClientSet := fake.NewSimpleClientset()
	koordSharedInformerFactory := koordinatorinformers.NewSharedInformerFactory(koordClientSet, 0)
	extenderFactory, err := frameworkext.NewFrameworkExtenderFactory(
		frameworkext.WithKoordinatorClientSet(koordClientSet),
		frameworkext.WithKoordinatorSharedInformerFactory(koordSharedInformerFactory),
	)
	assert.Nil(t, err)
	pgClientSet := pgfake.NewSimpleClientset()
	proxyNew := frameworkext.PluginFactoryProxy(extenderFactory, func(configuration apiruntime.Object, f framework.Handle) (framework.Plugin, error) {
		return New(configuration, &ElasticQuotaSetAndHandle{
			ExtendedHandle: f.(frameworkext.ExtendedHandle),
			Interface:      pgClientSet,
		})
	})

	registeredPlugins := []schedulertesting.RegisterPluginFunc{
		func(reg *runtime.Registry, profile *schedulerconfig.KubeSchedulerProfile) {
			profile.PluginConfig = []schedulerconfig.PluginConfig{
				elasticQuotaPluginConfig,
			}
		},
		schedulertesting.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		schedulertesting.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		schedulertesting.RegisterPreFilterPlugin(Name, proxyNew),
	}

	cs := kubefake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	snapshot := newTestSharedLister(nil, nodes)

	server := httptest.NewTLSServer(http.HandlerFunc(mockPodsList))
	defer server.Close()

	address, portStr, err := parseHostAndPort(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &rest.Config{
		Host:        net.JoinHostPort(address, portStr),
		BearerToken: token,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}
	if token == "" {
		flag.StringVar(&token, "token", "mockTest", "")
		flag.Parse()
	}
	fh, err := schedulertesting.NewFramework(
		context.TODO(),
		registeredPlugins,
		"koord-scheduler",
		runtime.WithClientSet(cs),
		runtime.WithInformerFactory(informerFactory),
		runtime.WithSnapshotSharedLister(snapshot),
		runtime.WithKubeConfig(cfg),
	)
	assert.Nil(t, err)
	return &pluginTestSuit{
		Handle:                           fh,
		koordinatorSharedInformerFactory: koordSharedInformerFactory,
		proxyNew:                         proxyNew,
		elasticQuotaArgs:                 &elasticQuotaArgs,
		client:                           pgClientSet,
	}
}

func newPluginTestSuitWithPod(t *testing.T, nodes []*corev1.Node, pods []*corev1.Pod) *pluginTestSuit {
	setLoglevel("5")
	var v1beta3args v1beta3.ElasticQuotaArgs
	v1beta3.SetDefaults_ElasticQuotaArgs(&v1beta3args)
	var elasticQuotaArgs config.ElasticQuotaArgs
	err := v1beta3.Convert_v1beta3_ElasticQuotaArgs_To_config_ElasticQuotaArgs(&v1beta3args, &elasticQuotaArgs, nil)
	assert.NoError(t, err)

	elasticQuotaPluginConfig := schedulerconfig.PluginConfig{
		Name: Name,
		Args: &elasticQuotaArgs,
	}

	koordClientSet := fake.NewSimpleClientset()
	koordSharedInformerFactory := koordinatorinformers.NewSharedInformerFactory(koordClientSet, 0)

	pgClientSet := pgfake.NewSimpleClientset()

	extenderFactory, err := frameworkext.NewFrameworkExtenderFactory(
		frameworkext.WithKoordinatorClientSet(koordClientSet),
		frameworkext.WithKoordinatorSharedInformerFactory(koordSharedInformerFactory),
	)
	assert.Nil(t, err)
	proxyNew := frameworkext.PluginFactoryProxy(extenderFactory, func(configuration apiruntime.Object, f framework.Handle) (framework.Plugin, error) {
		return New(configuration, &ElasticQuotaSetAndHandle{
			ExtendedHandle: f.(frameworkext.ExtendedHandle),
			Interface:      pgClientSet,
		})
	})

	registeredPlugins := []schedulertesting.RegisterPluginFunc{
		func(reg *runtime.Registry, profile *schedulerconfig.KubeSchedulerProfile) {
			profile.PluginConfig = []schedulerconfig.PluginConfig{
				elasticQuotaPluginConfig,
			}
		},
		schedulertesting.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		schedulertesting.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		schedulertesting.RegisterPreFilterPlugin(Name, proxyNew),
	}

	cs := kubefake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	snapshot := newTestSharedLister(pods, nodes)

	server := httptest.NewTLSServer(http.HandlerFunc(mockPodsList))
	defer server.Close()

	address, portStr, err := parseHostAndPort(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &rest.Config{
		Host:        net.JoinHostPort(address, portStr),
		BearerToken: token,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}
	if token == "" {
		flag.StringVar(&token, "token", "mockTest", "")
		flag.Parse()
	}
	fh, err := schedulertesting.NewFramework(
		context.TODO(),
		registeredPlugins,
		"koord-scheduler",
		runtime.WithClientSet(cs),
		runtime.WithInformerFactory(informerFactory),
		runtime.WithSnapshotSharedLister(snapshot),
		runtime.WithKubeConfig(cfg),
		runtime.WithPodNominator(NewPodNominator()),
	)
	assert.Nil(t, err)
	return &pluginTestSuit{
		Handle:                           fh,
		koordinatorSharedInformerFactory: koordSharedInformerFactory,
		proxyNew:                         proxyNew,
		elasticQuotaArgs:                 &elasticQuotaArgs,
		client:                           pgClientSet,
		Framework:                        fh,
	}
}

var _ framework.SharedLister = &testSharedLister{}

type testSharedLister struct {
	nodes       []*corev1.Node
	nodeInfos   []*framework.NodeInfo
	nodeInfoMap map[string]*framework.NodeInfo
}

func (f *testSharedLister) StorageInfos() framework.StorageInfoLister {
	return f
}

func (f *testSharedLister) IsPVCUsedByPods(key string) bool {
	return false
}

func (f *testSharedLister) NodeInfos() framework.NodeInfoLister {
	return f
}

func (f *testSharedLister) List() ([]*framework.NodeInfo, error) {
	return f.nodeInfos, nil
}

func (f *testSharedLister) HavePodsWithAffinityList() ([]*framework.NodeInfo, error) {
	return nil, nil
}

func (f *testSharedLister) HavePodsWithRequiredAntiAffinityList() ([]*framework.NodeInfo, error) {
	return nil, nil
}

func (f *testSharedLister) Get(nodeName string) (*framework.NodeInfo, error) {
	return f.nodeInfoMap[nodeName], nil
}

func newTestSharedLister(pods []*corev1.Pod, nodes []*corev1.Node) *testSharedLister {
	nodeInfoMap := make(map[string]*framework.NodeInfo)
	nodeInfos := make([]*framework.NodeInfo, 0)
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, ok := nodeInfoMap[nodeName]; !ok {
			nodeInfoMap[nodeName] = framework.NewNodeInfo()
		}
		nodeInfoMap[nodeName].AddPod(pod)
	}
	for _, node := range nodes {
		if _, ok := nodeInfoMap[node.Name]; !ok {
			nodeInfoMap[node.Name] = framework.NewNodeInfo()
		}
		nodeInfoMap[node.Name].SetNode(node)
	}

	for _, v := range nodeInfoMap {
		nodeInfos = append(nodeInfos, v)
	}

	return &testSharedLister{
		nodes:       nodes,
		nodeInfos:   nodeInfos,
		nodeInfoMap: nodeInfoMap,
	}
}

type pluginTestSuit struct {
	framework.Handle
	framework.Framework
	koordinatorSharedInformerFactory koordinatorinformers.SharedInformerFactory
	proxyNew                         runtime.PluginFactory
	elasticQuotaArgs                 *config.ElasticQuotaArgs
	client                           *pgfake.Clientset
}

func TestNew(t *testing.T) {
	suit := newPluginTestSuit(t, nil)
	p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
	assert.NotNil(t, p)
	assert.Nil(t, err)
	assert.Equal(t, Name, p.Name())
}

func defaultCreateNodeWithLabels(nodeName string, labels map[string]string) *corev1.Node {
	node := defaultCreateNode(nodeName)
	node.ResourceVersion = "3"
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	for k, v := range labels {
		node.Labels[k] = v
	}
	return node
}

func defaultCreateNode(nodeName string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
		Status: corev1.NodeStatus{
			Allocatable: createResourceList(100, 1000),
		},
	}
}

func createResourceList(cpu, mem int64) corev1.ResourceList {
	return corev1.ResourceList{
		// use NewMilliQuantity to calculate the runtimeQuota correctly in cpu dimension
		// when the request is smaller than 1 core.
		corev1.ResourceCPU:    *resource.NewMilliQuantity(cpu*1000, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(mem, resource.BinarySI),
	}
}

func TestPlugin_OnQuotaAdd(t *testing.T) {
	suit := newPluginTestSuit(t, nil)
	p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
	assert.Nil(t, err)
	pl := p.(*Plugin)
	pl.groupQuotaManager.UpdateClusterTotalResource(createResourceList(501952056, 0))
	gqm := pl.groupQuotaManager
	quota := suit.AddQuota("1", "", 0, 0, 0, 0, 0, 0, false, "")
	assert.NotNil(t, gqm.GetQuotaInfoByName("1"))
	quota.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	quota.Name = "2"
	pl.OnQuotaAdd(quota)
	assert.Nil(t, gqm.GetQuotaInfoByName("2"))
}

func (p *pluginTestSuit) AddQuotaWithTreeID(name string, parentName string, maxCpu, maxMem int64,
	minCpu, minMem int64, scaleCpu, scaleMem int64, isParGroup bool, namespace, treeID string) *v1alpha1.ElasticQuota {
	quota := CreateQuota2(name, parentName, maxCpu, maxMem, minCpu, minMem, scaleCpu, scaleMem, isParGroup, treeID)
	p.client.SchedulingV1alpha1().ElasticQuotas(namespace).Create(context.TODO(), quota, metav1.CreateOptions{})
	time.Sleep(100 * time.Millisecond)
	return quota
}

func (p *pluginTestSuit) AddQuota(name string, parentName string, maxCpu, maxMem int64,
	minCpu, minMem int64, scaleCpu, scaleMem int64, isParGroup bool, namespace string) *v1alpha1.ElasticQuota {
	quota := CreateQuota2(name, parentName, maxCpu, maxMem, minCpu, minMem, scaleCpu, scaleMem, isParGroup, "")
	p.client.SchedulingV1alpha1().ElasticQuotas(namespace).Create(context.TODO(), quota, metav1.CreateOptions{})
	time.Sleep(100 * time.Millisecond)
	return quota
}

func (g *Plugin) addQuota(name string, parentName string, maxCpu, maxMem int64,
	minCpu, minMem int64, scaleCpu, scaleMem int64, isParGroup bool, namespace, tree string) *v1alpha1.ElasticQuota {
	quota := CreateQuota2(name, parentName, maxCpu, maxMem, minCpu, minMem, scaleCpu, scaleMem, isParGroup, tree)
	g.OnQuotaAdd(quota)
	return quota
}

func (g *Plugin) addRootQuota(name string, parentName string, maxCpu, maxMem int64,
	minCpu, minMem int64, scaleCpu, scaleMem int64, isParGroup bool, namespace, tree string) *v1alpha1.ElasticQuota {
	quota := CreateQuota2(name, parentName, maxCpu, maxMem, minCpu, minMem, scaleCpu, scaleMem, isParGroup, tree)

	quota.Labels[extension.LabelQuotaIsRoot] = "true"
	quota.Annotations[extension.AnnotationTotalResource] = fmt.Sprintf("{\"cpu\":%v, \"memory\":\"%v\"}", minCpu, minMem)

	g.OnQuotaAdd(quota)
	return quota
}

func CreateQuota2(name string, parentName string, maxCpu, maxMem int64, minCpu, minMem int64,
	scaleCpu, scaleMem int64, isParGroup bool, treeID string) *v1alpha1.ElasticQuota {
	quota := &v1alpha1.ElasticQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: make(map[string]string),
			Labels:      make(map[string]string),
		},
		Spec: v1alpha1.ElasticQuotaSpec{
			Max: createResourceList(maxCpu, maxMem),
			Min: createResourceList(minCpu, minMem),
		},
	}
	quota.Annotations[extension.AnnotationSharedWeight] = fmt.Sprintf("{\"cpu\":%v, \"memory\":\"%v\"}", scaleCpu, scaleMem)
	quota.Labels[extension.LabelQuotaParent] = parentName
	if isParGroup {
		quota.Labels[extension.LabelQuotaIsParent] = "true"
	} else {
		quota.Labels[extension.LabelQuotaIsParent] = "false"
	}
	if treeID != "" {
		quota.Labels[extension.LabelQuotaTreeID] = treeID
	}

	return quota
}

func TestPlugin_OnQuotaUpdate(t *testing.T) {
	suit := newPluginTestSuit(t, nil)
	p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
	assert.Nil(t, err)
	plugin := p.(*Plugin)
	gqm := plugin.groupQuotaManager
	// test2 Max[96, 160]  Min[100,160] request[20,40]
	//   `-- test2-a Max[96, 160]  Min[50,80] request[20,40]
	// test1 Max[96, 160]  Min[100,160] request[60,100]
	//   `-- test1-a Max[96, 160]  Min[50,80] request[60,100]
	//         `-- a-123 Max[96, 160]  Min[50,80] request[60,100]
	plugin.addQuota("test1", extension.RootQuotaName, 96, 160, 100, 160, 96, 160, true, "", "")
	plugin.addQuota("test1-a", "test1", 96, 160, 50, 80, 96, 160, true, "", "")
	changeQuota := plugin.addQuota("a-123", "test1-a", 96, 160, 50, 80, 96, 160, false, "", "")
	plugin.addQuota("test2", extension.RootQuotaName, 96, 160, 100, 160, 96, 160, true, "", "")
	mmQuota := plugin.addQuota("test2-a", "test2", 96, 160, 50, 80, 96, 160, false, "", "")
	gqm.UpdateClusterTotalResource(createResourceList(96, 160))
	request := createResourceList(60, 100)
	pod := makePod2("pod", request)
	pod.Labels[extension.LabelQuotaName] = "a-123"
	plugin.OnPodAdd(pod)
	runtime := gqm.RefreshRuntime("a-123")
	assert.Equal(t, request, runtime)

	runtime = gqm.RefreshRuntime("test1-a")
	assert.Equal(t, request, runtime)

	runtime = gqm.RefreshRuntime("test1")
	assert.Equal(t, request, runtime)

	// test2-a request [20,40]
	request = createResourceList(20, 40)
	pod1 := makePod2("pod1", request)
	pod1.Labels[extension.LabelQuotaName] = "test2-a"
	plugin.OnPodAdd(pod1)
	runtime = gqm.RefreshRuntime("test2-a")
	assert.Equal(t, request, runtime)

	runtime = gqm.RefreshRuntime("test2")
	assert.Equal(t, request, runtime)

	// a-123 mv test2
	// test2 Max[96, 160]  Min[100,160] request[80,140]
	//   `-- test2-a Max[96, 160]  Min[50,80] request[20,40]
	//   `-- a-123 Max[96, 160]  Min[50,80] request[60,100]
	// test1 Max[96, 160]  Min[100,160] request[0,0]
	//   `-- test1-a Max[96, 160]  Min[50,80] request[0,0]
	oldQuota := changeQuota.DeepCopy()
	changeQuota.Labels[extension.LabelQuotaParent] = "test2"
	changeQuota.ResourceVersion = "2"
	gqm.GetQuotaInfoByName("test1-a").IsParent = true

	plugin.OnQuotaUpdate(oldQuota, changeQuota)
	quotaInfo := gqm.GetQuotaInfoByName("test1-a")
	gqm.RefreshRuntime("test1-a")
	assert.Equal(t, createResourceList(0, 0), quotaInfo.GetRequest())
	assert.Equal(t, createResourceList(0, 0), quotaInfo.GetUsed())
	assert.Equal(t, createResourceList(0, 0), quotaInfo.GetRuntime())

	quotaInfo = gqm.GetQuotaInfoByName("test1")
	gqm.RefreshRuntime("test1")
	assert.Equal(t, createResourceList(0, 0), quotaInfo.GetRequest())
	assert.Equal(t, createResourceList(0, 0), quotaInfo.GetUsed())
	assert.Equal(t, createResourceList(0, 0), quotaInfo.GetRuntime())

	quotaInfo = gqm.GetQuotaInfoByName("a-123")
	gqm.RefreshRuntime("a-123")
	assert.Equal(t, createResourceList(60, 100), quotaInfo.GetRequest())
	assert.Equal(t, createResourceList(60, 100), quotaInfo.GetUsed())
	assert.Equal(t, createResourceList(60, 100), quotaInfo.GetRuntime())
	assert.Equal(t, "test2", quotaInfo.ParentName)

	quotaInfo = gqm.GetQuotaInfoByName("test2-a")
	gqm.RefreshRuntime("test2-a")
	assert.Equal(t, createResourceList(20, 40), quotaInfo.GetRequest())
	assert.Equal(t, createResourceList(20, 40), quotaInfo.GetUsed())
	assert.Equal(t, createResourceList(20, 40), quotaInfo.GetRuntime())

	quotaInfo = gqm.GetQuotaInfoByName("test2")
	gqm.RefreshRuntime("test2")
	assert.Equal(t, createResourceList(80, 140), quotaInfo.GetRequest())
	assert.Equal(t, createResourceList(80, 140), quotaInfo.GetUsed())
	assert.Equal(t, createResourceList(80, 140), quotaInfo.GetRuntime())
	changeQuota.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	plugin.OnQuotaUpdate(oldQuota, changeQuota)
	changeQuota.ResourceVersion = "3"
	plugin.OnQuotaUpdate(oldQuota, changeQuota)
	plugin.OnQuotaDelete(mmQuota)
	assert.Nil(t, gqm.GetQuotaInfoByName("test2-a"))
	quotaInfo = gqm.GetQuotaInfoByName("test2")
	gqm.RefreshRuntime("test2")
	assert.Equal(t, createResourceList(60, 100), quotaInfo.GetRequest())
	assert.Equal(t, createResourceList(60, 100), quotaInfo.GetUsed())
	assert.Equal(t, createResourceList(60, 100), quotaInfo.GetRuntime())
}

func TestPlugin_OnPodAdd_Update_Delete(t *testing.T) {
	suit := newPluginTestSuitWithPod(t, nil, nil)
	p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
	assert.Nil(t, err)
	plugin := p.(*Plugin)
	gqm := plugin.groupQuotaManager
	plugin.addQuota("test1", extension.RootQuotaName, 96, 160, 100, 160, 96, 160, true, "", "")
	plugin.addQuota("test2", extension.RootQuotaName, 96, 160, 100, 160, 96, 160, true, "", "")
	pods := []*corev1.Pod{
		defaultCreatePodWithQuotaName("1", "test1", 10, 10, 10),
		defaultCreatePodWithQuotaName("2", "test1", 10, 10, 10),
		defaultCreatePodWithQuotaName("3", "test1", 10, 10, 10),
		defaultCreatePodWithQuotaName("4", "test1", 10, 10, 10),
	}
	time.Sleep(100 * time.Millisecond)
	for _, pod := range pods {
		plugin.OnPodAdd(pod)
	}
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, gqm.GetQuotaInfoByName("test1").GetRequest(), createResourceList(40, 40))
	assert.Equal(t, 4, len(gqm.GetQuotaInfoByName("test1").PodCache))
	newPods := []*corev1.Pod{
		defaultCreatePodWithQuotaNameAndVersion("1", "test2", "2", 10, 10, 10),
		defaultCreatePodWithQuotaNameAndVersion("2", "test2", "2", 10, 10, 10),
		defaultCreatePodWithQuotaNameAndVersion("3", "test2", "2", 10, 10, 10),
		defaultCreatePodWithQuotaNameAndVersion("4", "test2", "2", 10, 10, 10),
	}
	for i, pod := range pods {
		plugin.OnPodUpdate(pod, newPods[i])
	}
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, len(gqm.GetQuotaInfoByName("test1").GetPodCache()))
	assert.Equal(t, 4, len(gqm.GetQuotaInfoByName("test2").GetPodCache()))
	for _, pod := range newPods {
		plugin.OnPodDelete(pod)
	}
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, len(gqm.GetQuotaInfoByName("test2").GetPodCache()), 0)
}

func setLoglevel(logLevel string) {
	var level klog.Level
	if err := level.Set(logLevel); err != nil {
		fmt.Printf("failed set klog.logging.verbosity %v: %v", logLevel, err)
	}
	fmt.Printf("successfully set klog.logging.verbosity to %v", logLevel)
}

func TestPlugin_PreFilter(t *testing.T) {
	test := []struct {
		name                string
		pod                 *corev1.Pod
		quotaInfo           *core.QuotaInfo
		expectedStatus      *framework.Status
		checkParent         bool
		disableRuntimeQuota bool
	}{
		{
			name: "default",
			pod: MakePod("t1-ns1", "pod1").Container(
				MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).Obj(),
			quotaInfo: &core.QuotaInfo{
				Name: extension.DefaultQuotaName,
				CalculateInfo: core.QuotaCalculateInfo{
					Runtime: MakeResourceList().CPU(0).Mem(20).Obj(),
				},
			},
			expectedStatus: framework.NewStatus(framework.Unschedulable, fmt.Sprintf("Insufficient quotas, "+
				"quotaName: %v, runtime: %v, used: %v, pod's request: %v, exceedDimensions: [cpu]",
				extension.DefaultQuotaName, printResourceList(MakeResourceList().CPU(0).Mem(20).Obj()),
				printResourceList(corev1.ResourceList{}), printResourceList(MakeResourceList().CPU(1).Mem(2).Obj()))),
		},
		{
			name: "used dimension larger than runtime, but value is enough",
			pod: MakePod("t1-ns1", "pod1").Container(
				MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).Obj(),
			quotaInfo: &core.QuotaInfo{
				Name: extension.DefaultQuotaName,
				CalculateInfo: core.QuotaCalculateInfo{
					Runtime: MakeResourceList().CPU(10).Mem(20).Obj(),
				},
			},
			expectedStatus: framework.NewStatus(framework.Success, ""),
		},
		{
			name: "value not enough",
			pod: MakePod("t1-ns1", "pod1").Container(
				MakeResourceList().CPU(1).Mem(3).GPU(1).Obj()).Obj(),
			quotaInfo: &core.QuotaInfo{
				Name: extension.DefaultQuotaName,
				CalculateInfo: core.QuotaCalculateInfo{
					Max:     MakeResourceList().CPU(10).Mem(20).Obj(),
					Runtime: MakeResourceList().CPU(1).Mem(2).Obj(),
				},
			},
			expectedStatus: framework.NewStatus(framework.Unschedulable,
				fmt.Sprintf("Insufficient quotas, "+
					"quotaName: %v, runtime: %v, used: %v, pod's request: %v, exceedDimensions: [memory]",
					extension.DefaultQuotaName, printResourceList(MakeResourceList().CPU(1).Mem(2).Obj()),
					printResourceList(corev1.ResourceList{}), printResourceList(MakeResourceList().CPU(1).Mem(3).Obj()))),
		},
		{
			name: "used dimension larger than runtime, but value is enough",
			pod: MakePod("t1-ns1", "pod1").Container(
				MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).Obj(),
			quotaInfo: &core.QuotaInfo{
				Name: extension.DefaultQuotaName,
				CalculateInfo: core.QuotaCalculateInfo{
					Runtime: MakeResourceList().CPU(10).Mem(20).Obj(),
				},
			},
			expectedStatus: framework.NewStatus(framework.Success, ""),
		},
		{
			name: "runtime not enough, but disable runtime",
			pod: MakePod("t1-ns1", "pod1").Container(
				MakeResourceList().CPU(1).Mem(3).GPU(1).Obj()).Obj(),
			quotaInfo: &core.QuotaInfo{
				Name: extension.DefaultQuotaName,
				CalculateInfo: core.QuotaCalculateInfo{
					Runtime: MakeResourceList().CPU(1).Mem(2).Obj(),
				},
			},
			disableRuntimeQuota: true,
			expectedStatus:      framework.NewStatus(framework.Success, ""),
		},
	}
	for _, tt := range test {
		t.Run(tt.name, func(t *testing.T) {
			suit := newPluginTestSuit(t, nil)
			p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
			assert.Nil(t, err)
			gp := p.(*Plugin)
			gp.pluginArgs.EnableRuntimeQuota = !tt.disableRuntimeQuota
			qi := gp.groupQuotaManager.GetQuotaInfoByName(tt.quotaInfo.Name)
			qi.Lock()
			qi.CalculateInfo.Runtime = tt.quotaInfo.CalculateInfo.Runtime.DeepCopy()
			qi.UnLock()
			state := framework.NewCycleState()
			ctx := context.TODO()
			_, status := gp.PreFilter(ctx, state, tt.pod)
			assert.Equal(t, status, tt.expectedStatus)
		})
	}
}

func TestPlugin_PreFilter_CheckParent(t *testing.T) {
	test := []struct {
		name           string
		pod            *corev1.Pod
		quotaInfo      *v1alpha1.ElasticQuota
		childRuntime   corev1.ResourceList
		parQuotaInfo   *v1alpha1.ElasticQuota
		parentRuntime  corev1.ResourceList
		expectedStatus framework.Status
	}{
		{
			name: "parent reject",
			pod: MakePod("t1-ns1", "pod1").Label(extension.LabelQuotaName, "test-child").Container(
				MakeResourceList().CPU(1).Mem(3).GPU(1).Obj()).Obj(),
			quotaInfo: &v1alpha1.ElasticQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-child",
					Labels: map[string]string{
						extension.LabelQuotaParent: "test",
					},
				},
				Spec: v1alpha1.ElasticQuotaSpec{
					Max: MakeResourceList().CPU(10).Mem(30).GPU(10).Obj(),
					Min: MakeResourceList().CPU(0).Mem(0).GPU(0).Obj(),
				},
			},
			childRuntime:  MakeResourceList().CPU(1).Mem(3).GPU(1).Obj(),
			parentRuntime: MakeResourceList().CPU(1).Mem(2).GPU(1).Obj(),
			parQuotaInfo: &v1alpha1.ElasticQuota{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
				},
				Spec: v1alpha1.ElasticQuotaSpec{
					Max: MakeResourceList().CPU(10).Mem(30).GPU(10).Obj(),
					Min: MakeResourceList().CPU(0).Mem(0).GPU(0).Obj(),
				},
			},
			expectedStatus: *framework.NewStatus(framework.Unschedulable,
				fmt.Sprintf("Insufficient quotas, "+
					"quotaNameTopo: %v, runtime: %v, used: %v, pod's request: %v, exceedDimensions: [memory]",
					[]string{"test", "test-child"}, printResourceList(MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()),
					printResourceList(corev1.ResourceList{}), printResourceList(MakeResourceList().CPU(1).Mem(3).GPU(1).Obj()))),
		},
	}
	for _, tt := range test {
		t.Run(tt.name, func(t *testing.T) {
			suit := newPluginTestSuit(t, nil)
			p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
			assert.Nil(t, err)
			gp := p.(*Plugin)
			gp.pluginArgs.EnableCheckParentQuota = true
			gp.OnQuotaAdd(tt.parQuotaInfo)
			gp.OnQuotaAdd(tt.quotaInfo)
			qi := gp.groupQuotaManager.GetQuotaInfoByName(tt.quotaInfo.Name)
			qi.Lock()
			qi.CalculateInfo.Runtime = tt.childRuntime.DeepCopy()
			qi.UnLock()
			qi1 := gp.groupQuotaManager.GetQuotaInfoByName(tt.parQuotaInfo.Name)
			qi1.Lock()
			qi1.CalculateInfo.Runtime = tt.parentRuntime.DeepCopy()
			qi1.UnLock()
			podRequests := core.PodRequests(tt.pod)
			status := *gp.checkQuotaRecursive(gp.groupQuotaManager, tt.quotaInfo.Name, []string{tt.quotaInfo.Name}, podRequests)
			assert.Equal(t, tt.expectedStatus, status)
		})
	}
}

func TestPlugin_Prefilter_QuotaNonPreempt(t *testing.T) {
	test := []struct {
		name           string
		pod            *corev1.Pod
		initPods       []*corev1.Pod
		quotaInfos     []*v1alpha1.ElasticQuota
		totalResource  corev1.ResourceList
		expectedStatus *framework.Status
	}{
		{
			name: "default",
			pod:  defaultCreatePodWithQuotaAndNonPreemptible("4", "test1", 1, 2, 2, true),
			initPods: []*corev1.Pod{
				defaultCreatePodWithQuotaAndNonPreemptible("1", "test1", 10, 2, 1, false),
				defaultCreatePodWithQuotaAndNonPreemptible("2", "test1", 9, 1, 1, false),
				defaultCreatePodWithQuotaAndNonPreemptible("3", "test1", 8, 1, 1, false),
			},
			quotaInfos: []*v1alpha1.ElasticQuota{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test1",
					},
					Spec: v1alpha1.ElasticQuotaSpec{
						Max: MakeResourceList().CPU(10).Mem(10).Obj(),
						Min: MakeResourceList().CPU(5).Mem(5).Obj(),
					},
				},
			},
			totalResource:  createResourceList(10, 10),
			expectedStatus: framework.NewStatus(framework.Success, ""),
		},
		{
			name: "non-preemptible pod used larger than min",
			pod:  defaultCreatePodWithQuotaAndNonPreemptible("4", "test1", 1, 2, 2, true),
			initPods: []*corev1.Pod{
				defaultCreatePodWithQuotaAndNonPreemptible("1", "test1", 10, 2, 1, false),
				defaultCreatePodWithQuotaAndNonPreemptible("2", "test1", 9, 2, 1, true),
				defaultCreatePodWithQuotaAndNonPreemptible("3", "test1", 9, 2, 1, true),
			},
			quotaInfos: []*v1alpha1.ElasticQuota{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test1",
					},
					Spec: v1alpha1.ElasticQuotaSpec{
						Max: MakeResourceList().CPU(10).Mem(8).Obj(),
						Min: MakeResourceList().CPU(5).Mem(5).Obj(),
					},
				},
			},
			totalResource: createResourceList(8, 5),
			expectedStatus: framework.NewStatus(framework.Unschedulable,
				fmt.Sprintf("Insufficient non-preemptible quotas, "+
					"quotaName: %v, min: %v, nonPreemptibleUsed: %v, pod's request: %v, exceedDimensions: [cpu]",
					"test1", printResourceList(MakeResourceList().CPU(5).Mem(5).Obj()),
					printResourceList(MakeResourceList().CPU(4).Mem(2).Obj()), printResourceList(MakeResourceList().CPU(2).Mem(2).Obj()))),
		},
		{
			name: "non-preemptible pod will not be evicted",
			pod:  defaultCreatePodWithQuotaAndNonPreemptible("4", "test1", 10, 2, 1, true),
			initPods: []*corev1.Pod{
				defaultCreatePodWithQuotaAndNonPreemptible("1", "test1", 10, 2, 1, false),
				defaultCreatePodWithQuotaAndNonPreemptible("2", "test1", 4, 2, 1, false),
				defaultCreatePodWithQuotaAndNonPreemptible("3", "test1", 1, 2, 2, true),
			},
			quotaInfos: []*v1alpha1.ElasticQuota{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test1",
					},
					Spec: v1alpha1.ElasticQuotaSpec{
						Max: MakeResourceList().CPU(10).Mem(8).Obj(),
						Min: MakeResourceList().CPU(5).Mem(5).Obj(),
					},
				},
			},
			totalResource: createResourceList(7, 5),
			expectedStatus: framework.NewStatus(framework.Unschedulable,
				fmt.Sprintf("Insufficient quotas, "+
					"quotaName: %v, runtime: %v, used: %v, pod's request: %v, exceedDimensions: [cpu]",
					"test1", printResourceList(MakeResourceList().CPU(7).Mem(5).Obj()),
					printResourceList(MakeResourceList().CPU(6).Mem(4).Obj()), printResourceList(MakeResourceList().CPU(2).Mem(1).Obj()))),
		},
	}
	for _, tt := range test {
		t.Run(tt.name, func(t *testing.T) {
			suit := newPluginTestSuit(t, nil)
			p, _ := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
			gp := p.(*Plugin)
			gp.groupQuotaManager.UpdateClusterTotalResource(tt.totalResource)
			for _, qis := range tt.quotaInfos {
				gp.OnQuotaAdd(qis)
			}

			for _, pod := range tt.initPods {
				gp.OnPodAdd(pod)
			}
			tt.pod.Spec.NodeName = ""
			gp.OnPodAdd(tt.pod)

			state := framework.NewCycleState()
			ctx := context.TODO()
			_, status := gp.PreFilter(ctx, state, tt.pod)
			assert.Equal(t, status, tt.expectedStatus)
		})
	}
}

func TestPlugin_Reserve(t *testing.T) {
	test := []struct {
		name         string
		pod          *corev1.Pod
		quotaInfo    *core.QuotaInfo
		expectedUsed corev1.ResourceList
	}{
		{
			name: "basic",
			pod: MakePod("t1-ns1", "pod1").Container(
				MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).UID("pod1").Obj(),
			quotaInfo: &core.QuotaInfo{
				Name: extension.DefaultQuotaName,
				CalculateInfo: core.QuotaCalculateInfo{
					Used: MakeResourceList().CPU(10).Mem(20).Obj(),
				},
			},
			expectedUsed: MakeResourceList().CPU(11).Mem(22).Obj(),
		},
	}
	for _, tt := range test {
		t.Run(tt.name, func(t *testing.T) {
			suit := newPluginTestSuit(t, nil)
			p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
			assert.Nil(t, err)
			gp := p.(*Plugin)
			pod := makePod2("pod", tt.quotaInfo.CalculateInfo.Used)
			gp.OnPodAdd(pod)
			gp.OnPodAdd(tt.pod)
			ctx := context.TODO()
			gp.Reserve(ctx, framework.NewCycleState(), tt.pod, "")
			assert.Equal(t, gp.groupQuotaManager.GetQuotaInfoByName(tt.quotaInfo.Name).GetUsed(), tt.expectedUsed)
		})
	}
}

func TestPlugin_Unreserve(t *testing.T) {
	test := []struct {
		name         string
		pod          *corev1.Pod
		quotaInfo    *core.QuotaInfo
		expectStatus bool
	}{
		{
			name: "basic",
			pod: MakePod("t1-ns1", "pod1").Container(
				MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).Phase(corev1.PodRunning).UID("pod1").Obj(),
			quotaInfo: &core.QuotaInfo{
				Name: extension.DefaultQuotaName,
				CalculateInfo: core.QuotaCalculateInfo{
					Used: MakeResourceList().CPU(10).Mem(20).GPU(10).Obj(),
				},
				PodCache: make(map[string]*core.PodInfo),
			},
			expectStatus: false,
		},
	}
	for _, tt := range test {
		t.Run(tt.name, func(t *testing.T) {
			suit := newPluginTestSuit(t, nil)
			p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
			assert.Nil(t, err)
			gp := p.(*Plugin)
			ctx := context.TODO()
			gp.OnPodAdd(tt.pod)
			gp.Reserve(ctx, framework.NewCycleState(), tt.pod, "")
			assert.True(t, gp.groupQuotaManager.GetQuotaInfoByName(tt.quotaInfo.Name).CheckPodIsAssigned(tt.pod))
			gp.Unreserve(ctx, framework.NewCycleState(), tt.pod, "")
			assert.False(t, gp.groupQuotaManager.GetQuotaInfoByName(tt.quotaInfo.Name).CheckPodIsAssigned(tt.pod))
		})
	}
}

func TestPlugin_AddPod(t *testing.T) {
	test := []struct {
		name              string
		podInfo           *framework.PodInfo
		shouldAdd         bool
		quotaInfo         *core.QuotaInfo
		wantStatusSuccess bool
		expectedUsed      corev1.ResourceList
	}{
		{
			name: "basic",
			podInfo: &framework.PodInfo{
				Pod: MakePod("t1-ns1", "pod1").Container(
					MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).
					Label(extension.LabelQuotaName, "t1-eq1").UID("1").Obj(),
			},
			shouldAdd: true,
			quotaInfo: &core.QuotaInfo{
				Name: extension.DefaultQuotaName,
				CalculateInfo: core.QuotaCalculateInfo{
					Used: MakeResourceList().CPU(10).Mem(20).Obj(),
				},
				PodCache: make(map[string]*core.PodInfo),
			},
			wantStatusSuccess: true,
			expectedUsed:      MakeResourceList().CPU(11).Mem(22).Obj(),
		},
	}
	for _, tt := range test {
		t.Run(tt.name, func(t *testing.T) {
			suit := newPluginTestSuit(t, nil)
			p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
			assert.Nil(t, err)
			gp := p.(*Plugin)
			pod := makePod2("test", tt.quotaInfo.CalculateInfo.Used)
			gp.OnPodAdd(pod)
			if tt.shouldAdd {
				gp.OnPodAdd(tt.podInfo.Pod)
			}
			state := framework.NewCycleState()
			ctx := context.TODO()
			quotaName := gp.getPodAssociateQuotaName(tt.podInfo.Pod)
			quotaInfo := gp.groupQuotaManager.GetQuotaInfoByName(quotaName)
			gp.snapshotPostFilterState(quotaInfo, state)
			status := gp.AddPod(ctx, state, nil, tt.podInfo, nil)
			assert.Equal(t, tt.wantStatusSuccess, status.IsSuccess())
			data, _ := getPostFilterState(state)
			assert.Equal(t, tt.expectedUsed, data.used)
		})
	}
}

func TestPlugin_RemovePod(t *testing.T) {
	test := []struct {
		name              string
		podInfo           *framework.PodInfo
		shouldAdd         bool
		quotaInfo         *core.QuotaInfo
		wantStatusSuccess bool
		expectedUsed      corev1.ResourceList
	}{
		{
			name: "basic",
			podInfo: &framework.PodInfo{
				Pod: MakePod("t1-ns1", "pod1").Container(
					MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).
					Label(extension.LabelQuotaName, "t1-eq1").UID("1").Phase(corev1.PodRunning).Obj(),
			},
			shouldAdd: true,
			quotaInfo: &core.QuotaInfo{
				Name: extension.DefaultQuotaName,
				CalculateInfo: core.QuotaCalculateInfo{
					Used: MakeResourceList().CPU(10).Mem(20).Obj(),
				},
			},
			wantStatusSuccess: true,
			expectedUsed:      MakeResourceList().CPU(9).Mem(18).Obj(),
		},
	}
	for _, tt := range test {
		t.Run(tt.name, func(t *testing.T) {
			suit := newPluginTestSuit(t, nil)
			p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
			assert.Nil(t, err)
			gp := p.(*Plugin)
			pod := makePod2("pod", tt.quotaInfo.CalculateInfo.Used)
			gp.OnPodAdd(pod)
			if tt.shouldAdd {
				gp.OnPodAdd(tt.podInfo.Pod)
			}
			state := framework.NewCycleState()
			ctx := context.TODO()
			quotaName := gp.getPodAssociateQuotaName(tt.podInfo.Pod)
			quotaInfo := gp.groupQuotaManager.GetQuotaInfoByName(quotaName)
			gp.snapshotPostFilterState(quotaInfo, state)
			status := gp.RemovePod(ctx, state, nil, tt.podInfo, nil)
			assert.Equal(t, tt.wantStatusSuccess, status.IsSuccess())
			data, _ := getPostFilterState(state)
			assert.Equal(t, tt.expectedUsed, data.used)
		})
	}
}

func TestPlugin_createDefaultQuotaIfNotPresent(t *testing.T) {
	suit := newPluginTestSuit(t, nil)
	eq, _ := suit.client.SchedulingV1alpha1().ElasticQuotas(suit.elasticQuotaArgs.QuotaGroupNamespace).Get(context.TODO(), extension.DefaultQuotaName, metav1.GetOptions{})
	if !quotav1.Equals(eq.Spec.Max, suit.elasticQuotaArgs.DefaultQuotaGroupMax) {
		t.Errorf("error")
	}
}

func TestPlugin_createSystemQuotaIfNotPresent(t *testing.T) {
	suit := newPluginTestSuit(t, nil)
	eq, _ := suit.client.SchedulingV1alpha1().ElasticQuotas(suit.elasticQuotaArgs.QuotaGroupNamespace).Get(context.TODO(), extension.SystemQuotaName, metav1.GetOptions{})
	if !quotav1.Equals(eq.Spec.Max, suit.elasticQuotaArgs.SystemQuotaGroupMax) {
		t.Errorf("error")
	}
}

func makePod2(podName string, request corev1.ResourceList) *corev1.Pod {
	pause := imageutils.GetPauseImageName()
	pod := schedulertesting.MakePod().Namespace(extension.DefaultQuotaName).Name(podName).Container(pause).ZeroTerminationGracePeriod().Obj()
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: request,
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Spec.NodeName = "testNode"
	pod.Labels = make(map[string]string)
	return pod
}

// nominatedPodMap is a structure that stores pods nominated to run on nodes.
// It exists because nominatedNodeName of pod objects stored in the structure
// may be different than what scheduler has here. We should be able to find pods
// by their UID and update/delete them.
type nominatedPodMap struct {
	// nominatedPods is a map keyed by a node name and the value is a list of
	// pods which are nominated to run on the node. These are pods which can be in
	// the activeQ or unschedulableQ.
	nominatedPods map[string][]*framework.PodInfo
	// nominatedPodToNode is map keyed by a Pod UID to the node name where it is
	// nominated.
	nominatedPodToNode map[types.UID]string

	sync.RWMutex
}

func (npm *nominatedPodMap) add(pi *framework.PodInfo, nodeName string) {
	// always delete the pod if it already exist, to ensure we never store more than
	// one instance of the pod.
	npm.delete(pi.Pod)

	nnn := nodeName
	if len(nnn) == 0 {
		nnn = NominatedNodeName(pi.Pod)
		if len(nnn) == 0 {
			return
		}
	}
	npm.nominatedPodToNode[pi.Pod.UID] = nnn
	for _, npi := range npm.nominatedPods[nnn] {
		if npi.Pod.UID == pi.Pod.UID {
			klog.V(4).InfoS("Pod already exists in the nominated map", "pod", klog.KObj(npi.Pod))
			return
		}
	}
	npm.nominatedPods[nnn] = append(npm.nominatedPods[nnn], pi)
}

func (npm *nominatedPodMap) delete(p *corev1.Pod) {
	nnn, ok := npm.nominatedPodToNode[p.UID]
	if !ok {
		return
	}
	for i, np := range npm.nominatedPods[nnn] {
		if np.Pod.UID == p.UID {
			npm.nominatedPods[nnn] = append(npm.nominatedPods[nnn][:i], npm.nominatedPods[nnn][i+1:]...)
			if len(npm.nominatedPods[nnn]) == 0 {
				delete(npm.nominatedPods, nnn)
			}
			break
		}
	}
	delete(npm.nominatedPodToNode, p.UID)
}

// UpdateNominatedPod updates the <oldPod> with <newPod>.
func (npm *nominatedPodMap) UpdateNominatedPod(logr klog.Logger, oldPod *corev1.Pod, newPodInfo *framework.PodInfo) {
	npm.Lock()
	defer npm.Unlock()
	// In some cases, an Update event with no "NominatedNode" present is received right
	// after a node("NominatedNode") is reserved for this pod in memory.
	// In this case, we need to keep reserving the NominatedNode when updating the pod pointer.
	nodeName := ""
	// We won't fall into below `if` block if the Update event represents:
	// (1) NominatedNode info is added
	// (2) NominatedNode info is updated
	// (3) NominatedNode info is removed
	if NominatedNodeName(oldPod) == "" && NominatedNodeName(newPodInfo.Pod) == "" {
		if nnn, ok := npm.nominatedPodToNode[oldPod.UID]; ok {
			// This is the only case we should continue reserving the NominatedNode
			nodeName = nnn
		}
	}
	// We update irrespective of the nominatedNodeName changed or not, to ensure
	// that pod pointer is updated.
	npm.delete(oldPod)
	npm.add(newPodInfo, nodeName)
}

// NewPodNominator creates a nominatedPodMap as a backing of framework.PodNominator.
func NewPodNominator() framework.PodNominator {
	return &nominatedPodMap{
		nominatedPods:      make(map[string][]*framework.PodInfo),
		nominatedPodToNode: make(map[types.UID]string),
	}
}

// NominatedNodeName returns nominated node name of a Pod.
func NominatedNodeName(pod *corev1.Pod) string {
	return pod.Status.NominatedNodeName
}

// DeleteNominatedPodIfExists deletes <pod> from nominatedPods.
func (npm *nominatedPodMap) DeleteNominatedPodIfExists(pod *corev1.Pod) {
	npm.Lock()
	npm.delete(pod)
	npm.Unlock()
}

// AddNominatedPod adds a pod to the nominated pods of the given node.
// This is called during the preemption process after a node is nominated to run
// the pod. We update the structure before sending a request to update the pod
// object to avoid races with the following scheduling cycles.
func (npm *nominatedPodMap) AddNominatedPod(logger klog.Logger, pi *framework.PodInfo, nominatingInfo *framework.NominatingInfo) {
	npm.Lock()
	npm.add(pi, nominatingInfo.NominatedNodeName)
	npm.Unlock()
}

// NominatedPodsForNode returns pods that are nominated to run on the given node,
// but they are waiting for other pods to be removed from the node.
func (npm *nominatedPodMap) NominatedPodsForNode(nodeName string) []*framework.PodInfo {
	npm.RLock()
	defer npm.RUnlock()
	// TODO: we may need to return a copy of []*Pods to avoid modification
	// on the caller side.
	return npm.nominatedPods[nodeName]
}

func TestPlugin_Recover(t *testing.T) {
	suit := newPluginTestSuit(t, nil)
	nodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node1",
			},
			Status: corev1.NodeStatus{
				Allocatable: createResourceList(100, 1000),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node2",
			},
			Status: corev1.NodeStatus{
				Allocatable: createResourceList(100, 1000),
			},
		},
	}
	for _, node := range nodes {
		suit.Handle.ClientSet().CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
	}
	time.Sleep(100 * time.Millisecond)
	suit.AddQuota("test-parent", extension.RootQuotaName, 100, 1000, 0, 0, 0, 0, true, "")
	suit.AddQuota("test1", "test-parent", 100, 1000, 0, 0, 0, 0, false, "")
	time.Sleep(100 * time.Millisecond)
	pods := []*corev1.Pod{
		defaultCreatePodWithQuotaName("1", "test1", 10, 10, 10),
		defaultCreatePodWithQuotaName("2", "test1", 10, 10, 10),
		defaultCreatePodWithQuotaName("3", "test1", 10, 10, 10),
		defaultCreatePodWithQuotaName("4", "test1", 10, 10, 10),
	}
	for _, pod := range pods {
		suit.Handle.ClientSet().CoreV1().Pods("").Create(context.TODO(), pod, metav1.CreateOptions{})
	}
	time.Sleep(100 * time.Millisecond)
	p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
	assert.Nil(t, err)
	pl := p.(*Plugin)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, pl.groupQuotaManager.GetQuotaInfoByName("test1").GetRequest(), createResourceList(40, 40))
	assert.Equal(t, pl.groupQuotaManager.GetQuotaInfoByName("test1").GetUsed(), createResourceList(40, 40))
	assert.True(t, quotav1.IsZero(pl.groupQuotaManager.GetQuotaInfoByName(extension.DefaultQuotaName).GetRequest()))
	assert.Equal(t, len(pl.groupQuotaManager.GetAllQuotaNames()), 5)
}

func TestPlugin_migrateDefaultQuotaGroupsPod(t *testing.T) {
	suit := newPluginTestSuit(t, nil)
	p, err := suit.proxyNew(suit.elasticQuotaArgs, suit.Handle)
	assert.Nil(t, err)
	plugin := p.(*Plugin)
	gqm := plugin.groupQuotaManager
	plugin.addQuota("test2", extension.RootQuotaName, 96, 160, 100, 160, 96, 160, true, "", "")
	pods := []*corev1.Pod{
		defaultCreatePodWithQuotaName("1", "test1", 10, 10, 10),
		defaultCreatePodWithQuotaName("2", "test1", 10, 10, 10),
		defaultCreatePodWithQuotaName("3", "test1", 10, 10, 10),
		defaultCreatePodWithQuotaName("4", "test1", 10, 10, 10),
	}
	for _, pod := range pods {
		plugin.OnPodAdd(pod)
	}
	assert.Equal(t, gqm.GetQuotaInfoByName(extension.DefaultQuotaName).GetRequest(), createResourceList(40, 40))
	assert.Equal(t, 4, len(gqm.GetQuotaInfoByName(extension.DefaultQuotaName).PodCache))
	plugin.addQuota("test1", extension.RootQuotaName, 96, 160, 100, 160, 96, 160, true, "", "")
	time.Sleep(100 * time.Millisecond)
	go plugin.Start()
	for i := 0; i < 10; i++ {
		if len(gqm.GetQuotaInfoByName(extension.DefaultQuotaName).GetPodCache()) != 0 || len(gqm.GetQuotaInfoByName("test1").GetPodCache()) != 4 {
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}
	assert.Equal(t, 0, len(gqm.GetQuotaInfoByName(extension.DefaultQuotaName).PodCache))
	assert.Equal(t, 4, len(gqm.GetQuotaInfoByName("test1").PodCache))
}

func defaultCreatePodWithQuotaNameAndVersion(name, quotaName, version string, priority int32, cpu, mem int64) *corev1.Pod {
	pod := defaultCreatePod(name, priority, cpu, mem)
	pod.Labels[extension.LabelQuotaName] = quotaName
	pod.ResourceVersion = version
	pod.UID = types.UID(name)
	return pod
}

func defaultCreatePodWithQuotaName(name, quotaName string, priority int32, cpu, mem int64) *corev1.Pod {
	pod := defaultCreatePod(name, priority, cpu, mem)
	pod.Labels[extension.LabelQuotaName] = quotaName
	pod.UID = types.UID(name)
	pod.Spec.NodeName = "test"
	return pod
}

func defaultCreatePodWithQuotaAndNonPreemptible(name, quotaName string, priority int32, cpu, mem int64, nonPreempt bool) *corev1.Pod {
	pod := defaultCreatePod(name, priority, cpu, mem)
	pod.Labels[extension.LabelQuotaName] = quotaName
	if nonPreempt {
		pod.Labels[extension.LabelPreemptible] = "false"
	}
	pod.UID = types.UID(name)
	return pod
}

func defaultCreatePod(name string, priority int32, cpu, mem int64) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: make(map[string]string),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Requests: createResourceList(cpu, mem),
					},
				},
			},
			Priority: pointer.Int32(priority),
		},
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Spec.NodeName = "test-node"
	return pod
}

func TestPostFilterState(t *testing.T) {
	t.Run("test", func(t *testing.T) {
		testCycleState := framework.NewCycleState()
		p := &Plugin{
			pluginArgs: &config.ElasticQuotaArgs{
				EnableRuntimeQuota: false,
			},
		}

		p.skipPostFilterState(testCycleState)
		got, err := getPostFilterState(testCycleState)
		assert.NoError(t, err)
		assert.NotNil(t, got)
		cycleStateCopy := testCycleState.Clone()
		got1, err := getPostFilterState(cycleStateCopy)
		assert.NoError(t, err)
		assert.Equal(t, got, got1)

		testCycleState = framework.NewCycleState()
		p.snapshotPostFilterState(&core.QuotaInfo{}, testCycleState)
		got, err = getPostFilterState(testCycleState)
		assert.NoError(t, err)
		assert.NotNil(t, got)
		cycleStateCopy = testCycleState.Clone()
		got1, err = getPostFilterState(cycleStateCopy)
		assert.NoError(t, err)
		assert.Equal(t, got, got1)
	})
}
