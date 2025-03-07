// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sobj "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8srtschema "k8s.io/apimachinery/pkg/runtime/schema"
	ctrlrtzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	ackcompare "github.com/aws-controllers-k8s/runtime/pkg/compare"
	"github.com/aws-controllers-k8s/runtime/pkg/condition"
	ackcfg "github.com/aws-controllers-k8s/runtime/pkg/config"
	ackerr "github.com/aws-controllers-k8s/runtime/pkg/errors"
	ackmetrics "github.com/aws-controllers-k8s/runtime/pkg/metrics"
	"github.com/aws-controllers-k8s/runtime/pkg/requeue"
	ackrt "github.com/aws-controllers-k8s/runtime/pkg/runtime"
	ackrtcache "github.com/aws-controllers-k8s/runtime/pkg/runtime/cache"
	acktypes "github.com/aws-controllers-k8s/runtime/pkg/types"

	k8srtschemamocks "github.com/aws-controllers-k8s/runtime/mocks/apimachinery/pkg/runtime/schema"
	ctrlrtclientmock "github.com/aws-controllers-k8s/runtime/mocks/controller-runtime/pkg/client"
	ackmocks "github.com/aws-controllers-k8s/runtime/mocks/pkg/types"
)

func resourceMocks() (
	*ackmocks.AWSResource, // mocked resource
	*ctrlrtclientmock.Object, // mocked k8s controller-runtime RuntimeObject
	*k8sobj.Unstructured, // NON-mocked k8s apimachinery meta object
) {
	objKind := &k8srtschemamocks.ObjectKind{}
	objKind.On("GroupVersionKind").Return(
		k8srtschema.GroupVersionKind{
			Group:   "bookstore.services.k8s.aws",
			Kind:    "Book",
			Version: "v1alpha1",
		},
	)

	rtObj := &ctrlrtclientmock.Object{}
	rtObj.On("GetObjectKind").Return(objKind)
	rtObj.On("DeepCopyObject").Return(rtObj)

	metaObj := &k8sobj.Unstructured{}
	metaObj.SetAnnotations(map[string]string{})
	metaObj.SetNamespace("default")
	metaObj.SetName("mybook")
	metaObj.SetGeneration(int64(1))

	res := &ackmocks.AWSResource{}
	res.On("MetaObject").Return(metaObj)
	res.On("RuntimeObject").Return(rtObj)
	res.On("DeepCopy").Return(res)
	// DoNothing on SetStatus call.
	res.On("SetStatus", res).Return(func(res ackmocks.AWSResource) {})

	return res, rtObj, metaObj
}

func reconcilerMocks(
	rmf acktypes.AWSResourceManagerFactory,
) (
	acktypes.AWSResourceReconciler,
	*ctrlrtclientmock.Client,
) {
	zapOptions := ctrlrtzap.Options{
		Development: true,
		Level:       zapcore.InfoLevel,
	}
	fakeLogger := ctrlrtzap.New(ctrlrtzap.UseFlagOptions(&zapOptions))
	cfg := ackcfg.Config{}
	metrics := ackmetrics.NewMetrics("bookstore")

	sc := &ackmocks.ServiceController{}
	kc := &ctrlrtclientmock.Client{}

	return ackrt.NewReconcilerWithClient(
		sc, kc, rmf, fakeLogger, cfg, metrics, ackrtcache.Caches{},
	), kc
}

func managedResourceManagerFactoryMocks(
	desired acktypes.AWSResource,
	latest acktypes.AWSResource,
) (
	*ackmocks.AWSResourceManagerFactory,
	*ackmocks.AWSResourceDescriptor,
) {
	return managerFactoryMocks(desired, latest, true)
}

func managerFactoryMocks(
	desired acktypes.AWSResource,
	latest acktypes.AWSResource,
	isManaged bool,
) (
	*ackmocks.AWSResourceManagerFactory,
	*ackmocks.AWSResourceDescriptor,
) {
	rd := &ackmocks.AWSResourceDescriptor{}
	rd.On("GroupKind").Return(
		&metav1.GroupKind{
			Group: "bookstore.services.k8s.aws",
			Kind:  "fakeBook",
		},
	)
	rd.On("EmptyRuntimeObject").Return(
		&fakeBook{},
	)
	rd.On("IsManaged", latest).Return(isManaged)

	rmf := &ackmocks.AWSResourceManagerFactory{}
	rmf.On("ResourceDescriptor").Return(rd)

	reg := ackrt.NewRegistry()
	reg.RegisterResourceManagerFactory(rmf)
	return rmf, rd
}

