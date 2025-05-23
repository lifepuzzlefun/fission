/*
Copyright 2020 The Fission Authors.

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

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	apiv1 "k8s.io/api/core/v1"
	k8sErrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	k8sInformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	k8sCache "k8s.io/client-go/tools/cache"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/executor/executortype"
	"github.com/fission/fission/pkg/executor/fscache"
	"github.com/fission/fission/pkg/executor/metrics"
	"github.com/fission/fission/pkg/executor/reaper"
	executorUtils "github.com/fission/fission/pkg/executor/util"
	hpautils "github.com/fission/fission/pkg/executor/util/hpa"
	"github.com/fission/fission/pkg/generated/clientset/versioned"
	genInformer "github.com/fission/fission/pkg/generated/informers/externalversions"
	"github.com/fission/fission/pkg/throttler"
	"github.com/fission/fission/pkg/utils"
	"github.com/fission/fission/pkg/utils/manager"
	"github.com/fission/fission/pkg/utils/maps"
	otelUtils "github.com/fission/fission/pkg/utils/otel"
)

var (
	_ executortype.ExecutorType = &Container{}
)

type (
	// Container represents an executor type
	Container struct {
		logger *zap.Logger

		kubernetesClient kubernetes.Interface
		fissionClient    versioned.Interface
		instanceID       string
		nsResolver       *utils.NamespaceResolver
		// fetcherConfig    *fetcherConfig.Config

		runtimeImagePullPolicy apiv1.PullPolicy
		useIstio               bool

		fsCache *fscache.FunctionServiceCache // cache funcSvc's by function, address and pod name

		throttler *throttler.Throttler

		defaultIdlePodReapTime time.Duration

		deplLister map[string]appslisters.DeploymentLister
		svcLister  map[string]corelisters.ServiceLister

		deplListerSynced map[string]k8sCache.InformerSynced
		svcListerSynced  map[string]k8sCache.InformerSynced

		hpaops                     *hpautils.HpaOperations
		objectReaperIntervalSecond time.Duration

		enableOwnerReferences bool
	}
)

// MakeContainer initializes and returns an instance of CaaF
func MakeContainer(
	ctx context.Context,
	logger *zap.Logger,
	fissionClient versioned.Interface,
	kubernetesClient kubernetes.Interface,
	instanceID string,
	finformerFactory map[string]genInformer.SharedInformerFactory,
	cnmInformerFactory map[string]k8sInformers.SharedInformerFactory,
) (executortype.ExecutorType, error) {
	enableIstio := false
	if len(os.Getenv("ENABLE_ISTIO")) > 0 {
		istio, err := strconv.ParseBool(os.Getenv("ENABLE_ISTIO"))
		if err != nil {
			logger.Error("failed to parse 'ENABLE_ISTIO', set to false", zap.Error(err))
		}
		enableIstio = istio
	}

	caaf := &Container{
		logger: logger.Named("CaaF"),

		fissionClient:    fissionClient,
		kubernetesClient: kubernetesClient,
		instanceID:       instanceID,
		nsResolver:       utils.DefaultNSResolver(),

		fsCache:   fscache.MakeFunctionServiceCache(logger),
		throttler: throttler.MakeThrottler(1 * time.Minute),

		runtimeImagePullPolicy: utils.GetImagePullPolicy(os.Getenv("RUNTIME_IMAGE_PULL_POLICY")),
		useIstio:               enableIstio,
		// Time is set slightly higher than NewDeploy as cold starts are longer for CaaF
		defaultIdlePodReapTime:     1 * time.Minute,
		objectReaperIntervalSecond: time.Duration(executorUtils.GetObjectReaperInterval(logger, fv1.ExecutorTypeContainer, 5)) * time.Second,
		hpaops:                     hpautils.NewHpaOperations(logger, kubernetesClient, instanceID),
		deplLister:                 make(map[string]appslisters.DeploymentLister),
		deplListerSynced:           make(map[string]k8sCache.InformerSynced),
		svcLister:                  make(map[string]corelisters.ServiceLister),
		svcListerSynced:            make(map[string]k8sCache.InformerSynced),

		enableOwnerReferences: utils.IsOwnerReferencesEnabled(),
	}

	for ns, informerFactory := range cnmInformerFactory {
		caaf.deplLister[ns] = informerFactory.Apps().V1().Deployments().Lister()
		caaf.deplListerSynced[ns] = informerFactory.Apps().V1().Deployments().Informer().HasSynced
		caaf.svcLister[ns] = informerFactory.Core().V1().Services().Lister()
		caaf.svcListerSynced[ns] = informerFactory.Core().V1().Services().Informer().HasSynced
	}
	for _, factory := range finformerFactory {
		_, err := factory.Core().V1().Functions().Informer().AddEventHandler(caaf.FuncInformerHandler(ctx))
		if err != nil {
			return nil, fmt.Errorf("failed to add event handler for function informer: %w", err)
		}
	}
	return caaf, nil
}

// Run start the function along with an object reaper.
func (caaf *Container) Run(ctx context.Context, mgr manager.Interface) {
	waitSynced := make([]k8sCache.InformerSynced, 0)
	for _, deplListerSynced := range caaf.deplListerSynced {
		waitSynced = append(waitSynced, deplListerSynced)
	}
	for _, svcListerSynced := range caaf.svcListerSynced {
		waitSynced = append(waitSynced, svcListerSynced)
	}

	if ok := k8sCache.WaitForCacheSync(ctx.Done(), waitSynced...); !ok {
		caaf.logger.Fatal("failed to wait for caches to sync")
	}
	mgr.Add(ctx, func(ctx context.Context) {
		caaf.idleObjectReaper(ctx)
	})
}

// GetTypeName returns the executor type name.
func (caaf *Container) GetTypeName(ctx context.Context) fv1.ExecutorType {
	return fv1.ExecutorTypeContainer
}

// GetTotalAvailable has not been implemented for CaaF.
func (caaf *Container) GetTotalAvailable(fn *fv1.Function) int {
	// Not Implemented for CaaF.
	return 0
}

// UnTapService has not been implemented for CaaF.
func (caaf *Container) UnTapService(ctx context.Context, fnMeta *metav1.ObjectMeta, svcHost string) {
	// Not Implemented for CaaF.
}

// MarkSpecializationFailure has not been implemented for CaaF.
func (caaf *Container) MarkSpecializationFailure(ctx context.Context, fnMeta *metav1.ObjectMeta) {
	// Not Implemented for CaaF.
}

// GetFuncSvc returns a function service; error otherwise.
func (caaf *Container) GetFuncSvc(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	return caaf.createFunction(ctx, fn)
}

// GetFuncSvcFromCache returns a function service from cache; error otherwise.
func (caaf *Container) GetFuncSvcFromCache(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	otelUtils.SpanTrackEvent(ctx, "GetFuncSvcFromCache", otelUtils.GetAttributesForFunction(fn)...)
	return caaf.fsCache.GetByFunctionUID(fn.UID)
}

// DeleteFuncSvcFromCache deletes a function service from cache.
func (caaf *Container) DeleteFuncSvcFromCache(ctx context.Context, fsvc *fscache.FuncSvc) {
	caaf.fsCache.DeleteEntry(fsvc)
}

// TapService makes a TouchByAddress request to the cache.
func (caaf *Container) TapService(ctx context.Context, svcHost string) error {
	err := caaf.fsCache.TouchByAddress(svcHost)
	if err != nil {
		return err
	}
	return nil
}

// IsValid does a get on the service address to ensure it's a valid service, then
// scale deployment to 1 replica if there are no available replicas for function.
// Return true if no error occurs, return false otherwise.
func (caaf *Container) IsValid(ctx context.Context, fsvc *fscache.FuncSvc) bool {
	logger := otelUtils.LoggerWithTraceID(ctx, caaf.logger)
	otelUtils.SpanTrackEvent(ctx, "IsValid", fscache.GetAttributesForFuncSvc(fsvc)...)
	if len(strings.Split(fsvc.Address, ".")) == 0 {
		logger.Error("address not found in function service")
		return false
	}
	if len(fsvc.KubernetesObjects) == 0 {
		logger.Error("no kubernetes object related to function", zap.String("function", fsvc.Function.Name))
		return false
	}
	for _, obj := range fsvc.KubernetesObjects {
		if strings.ToLower(obj.Kind) == "service" {
			_, err := caaf.svcLister[obj.Namespace].Services(obj.Namespace).Get(obj.Name)
			if err != nil {
				if !k8sErrs.IsNotFound(err) {
					logger.Error("error validating function service", zap.String("function", fsvc.Function.Name), zap.Error(err))
				}
				return false
			}
		} else if strings.ToLower(obj.Kind) == "deployment" {
			currentDeploy, err := caaf.deplLister[obj.Namespace].Deployments(obj.Namespace).Get(obj.Name)
			if err != nil {
				if !k8sErrs.IsNotFound(err) {
					logger.Error("error validating function deployment", zap.String("function", fsvc.Function.Name), zap.Error(err))
				}
				return false
			}
			if currentDeploy.Status.AvailableReplicas < 1 {
				return false
			}
		}
	}
	return true
}

// RefreshFuncPods deletes pods related to the function so that new pods are replenished
func (caaf *Container) RefreshFuncPods(ctx context.Context, logger *zap.Logger, f fv1.Function) error {

	funcLabels := caaf.getDeployLabels(f.ObjectMeta)

	nsResolver := utils.DefaultNSResolver()
	dep, err := caaf.kubernetesClient.AppsV1().Deployments(nsResolver.GetFunctionNS(f.ObjectMeta.Namespace)).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(funcLabels).AsSelector().String(),
	})
	if err != nil {
		return err
	}

	// Ideally there should be only one deployment but for now we rely on label/selector to ensure that condition
	for _, deployment := range dep.Items {
		rvCount, err := referencedResourcesRVSum(ctx, caaf.kubernetesClient, deployment.Namespace, f.Spec.Secrets, f.Spec.ConfigMaps)
		if err != nil {
			return err
		}

		patch := fmt.Sprintf(`{"spec" : {"template": {"spec":{"containers":[{"name": "%s", "env":[{"name": "%s", "value": "%d"}]}]}}}}`,
			f.ObjectMeta.Name, fv1.ResourceVersionCount, rvCount)

		_, err = caaf.kubernetesClient.AppsV1().Deployments(deployment.ObjectMeta.Namespace).Patch(ctx, deployment.ObjectMeta.Name,
			k8sTypes.StrategicMergePatchType,
			[]byte(patch), metav1.PatchOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

// AdoptExistingResources attempts to adopt resources for functions in all namespaces.
func (caaf *Container) AdoptExistingResources(ctx context.Context) {
	wg := &sync.WaitGroup{}

	for _, namepsace := range utils.DefaultNSResolver().FissionResourceNS {
		fnList, err := caaf.fissionClient.CoreV1().Functions(namepsace).List(ctx, metav1.ListOptions{})
		if err != nil {
			caaf.logger.Error("error getting function list", zap.Error(err))
			return
		}

		for i := range fnList.Items {
			fn := &fnList.Items[i]
			if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeContainer {
				wg.Add(1)
				go func() {
					defer wg.Done()

					_, err = caaf.fnCreate(ctx, fn)
					if err != nil {
						caaf.logger.Warn("failed to adopt resources for function", zap.Error(err))
						return
					}
					caaf.logger.Info("adopt resources for function", zap.String("function", fn.ObjectMeta.Name))
				}()
			}
		}
	}

	wg.Wait()
}

// CleanupOldExecutorObjects cleans orphaned resources.
func (caaf *Container) CleanupOldExecutorObjects(ctx context.Context) {
	caaf.logger.Info("CaaF starts to clean orphaned resources", zap.String("instanceID", caaf.instanceID))

	var errs error
	listOpts := metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{fv1.EXECUTOR_TYPE: string(fv1.ExecutorTypeContainer)}).AsSelector().String(),
	}

	err := reaper.CleanupHpa(ctx, caaf.logger, caaf.kubernetesClient, caaf.instanceID, listOpts)
	if err != nil {
		errs = errors.Join(errs, err)
	}

	err = reaper.CleanupDeployments(ctx, caaf.logger, caaf.kubernetesClient, caaf.instanceID, listOpts)
	if err != nil {
		errs = errors.Join(errs, err)
	}

	err = reaper.CleanupServices(ctx, caaf.logger, caaf.kubernetesClient, caaf.instanceID, listOpts)
	if err != nil {
		errs = errors.Join(errs, err)
	}

	if errs != nil {
		// TODO retry reaper; logged and ignored for now
		caaf.logger.Error("Failed to cleanup old executor objects", zap.Error(errs))
	}
}

func (caaf *Container) createFunction(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeContainer {
		return nil, nil
	}

	fsvcObj, err := caaf.throttler.RunOnce(string(fn.ObjectMeta.UID), func(ableToCreate bool) (interface{}, error) {
		if ableToCreate {
			return caaf.fnCreate(ctx, fn)
		}
		return caaf.fsCache.GetByFunctionUID(fn.ObjectMeta.UID)
	})
	if err != nil {
		e := "error creating k8s resources for function"
		caaf.logger.Error(e,
			zap.Error(err),
			zap.String("function_name", fn.ObjectMeta.Name),
			zap.String("function_namespace", fn.ObjectMeta.Namespace))
		return nil, fmt.Errorf("error creating k8s resources for function %s/%s: %w", fn.ObjectMeta.Namespace, fn.ObjectMeta.Name, err)
	}

	fsvc, ok := fsvcObj.(*fscache.FuncSvc)
	if !ok {
		caaf.logger.Panic("receive unknown object while creating function - expected pointer of function service object")
	}

	return fsvc, err
}

func (caaf *Container) deleteFunction(ctx context.Context, fn *fv1.Function) error {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeContainer {
		return nil
	}
	err := caaf.fnDelete(ctx, fn)
	if err != nil {
		return fmt.Errorf("error deleting kubernetes objects of function %s: %w", k8sCache.MetaObjectToName(fn), err)
	}
	return err
}

func (caaf *Container) fnCreate(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	cleanupFunc := func(ns string, name string) {
		err := caaf.cleanupContainer(ctx, ns, name)
		if err != nil {
			caaf.logger.Error("received error while cleaning function resources",
				zap.String("namespace", ns), zap.String("name", name))
		}
	}
	objName := caaf.getObjName(fn)
	deployLabels := caaf.getDeployLabels(fn.ObjectMeta)
	deployAnnotations := caaf.getDeployAnnotations(fn.ObjectMeta)

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns
	ns := caaf.nsResolver.GetFunctionNS(fn.ObjectMeta.Namespace)

	// Envoy(istio-proxy) returns 404 directly before istio pilot
	// propagates latest Envoy-specific configuration.
	// Since Container waits for pods of deployment to be ready,
	// change the order of kubeObject creation (create service first,
	// then deployment) to take advantage of waiting time.
	svc, err := caaf.createOrGetSvc(ctx, fn, deployLabels, deployAnnotations, objName, ns)
	if err != nil {
		caaf.logger.Error("error creating service", zap.Error(err), zap.String("service", objName))
		go cleanupFunc(ns, objName)
		return nil, fmt.Errorf("error creating service %s: %w", objName, err)
	}
	svcAddress := fmt.Sprintf("%s.%s", svc.Name, svc.Namespace)

	depl, err := caaf.createOrGetDeployment(ctx, fn, objName, deployLabels, deployAnnotations, ns)
	if err != nil {
		caaf.logger.Error("error creating deployment", zap.Error(err), zap.String("deployment", objName))
		go cleanupFunc(ns, objName)
		return nil, fmt.Errorf("error creating deployment %s: %w", objName, err)
	}

	hpa, err := caaf.hpaops.CreateOrGetHpa(ctx, fn, objName, &fn.Spec.InvokeStrategy.ExecutionStrategy, depl, deployLabels, deployAnnotations)
	if err != nil {
		caaf.logger.Error("error creating HPA", zap.Error(err), zap.String("hpa", objName))
		go cleanupFunc(ns, objName)
		return nil, fmt.Errorf("error creating HPA %s: %w", objName, err)
	}

	kubeObjRefs := []apiv1.ObjectReference{
		{
			// obj.TypeMeta.Kind does not work hence this, needs investigation and a fix
			Kind:            "deployment",
			Name:            depl.Name,
			APIVersion:      depl.APIVersion,
			Namespace:       depl.Namespace,
			ResourceVersion: depl.ResourceVersion,
			UID:             depl.UID,
		},
		{
			Kind:            "service",
			Name:            svc.Name,
			APIVersion:      svc.APIVersion,
			Namespace:       svc.Namespace,
			ResourceVersion: svc.ResourceVersion,
			UID:             svc.UID,
		},
		{
			Kind:            "horizontalpodautoscaler",
			Name:            hpa.Name,
			APIVersion:      hpa.APIVersion,
			Namespace:       hpa.Namespace,
			ResourceVersion: hpa.ResourceVersion,
			UID:             hpa.UID,
		},
	}

	fsvc := &fscache.FuncSvc{
		Name:              objName,
		Function:          &fn.ObjectMeta,
		Address:           svcAddress,
		KubernetesObjects: kubeObjRefs,
		Executor:          fv1.ExecutorTypeContainer,
	}

	_, err = caaf.fsCache.Add(*fsvc)
	if err != nil {
		caaf.logger.Error("error adding function to cache", zap.Error(err), zap.Any("function", fsvc.Function))
		metrics.ColdStartsError.WithLabelValues(fn.Name, fn.Namespace).Inc()
		return fsvc, err
	}

	metrics.ColdStarts.WithLabelValues(fn.Name, fn.Namespace).Inc()

	return fsvc, nil
}

func (caaf *Container) updateFunction(ctx context.Context, oldFn *fv1.Function, newFn *fv1.Function) error {

	if oldFn.ResourceVersion == newFn.ResourceVersion {
		return nil
	}

	// Ignoring updates to functions which are not of Container type
	if newFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeContainer &&
		oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeContainer {
		return nil
	}

	// Executor type is no longer Container
	if newFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeContainer &&
		oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeContainer {
		caaf.logger.Info("function does not use new deployment executor anymore, deleting resources",
			zap.Any("function", newFn))
		// IMP - pass the oldFn, as the new/modified function is not in cache
		return caaf.deleteFunction(ctx, oldFn)
	}

	// Executor type changed to Container from something else
	if oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeContainer &&
		newFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeContainer {
		caaf.logger.Info("function type changed to Container, creating resources",
			zap.Any("old_function", oldFn.ObjectMeta),
			zap.Any("new_function", newFn.ObjectMeta))
		_, err := caaf.createFunction(ctx, newFn)
		if err != nil {
			caaf.updateStatus(oldFn, err, "error changing the function's type to Container")
		}
		return err
	}

	if !reflect.DeepEqual(oldFn.Spec.InvokeStrategy, newFn.Spec.InvokeStrategy) {
		// to support backward compatibility, if the function was created in default ns, we fall back to creating the
		// deployment of the function in fission-function ns, so cleaning up resources there
		ns := caaf.nsResolver.GetFunctionNS(newFn.ObjectMeta.Namespace)

		fsvc, err := caaf.fsCache.GetByFunctionUID(newFn.ObjectMeta.UID)
		if err != nil {
			return fmt.Errorf("error updating function due to unable to find function service cache %s: %w", k8sCache.MetaObjectToName(oldFn), err)
		}

		hpa, err := caaf.hpaops.GetHpa(ctx, ns, fsvc.Name)
		if err != nil {
			caaf.updateStatus(oldFn, err, "error getting HPA while updating function")
			return err
		}

		hpaChanged := false

		if newFn.Spec.InvokeStrategy.ExecutionStrategy.MinScale != oldFn.Spec.InvokeStrategy.ExecutionStrategy.MinScale {
			replicas := int32(newFn.Spec.InvokeStrategy.ExecutionStrategy.MinScale)
			hpa.Spec.MinReplicas = &replicas
			hpaChanged = true
		}

		if newFn.Spec.InvokeStrategy.ExecutionStrategy.MaxScale != oldFn.Spec.InvokeStrategy.ExecutionStrategy.MaxScale {
			hpa.Spec.MaxReplicas = int32(newFn.Spec.InvokeStrategy.ExecutionStrategy.MaxScale)
			hpaChanged = true
		}

		if !reflect.DeepEqual(newFn.Spec.InvokeStrategy.ExecutionStrategy.Metrics, oldFn.Spec.InvokeStrategy.ExecutionStrategy.Metrics) {
			hpa.Spec.Metrics = newFn.Spec.InvokeStrategy.ExecutionStrategy.Metrics
			hpaChanged = true
		}

		if !reflect.DeepEqual(newFn.Spec.InvokeStrategy.ExecutionStrategy.Behavior, oldFn.Spec.InvokeStrategy.ExecutionStrategy.Behavior) {
			hpa.Spec.Behavior = newFn.Spec.InvokeStrategy.ExecutionStrategy.Behavior
			hpaChanged = true
		}

		if hpaChanged {
			err := caaf.hpaops.UpdateHpa(ctx, hpa)
			if err != nil {
				caaf.updateStatus(oldFn, err, "error updating HPA while updating function")
				return err
			}
		}
	}

	deployChanged := false

	// If length of slice has changed then no need to check individual elements
	if len(oldFn.Spec.Secrets) != len(newFn.Spec.Secrets) {
		deployChanged = true
	} else {
		for i, newSecret := range newFn.Spec.Secrets {
			if newSecret != oldFn.Spec.Secrets[i] {
				deployChanged = true
				break
			}
		}
	}
	if len(oldFn.Spec.ConfigMaps) != len(newFn.Spec.ConfigMaps) {
		deployChanged = true
	} else {
		for i, newConfig := range newFn.Spec.ConfigMaps {
			if newConfig != oldFn.Spec.ConfigMaps[i] {
				deployChanged = true
				break
			}
		}
	}

	if !reflect.DeepEqual(oldFn.Spec.PodSpec, newFn.Spec.PodSpec) {
		deployChanged = true
	}

	if deployChanged {
		return caaf.updateFuncDeployment(ctx, newFn)
	}

	return nil
}

func (caaf *Container) updateFuncDeployment(ctx context.Context, fn *fv1.Function) error {

	fsvc, err := caaf.fsCache.GetByFunctionUID(fn.ObjectMeta.UID)
	if err != nil {
		return fmt.Errorf("error updating function due to unable to find function service cache %s: %w", k8sCache.MetaObjectToName(fn), err)
	}
	fnObjName := fsvc.Name

	deployLabels := caaf.getDeployLabels(fn.ObjectMeta)
	caaf.logger.Info("updating deployment due to function update",
		zap.String("deployment", fnObjName), zap.Any("function", fn.ObjectMeta.Name))

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns
	ns := caaf.nsResolver.GetFunctionNS(fn.ObjectMeta.Namespace)

	existingDepl, err := caaf.kubernetesClient.AppsV1().Deployments(ns).Get(ctx, fnObjName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// the resource version inside function packageRef is changed,
	// so the content of fetchRequest in deployment cmd is different.
	// Therefore, the deployment update will trigger a rolling update.
	newDeployment, err := caaf.getDeploymentSpec(ctx, fn, existingDepl.Spec.Replicas, // use current replicas instead of minscale in the ExecutionStrategy.
		fnObjName, ns, deployLabels, caaf.getDeployAnnotations(fn.ObjectMeta))
	if err != nil {
		caaf.updateStatus(fn, err, "failed to get new deployment spec while updating function")
		return err
	}

	err = caaf.updateDeployment(ctx, newDeployment, ns)
	if err != nil {
		caaf.updateStatus(fn, err, "failed to update deployment while updating function")
		return err
	}

	return nil
}

func (caaf *Container) fnDelete(ctx context.Context, fn *fv1.Function) error {
	var multierr error

	// GetByFunction uses resource version as part of cache key, however,
	// the resource version in function metadata will be changed when a function
	// is deleted and cause Container backend fails to delete the entry.
	// Use GetByFunctionUID instead of GetByFunction here to find correct
	// fsvc entry.
	fsvc, err := caaf.fsCache.GetByFunctionUID(fn.ObjectMeta.UID)
	if err != nil {
		return fmt.Errorf("fsvc not found in cache %s: %w", k8sCache.MetaObjectToName(fn), err)
	}

	objName := fsvc.Name

	_, err = caaf.fsCache.DeleteOld(fsvc, time.Second*0)
	if err != nil {
		multierr = errors.Join(multierr, fmt.Errorf("error deleting function from cache: %w", err))
	}

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns, so cleaning up resources there
	ns := caaf.nsResolver.GetFunctionNS(fn.ObjectMeta.Namespace)

	err = caaf.cleanupContainer(ctx, ns, objName)
	multierr = errors.Join(multierr, err)
	return multierr
}

// getObjName returns a unique name for kubernetes objects of function
func (caaf *Container) getObjName(fn *fv1.Function) string {
	// use meta uuid of function, this ensure we always get the same name for the same function.
	uid := fn.ObjectMeta.UID[len(fn.ObjectMeta.UID)-17:]
	var functionMetadata string
	if len(fn.ObjectMeta.Name)+len(fn.ObjectMeta.Namespace) < 35 {
		functionMetadata = fn.ObjectMeta.Name + "-" + fn.ObjectMeta.Namespace
	} else {
		if len(fn.ObjectMeta.Name) > 17 {
			functionMetadata = fn.ObjectMeta.Name[:17]
		} else {
			functionMetadata = fn.ObjectMeta.Name
		}
		if len(fn.ObjectMeta.Namespace) > 17 {
			functionMetadata = functionMetadata + "-" + fn.ObjectMeta.Namespace[:17]
		} else {
			functionMetadata = functionMetadata + "-" + fn.ObjectMeta.Namespace
		}
	}
	// constructed name should be 63 characters long, as it is a valid k8s name
	// functionMetadata should be 35 characters long, as we take 17 characters from functionUid
	// with newdeploy 10 character prefix
	return strings.ToLower(fmt.Sprintf("container-%s-%s", functionMetadata, uid))
}

func (caaf *Container) getDeployLabels(fnMeta metav1.ObjectMeta) map[string]string {
	deployLabels := maps.CopyStringMap(fnMeta.Labels)
	deployLabels[fv1.EXECUTOR_TYPE] = string(fv1.ExecutorTypeContainer)
	deployLabels[fv1.FUNCTION_NAME] = fnMeta.Name
	deployLabels[fv1.FUNCTION_NAMESPACE] = fnMeta.Namespace
	deployLabels[fv1.FUNCTION_UID] = string(fnMeta.UID)
	return deployLabels
}

func (caaf *Container) getDeployAnnotations(fnMeta metav1.ObjectMeta) map[string]string {
	deployAnnotations := maps.CopyStringMap(fnMeta.Annotations)
	deployAnnotations[fv1.EXECUTOR_INSTANCEID_LABEL] = caaf.instanceID
	deployAnnotations[fv1.FUNCTION_RESOURCE_VERSION] = fnMeta.ResourceVersion
	return deployAnnotations
}

// updateStatus is a function which updates status of update.
// Current implementation only logs messages, in future it will update function status
func (caaf *Container) updateStatus(fn *fv1.Function, err error, message string) {
	caaf.logger.Error("function status update", zap.Error(err), zap.Any("function", fn), zap.String("message", message))
}

// idleObjectReaper reaps objects after certain idle time
func (caaf *Container) idleObjectReaper(ctx context.Context) {
	// calling function doIdleObjectReaper() repeatedly at given interval of time
	wait.UntilWithContext(ctx, caaf.doIdleObjectReaper, caaf.objectReaperIntervalSecond)
}

func (caaf *Container) doIdleObjectReaper(ctx context.Context) {
	funcSvcs, err := caaf.fsCache.ListOld(time.Second * 5)
	if err != nil {
		caaf.logger.Error("error reaping idle pods", zap.Error(err))
		return
	}

	for i := range funcSvcs {
		fsvc := funcSvcs[i]

		if fsvc.Executor != fv1.ExecutorTypeContainer {
			continue
		}

		fn, err := caaf.fissionClient.CoreV1().Functions(fsvc.Function.Namespace).Get(ctx, fsvc.Function.Name, metav1.GetOptions{})
		if err != nil {
			// CaaF manager handles the function delete event and clean cache/kubeobjs itself,
			// so we ignore the not found error for functions with CaaF executor type here.
			if k8sErrs.IsNotFound(err) && fsvc.Executor == fv1.ExecutorTypeContainer {
				continue
			}
			caaf.logger.Error("error getting function", zap.Error(err), zap.String("function", fsvc.Function.Name))
			continue
		}

		idlePodReapTime := caaf.defaultIdlePodReapTime
		if fn.Spec.IdleTimeout != nil {
			idlePodReapTime = time.Duration(*fn.Spec.IdleTimeout) * time.Second
		}

		if time.Since(fsvc.Atime) < idlePodReapTime {
			continue
		}

		go func() {
			deployObj := getDeploymentObj(fsvc.KubernetesObjects)
			if deployObj == nil {
				caaf.logger.Error("error finding function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
				return
			}

			currentDeploy, err := caaf.kubernetesClient.AppsV1().
				Deployments(deployObj.Namespace).Get(ctx, deployObj.Name, metav1.GetOptions{})
			if err != nil {
				caaf.logger.Error("error getting function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
				return
			}

			minScale := int32(fn.Spec.InvokeStrategy.ExecutionStrategy.MinScale)

			// do nothing if the current replicas is already lower than minScale
			if *currentDeploy.Spec.Replicas <= minScale {
				return
			}

			err = caaf.scaleDeployment(ctx, deployObj.Namespace, deployObj.Name, minScale)
			if err != nil {
				caaf.logger.Error("error scaling down function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
			}
		}()
	}
}

func getDeploymentObj(kubeobjs []apiv1.ObjectReference) *apiv1.ObjectReference {
	for _, kubeobj := range kubeobjs {
		switch strings.ToLower(kubeobj.Kind) {
		case "deployment":
			return &kubeobj
		}
	}
	return nil
}

func (caaf *Container) DumpDebugInfo(ctx context.Context) error {
	return nil
}