func TestReconcilerUpdate(t *testing.T) {
	require := require.New(t)

	ctx := context.TODO()
	arn := ackv1alpha1.AWSResourceName("mybook-arn")

	delta := ackcompare.NewDelta()
	delta.Add("Spec.A", "val1", "val2")

	desired, _, _ := resourceMocks()
	desired.On("ReplaceConditions", []*ackv1alpha1.Condition{}).Return()

	ids := &ackmocks.AWSResourceIdentifiers{}
	ids.On("ARN").Return(&arn)

	latest, latestRTObj, _ := resourceMocks()
	latest.On("Identifiers").Return(ids)

	// resourceReconciler.ensureConditions will ensure that if the resource
	// manager has not set any Conditions on the resource, that at least an
	// ACK.ResourceSynced condition with status Unknown will be set on the
	// resource.
	latest.On("Conditions").Return([]*ackv1alpha1.Condition{})
	latest.On(
		"ReplaceConditions",
		mock.AnythingOfType("[]*v1alpha1.Condition"),
	).Return().Run(func(args mock.Arguments) {
		conditions := args.Get(0).([]*ackv1alpha1.Condition)
		assert.Equal(t, 1, len(conditions))
		cond := conditions[0]
		assert.Equal(t, cond.Type, ackv1alpha1.ConditionTypeResourceSynced)
		assert.Equal(t, cond.Status, corev1.ConditionUnknown)
	})

	rm := &ackmocks.AWSResourceManager{}
	rm.On("ResolveReferences", ctx, nil, desired).Return(
		desired, nil,
	)
	rm.On("ReadOne", ctx, desired).Return(
		latest, nil,
	)
	rm.On("Update", ctx, desired, latest, delta).Return(
		latest, nil,
	)

	rmf, rd := managedResourceManagerFactoryMocks(desired, latest)
	rd.On("Delta", desired, latest).Return(
		delta,
	).Once()
	rd.On("Delta", desired, latest).Return(ackcompare.NewDelta())

	rm.On("LateInitialize", ctx, latest).Return(latest, nil)
	rd.On("Delta", latest, latest).Return(ackcompare.NewDelta())

	r, kc := reconcilerMocks(rmf)

	// pointers returned from "client.MergeFrom" fails the equality check during
	// assertion even when parameters inside two objects are same.
	// hence we use mock.AnythingOfType parameter to assert patch call
	kc.On("Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch")).Return(nil)

	// With the above mocks and below assertions, we check that if we got a
	// non-error return from `AWSResourceManager.ReadOne()` and the
	// `AWSResourceDescriptor.Delta()` returned a non-empty Delta, that we end
	// up calling the AWSResourceManager.Update() call in the Reconciler.Sync()
	// method,
	_, err := r.Sync(ctx, rm, desired)
	require.Nil(err)
	rm.AssertCalled(t, "ResolveReferences", ctx, nil, desired)
	rm.AssertCalled(t, "ReadOne", ctx, desired)
	rd.AssertCalled(t, "Delta", desired, latest)
	rm.AssertCalled(t, "Update", ctx, desired, latest, delta)
	// No changes to metadata or spec so Patch on the object shouldn't be done
	kc.AssertNotCalled(t, "Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch"))
	// Only the HandleReconcilerError wrapper function ever calls patchResourceStatus
	kc.AssertNotCalled(t, "Status")
	rm.AssertCalled(t, "LateInitialize", ctx, latest)
}

func TestReconcilerUpdate_PatchMetadataAndSpec_DiffInMetadata(t *testing.T) {
	require := require.New(t)

	ctx := context.TODO()
	arn := ackv1alpha1.AWSResourceName("mybook-arn")

	delta := ackcompare.NewDelta()
	delta.Add("Spec.A", "val1", "val2")

	desired, _, _ := resourceMocks()
	desired.On("ReplaceConditions", []*ackv1alpha1.Condition{}).Return()

	ids := &ackmocks.AWSResourceIdentifiers{}
	ids.On("ARN").Return(&arn)

	latest, latestRTObj, latestMetaObj := resourceMocks()
	latest.On("Identifiers").Return(ids)
	latest.On("Conditions").Return([]*ackv1alpha1.Condition{})
	latest.On(
		"ReplaceConditions",
		mock.AnythingOfType("[]*v1alpha1.Condition"),
	).Return()

	// Note the change in annotations
	latestMetaObj.SetAnnotations(map[string]string{"a": "b"})

	rmf, rd := managedResourceManagerFactoryMocks(desired, latest)
	rd.On("Delta", desired, latest).Return(
		delta,
	).Once()
	rd.On("Delta", desired, latest).Return(ackcompare.NewDelta())

	rm := &ackmocks.AWSResourceManager{}
	rm.On("ResolveReferences", ctx, nil, desired).Return(
		desired, nil,
	)
	rm.On("ReadOne", ctx, desired).Return(
		latest, nil,
	)
	rm.On("Update", ctx, desired, latest, delta).Return(
		latest, nil,
	)
	rm.On("LateInitialize", ctx, latest).Return(latest, nil)
	rd.On("Delta", latest, latest).Return(ackcompare.NewDelta())

	r, kc := reconcilerMocks(rmf)

	kc.On("Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch")).Return(nil)

	_, err := r.Sync(ctx, rm, desired)
	require.Nil(err)
	rm.AssertCalled(t, "ResolveReferences", ctx, nil, desired)
	rm.AssertCalled(t, "ReadOne", ctx, desired)
	rd.AssertCalled(t, "Delta", desired, latest)
	rm.AssertCalled(t, "Update", ctx, desired, latest, delta)
	kc.AssertCalled(t, "Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch"))
	// Only the HandleReconcilerError wrapper function ever calls patchResourceStatus
	kc.AssertNotCalled(t, "Status")
	rm.AssertCalled(t, "LateInitialize", ctx, latest)
	latest.AssertCalled(t, "DeepCopy")
	latest.AssertCalled(t, "SetStatus", latest)
}

func TestReconcilerUpdate_PatchMetadataAndSpec_DiffInSpec(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := context.TODO()
	arn := ackv1alpha1.AWSResourceName("mybook-arn")

	delta := ackcompare.NewDelta()
	delta.Add("Spec.A", "val1", "val2")

	desired, _, _ := resourceMocks()
	desired.On("ReplaceConditions", []*ackv1alpha1.Condition{}).Return()

	ids := &ackmocks.AWSResourceIdentifiers{}
	ids.On("ARN").Return(&arn)

	latest, latestRTObj, _ := resourceMocks()
	latest.On("Identifiers").Return(ids)
	latest.On("Conditions").Return([]*ackv1alpha1.Condition{})
	// ensureConditions method will add ACK.ResourceSynced condition as Unknown
	latest.On(
		"ReplaceConditions",
		mock.AnythingOfType("[]*v1alpha1.Condition"),
	).Return().Run(func(args mock.Arguments) {
		conditions := args.Get(0).([]*ackv1alpha1.Condition)
		hasSynced := false
		for _, condition := range conditions {
			if condition.Type != ackv1alpha1.ConditionTypeResourceSynced {
				continue
			}

			hasSynced = true
			assert.Equal(condition.Status, corev1.ConditionUnknown)
		}

		assert.True(hasSynced)
	})
	// Note no change to metadata...

	rmf, rd := managedResourceManagerFactoryMocks(desired, latest)
	rd.On("Delta", desired, latest).Return(
		delta,
	)

	rm := &ackmocks.AWSResourceManager{}
	rm.On("ResolveReferences", ctx, nil, desired).Return(
		desired, nil,
	)
	rm.On("ReadOne", ctx, desired).Return(
		latest, nil,
	)
	rm.On("Update", ctx, desired, latest, delta).Return(
		latest, nil,
	)
	rm.On("LateInitialize", ctx, latest).Return(latest, nil)
	rd.On("Delta", latest, latest).Return(ackcompare.NewDelta())

	r, kc := reconcilerMocks(rmf)

	kc.On("Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch")).Return(nil)

	_, err := r.Sync(ctx, rm, desired)
	require.Nil(err)
	rm.AssertCalled(t, "ResolveReferences", ctx, nil, desired)
	rm.AssertCalled(t, "ReadOne", ctx, desired)
	rd.AssertCalled(t, "Delta", desired, latest)
	rm.AssertCalled(t, "Update", ctx, desired, latest, delta)
	kc.AssertCalled(t, "Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch"))
	// Only the HandleReconcilerError wrapper function ever calls patchResourceStatus
	kc.AssertNotCalled(t, "Status")
	rm.AssertCalled(t, "LateInitialize", ctx, latest)
}

func TestReconcilerHandleReconcilerError_PatchStatus_Latest(t *testing.T) {
	require := require.New(t)

	ctx := context.TODO()
	arn := ackv1alpha1.AWSResourceName("mybook-arn")

	delta := ackcompare.NewDelta()
	delta.Add("Spec.A", "val1", "val2")

	desired, _, _ := resourceMocks()
	desired.On("ReplaceConditions", []*ackv1alpha1.Condition{}).Return()

	ids := &ackmocks.AWSResourceIdentifiers{}
	ids.On("ARN").Return(&arn)

	latest, latestRTObj, latestMetaObj := resourceMocks()
	latest.On("Identifiers").Return(ids)
	latest.On("Conditions").Return([]*ackv1alpha1.Condition{})
	latest.On(
		"ReplaceConditions",
		mock.AnythingOfType("[]*v1alpha1.Condition"),
	).Return()

	latestMetaObj.SetAnnotations(map[string]string{"a": "b"})

	rmf, _ := managedResourceManagerFactoryMocks(desired, latest)
	r, kc := reconcilerMocks(rmf)

	statusWriter := &ctrlrtclientmock.StatusWriter{}
	kc.On("Status").Return(statusWriter)
	statusWriter.On("Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch")).Return(nil)
	kc.On("Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch")).Return(nil)

	_, err := r.HandleReconcileError(ctx, desired, latest, nil)
	require.Nil(err)
	statusWriter.AssertCalled(t, "Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch"))
	// The HandleReconcilerError function never updates spec or metadata, so
	// even though there is a change to the annotations we expect no call to
	// patch the spec/metadata...
	kc.AssertNotCalled(t, "Patch")
}

func TestReconcilerHandleReconcilerError_NoPatchStatus_NoLatest(t *testing.T) {
	require := require.New(t)

	ctx := context.TODO()

	desired, _, _ := resourceMocks()
	desired.On("ReplaceConditions", []*ackv1alpha1.Condition{}).Return()

	rmf, _ := managedResourceManagerFactoryMocks(desired, nil)
	r, kc := reconcilerMocks(rmf)

	statusWriter := &ctrlrtclientmock.StatusWriter{}
	kc.On("Status").Return(statusWriter)

	_, err := r.HandleReconcileError(ctx, desired, nil, nil)
	require.Nil(err)
	// If latest is nil, we should not call patch status...
	statusWriter.AssertNotCalled(t, "Patch")
	// The HandleReconcilerError function never updates spec or metadata, so
	// even though there is a change to the annotations we expect no call to
	// patch the spec/metadata...
	kc.AssertNotCalled(t, "Patch")
}

func TestReconcilerUpdate_ErrorInLateInitialization(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := context.TODO()
	arn := ackv1alpha1.AWSResourceName("mybook-arn")

	delta := ackcompare.NewDelta()
	delta.Add("Spec.A", "val1", "val2")

	desired, _, _ := resourceMocks()
	desired.On("ReplaceConditions", []*ackv1alpha1.Condition{}).Return()

	ids := &ackmocks.AWSResourceIdentifiers{}
	ids.On("ARN").Return(&arn)

	latest, latestRTObj, _ := resourceMocks()
	latest.On("Identifiers").Return(ids)
	syncCondition := ackv1alpha1.Condition{
		Type:   ackv1alpha1.ConditionTypeResourceSynced,
		Status: corev1.ConditionFalse,
	}
	latest.On("Conditions").Return([]*ackv1alpha1.Condition{}).Times(2)
	latest.On("Conditions").Return([]*ackv1alpha1.Condition{&syncCondition})
	latest.On(
		"ReplaceConditions",
		mock.AnythingOfType("[]*v1alpha1.Condition"),
	).Return().Run(func(args mock.Arguments) {
		conditions := args.Get(0).([]*ackv1alpha1.Condition)
		hasSynced := false
		for _, condition := range conditions {
			if condition.Type != ackv1alpha1.ConditionTypeResourceSynced {
				continue
			}

			hasSynced = true
			assert.Equal(condition.Status, corev1.ConditionFalse)
		}

		assert.True(hasSynced)
	})

	rm := &ackmocks.AWSResourceManager{}
	rm.On("ResolveReferences", ctx, nil, desired).Return(
		desired, nil,
	)
	rm.On("ReadOne", ctx, desired).Return(
		latest, nil,
	)
	rm.On("Update", ctx, desired, latest, delta).Return(
		latest, nil,
	)

	rmf, rd := managedResourceManagerFactoryMocks(desired, latest)
	rd.On("Delta", desired, latest).Return(
		delta,
	).Once()
	rd.On("Delta", desired, latest).Return(ackcompare.NewDelta())

	requeueError := requeue.NeededAfter(errors.New("error from late initialization"), time.Duration(0)*time.Second)
	rm.On("LateInitialize", ctx, latest).Return(latest, requeueError)
	rd.On("Delta", latest, latest).Return(ackcompare.NewDelta())

	r, kc := reconcilerMocks(rmf)

	kc.On("Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch")).Return(nil)

	_, err := r.Sync(ctx, rm, desired)
	// Assert the error from late initialization
	require.NotNil(err)
	assert.Equal(requeueError, err)
	rm.AssertCalled(t, "ResolveReferences", ctx, nil, desired)
	rm.AssertCalled(t, "ReadOne", ctx, desired)
	rd.AssertCalled(t, "Delta", desired, latest)
	rm.AssertCalled(t, "Update", ctx, desired, latest, delta)
	// No difference in desired, latest metadata and spec
	kc.AssertNotCalled(t, "Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch"))
	rm.AssertCalled(t, "LateInitialize", ctx, latest)
}

func TestReconcilerUpdate_ResourceNotManaged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx := context.TODO()
	arn := ackv1alpha1.AWSResourceName("mybook-arn")

	delta := ackcompare.NewDelta()

	desired, _, _ := resourceMocks()
	desired.On("ReplaceConditions", []*ackv1alpha1.Condition{}).Return()

	ids := &ackmocks.AWSResourceIdentifiers{}
	ids.On("ARN").Return(&arn)

	latest, _, _ := resourceMocks()
	latest.On("Identifiers").Return(ids)

	terminalCondition := ackv1alpha1.Condition{
		Type:    ackv1alpha1.ConditionTypeTerminal,
		Status:  corev1.ConditionTrue,
		Reason:  &condition.NotManagedReason,
		Message: &condition.NotManagedMessage,
	}
	// Return empty conditions for first two times
	latest.On("Conditions").Return([]*ackv1alpha1.Condition{}).Times(2)
	// Once the terminal condition is added, return terminal condition
	// These calls will be made from ensureConditions method, which sets
	// ACK.ResourceSynced condition correctly
	latest.On("Conditions").Return([]*ackv1alpha1.Condition{&terminalCondition})

	latest.On("ReplaceConditions", mock.AnythingOfType("[]*v1alpha1.Condition")).Return([]*ackv1alpha1.Condition{&terminalCondition}).Run(func(args mock.Arguments) {
		conditions := args.Get(0).([]*ackv1alpha1.Condition)
		hasTerminal := false
		for _, condition := range conditions {
			if condition.Type != ackv1alpha1.ConditionTypeTerminal {
				continue
			}

			hasTerminal = true
			assert.Equal(condition.Message, terminalCondition.Message)
			assert.Equal(condition.Reason, terminalCondition.Reason)
		}

		assert.True(hasTerminal)
	})

	rm := &ackmocks.AWSResourceManager{}
	rm.On("ResolveReferences", ctx, nil, desired).Return(
		desired, nil,
	)
	rm.On("ReadOne", ctx, desired).Return(
		latest, nil,
	)

	rmf, rd := managerFactoryMocks(desired, latest, false)

	r, _ := reconcilerMocks(rmf)

	_, err := r.Sync(ctx, rm, desired)
	require.NotNil(err)
	assert.Equal(ackerr.Terminal, err)
	rm.AssertCalled(t, "ResolveReferences", ctx, nil, desired)
	rm.AssertCalled(t, "ReadOne", ctx, desired)
	rd.AssertNotCalled(t, "Delta", desired, latest)
	rm.AssertNotCalled(t, "Update", ctx, desired, latest, delta)
	rm.AssertNotCalled(t, "LateInitialize", ctx, latest)
}

func TestReconcilerUpdate_ResolveReferencesError(t *testing.T) {
	require := require.New(t)

	ctx := context.TODO()
	arn := ackv1alpha1.AWSResourceName("mybook-arn")

	delta := ackcompare.NewDelta()
	delta.Add("Spec.A", "val1", "val2")

	desired, _, _ := resourceMocks()
	desired.On("ReplaceConditions", []*ackv1alpha1.Condition{}).Return()

	ids := &ackmocks.AWSResourceIdentifiers{}
	ids.On("ARN").Return(&arn)

	latest, latestRTObj, _ := resourceMocks()
	latest.On("Identifiers").Return(ids)

	// resourceReconciler.ensureConditions will ensure that if the resource
	// manager has not set any Conditions on the resource, that at least an
	// ACK.ResourceSynced condition with status Unknown will be set on the
	// resource.
	latest.On("Conditions").Return([]*ackv1alpha1.Condition{})
	latest.On(
		"ReplaceConditions",
		mock.AnythingOfType("[]*v1alpha1.Condition"),
	).Return().Run(func(args mock.Arguments) {
		conditions := args.Get(0).([]*ackv1alpha1.Condition)
		assert.Equal(t, 1, len(conditions))
		cond := conditions[0]
		assert.Equal(t, cond.Type, ackv1alpha1.ConditionTypeResourceSynced)
		assert.Equal(t, cond.Status, corev1.ConditionUnknown)
	})

	rm := &ackmocks.AWSResourceManager{}
	resolveReferenceError := errors.New("failed to resolve reference")
	rm.On("ResolveReferences", ctx, nil, desired).Return(
		nil, resolveReferenceError,
	)
	rm.On("ReadOne", ctx, desired).Return(
		latest, nil,
	)
	rm.On("Update", ctx, desired, latest, delta).Return(
		latest, nil,
	)

	rmf, rd := managedResourceManagerFactoryMocks(desired, latest)
	rd.On("Delta", desired, latest).Return(
		delta,
	).Once()
	rd.On("Delta", desired, latest).Return(ackcompare.NewDelta())

	rm.On("LateInitialize", ctx, latest).Return(latest, nil)
	rd.On("Delta", latest, latest).Return(ackcompare.NewDelta())

	r, kc := reconcilerMocks(rmf)

	kc.On("Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch")).Return(nil)

	// With the above mocks and below assertions, we check that if we got a
	// non-error return from `AWSResourceManager.ReadOne()` and the
	// `AWSResourceDescriptor.Delta()` returned a non-empty Delta, that we end
	// up calling the AWSResourceManager.Update() call in the Reconciler.Sync()
	// method,
	_, err := r.Sync(ctx, rm, desired)
	require.NotNil(err)
	rm.AssertCalled(t, "ResolveReferences", ctx, nil, desired)
	rm.AssertNotCalled(t, "ReadOne", ctx, desired)
	rd.AssertNotCalled(t, "Delta", desired, latest)
	rm.AssertNotCalled(t, "Update", ctx, desired, latest, delta)
	// No changes to metadata or spec so Patch on the object shouldn't be done
	kc.AssertNotCalled(t, "Patch", ctx, latestRTObj, mock.AnythingOfType("*client.mergeFromPatch"))
	// Only the HandleReconcilerError wrapper function ever calls patchResourceStatus
	kc.AssertNotCalled(t, "Status")
	rm.AssertNotCalled(t, "LateInitialize", ctx, latest)
}
