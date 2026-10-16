//
// Copyright (c) 2019-2026 Red Hat, Inc.
// This program and the accompanying materials are made
// available under the terms of the Eclipse Public License 2.0
// which is available at https://www.eclipse.org/legal/epl-2.0/
//
// SPDX-License-Identifier: EPL-2.0
//
// Contributors:
//   Red Hat, Inc. - initial API and implementation
//

package pinnedimageset

import (
	"context"
	"fmt"
	"testing"

	chev2 "github.com/eclipse-che/che-operator/api/v2"
	"github.com/eclipse-che/che-operator/pkg/common/chetypes"
	"github.com/eclipse-che/che-operator/pkg/common/constants"
	"github.com/eclipse-che/che-operator/pkg/common/infrastructure"
	k8s_client "github.com/eclipse-che/che-operator/pkg/common/k8s-client"
	"github.com/eclipse-che/che-operator/pkg/common/test"
	imagepuller "github.com/eclipse-che/che-operator/pkg/deploy/image-puller"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	fakeDiscovery "k8s.io/client-go/discovery/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	digest1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digest2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	image1 = "quay.io/che/image-1@" + digest1
	image2 = "quay.io/che/image-2@" + digest2
)

// newMachineConfigPool mirrors how a real pool selects a set: `mcp/worker.spec.machineConfigSelector`
// on OpenShift 4.22 is exactly `matchLabels: {machineconfiguration.openshift.io/role: worker}`.
func newMachineConfigPool(role string) *mcfgv1.MachineConfigPool {
	return &mcfgv1.MachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: role},
		Spec: mcfgv1.MachineConfigPoolSpec{
			MachineConfigSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{constants.MachineConfigPoolRoleLabelKey: role},
			},
		},
	}
}

// fakeExternalImagesProvider returns a canned list instead of calling out to the dashboard.
type fakeExternalImagesProvider struct {
	images []string
	err    error

	// calls counts fetches, so that a test can assert the dashboard was not called at all.
	// Not reachable any other way: a provider that is never asked and one whose answer is
	// discarded produce the same objects.
	calls int
}

func (p *fakeExternalImagesProvider) Get(ctx *chetypes.DeployContext) ([]string, error) {
	p.calls++

	return p.images, p.err
}

func TestSyncPinnedImageSet(t *testing.T) {
	type testCase struct {
		name                 string
		enableImagePuller    bool
		externalImages       []string
		expectedPinnedImages []mcfgv1.PinnedImageRef
	}

	testCases := []testCase{
		{
			name:              "Create a PinnedImageSet from digest pinned images",
			enableImagePuller: true,
			externalImages:    []string{image1, image2},
			expectedPinnedImages: []mcfgv1.PinnedImageRef{
				{Name: mcfgv1.ImageDigestFormat(image1)},
				{Name: mcfgv1.ImageDigestFormat(image2)},
			},
		},
		{
			name:              "Skip images not referenced by digest",
			enableImagePuller: true,
			externalImages:    []string{image1, "quay.io/che/image-2:latest", "quay.io/che/image-3"},
			expectedPinnedImages: []mcfgv1.PinnedImageRef{
				{Name: mcfgv1.ImageDigestFormat(image1)},
			},
		},
		{
			name:              "Do not create a PinnedImageSet if no image is referenced by digest",
			enableImagePuller: true,
			externalImages:    []string{"quay.io/che/image-1:latest"},
		},
		{
			name:              "Do not create a PinnedImageSet if image puller is disabled",
			enableImagePuller: false,
			externalImages:    []string{image1},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := newTestCtx(testCase.enableImagePuller)

			p := &PinnedImageSetReconciler{
				externalImages: &fakeExternalImagesProvider{images: testCase.externalImages},
			}

			test.EnsureReconcile(t, ctx, p.Reconcile)

			actual := &mcfgv1.PinnedImageSet{}
			exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
				context.TODO(),
				types.NamespacedName{Name: "eclipse-che-eclipse-che"},
				actual,
			)
			assert.NoError(t, err)

			if len(testCase.expectedPinnedImages) == 0 {
				assert.False(t, exists)
				return
			}

			assert.True(t, exists)
			assert.Equal(t, testCase.expectedPinnedImages, actual.Spec.PinnedImages)
			assert.Equal(t, constants.PinnedImageSetComponentName, actual.Labels[constants.KubernetesComponentLabelKey])
			assert.Equal(t, constants.MachineConfigPoolRole, actual.Labels[constants.MachineConfigPoolRoleLabelKey])
			assert.Contains(t, ctx.CheCluster.Finalizers, finalizerName)
		})
	}
}

func TestDeletePinnedImageSetWhenImagePullerIsDisabled(t *testing.T) {
	ctx := newTestCtx(false)
	ctx.CheCluster.Finalizers = []string{finalizerName}

	err := ctx.ClusterAPI.Client.Create(
		context.TODO(),
		newPinnedImageSet(ctx, []string{image1}),
	)
	assert.NoError(t, err)

	p := &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: []string{image1}},
	}

	test.EnsureReconcile(t, ctx, p.Reconcile)

	exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
		context.TODO(),
		types.NamespacedName{Name: "eclipse-che-eclipse-che"},
		&mcfgv1.PinnedImageSet{},
	)
	assert.NoError(t, err)
	assert.False(t, exists)
	assert.NotContains(t, ctx.CheCluster.Finalizers, finalizerName)
}

func TestFinalize(t *testing.T) {
	ctx := newTestCtx(true)

	p := &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: []string{image1}},
	}

	test.EnsureReconcile(t, ctx, p.Reconcile)
	assert.Contains(t, ctx.CheCluster.Finalizers, finalizerName)

	assert.True(t, p.Finalize(ctx))

	exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
		context.TODO(),
		types.NamespacedName{Name: "eclipse-che-eclipse-che"},
		&mcfgv1.PinnedImageSet{},
	)
	assert.NoError(t, err)
	assert.False(t, exists)
	assert.NotContains(t, ctx.CheCluster.Finalizers, finalizerName)
}

// TestOverTheCapIsTruncatedRatherThanRejected covers a list longer than the CRD's `maxItems`.
// Submitting it would have the API server reject the whole object, so nothing at all would be
// pinned. Pinning as many as fit is strictly better, and the overflow is not lost either: it
// falls to the community backend, which has no such limit.
func TestOverTheCapIsTruncatedRatherThanRejected(t *testing.T) {
	images := make([]string, 0, imagepuller.MaxPinnedImages+1)
	for i := 0; i <= imagepuller.MaxPinnedImages; i++ {
		images = append(images, fmt.Sprintf("quay.io/che/image-%d@%s", i, digest1))
	}

	ctx := newTestCtx(true)

	p := &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: images},
	}

	test.EnsureReconcile(t, ctx, p.Reconcile)

	actual := &mcfgv1.PinnedImageSet{}
	exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
		context.TODO(),
		types.NamespacedName{Name: "eclipse-che-eclipse-che"},
		actual,
	)
	assert.NoError(t, err)
	assert.True(t, exists)
	assert.Len(t, actual.Spec.PinnedImages, imagepuller.MaxPinnedImages)

	// The first ones that fit are kept, so the truncation point is predictable.
	assert.Equal(t, mcfgv1.ImageDigestFormat(images[0]), actual.Spec.PinnedImages[0].Name)
	assert.Equal(
		t,
		mcfgv1.ImageDigestFormat(images[imagepuller.MaxPinnedImages-1]),
		actual.Spec.PinnedImages[imagepuller.MaxPinnedImages-1].Name,
	)
}

// TestReconcileOnClusterWithoutPinnedImageSetAPI covers the OpenShift versions that do not serve
// the API at all. Reading or deleting the object there fails with a no-matches error rather than
// a not-found one, which used to stall the reconcile chain and leave the CheCluster undeletable.
func TestReconcileOnClusterWithoutPinnedImageSetAPI(t *testing.T) {
	for _, enableImagePuller := range []bool{true, false} {
		ctx := newTestCtxWithoutPinnedImageSetAPI(enableImagePuller)
		ctx.CheCluster.Finalizers = []string{finalizerName}

		p := &PinnedImageSetReconciler{
			externalImages: &fakeExternalImagesProvider{images: []string{image1}},
		}

		_, done, err := p.Reconcile(ctx)
		assert.NoError(t, err)
		assert.True(t, done)

		assert.True(t, p.Finalize(ctx))
		assert.NotContains(t, ctx.CheCluster.Finalizers, finalizerName)
	}
}

// TestUninstallIsSkippedWhenAPIIsAbsent asserts the guard is the discovery check and not the
// client, by pointing the reconciler at a client that cannot serialise a PinnedImageSet at all.
func TestUninstallIsSkippedWhenAPIIsAbsent(t *testing.T) {
	ctx := newTestCtxWithoutPinnedImageSetAPI(false)

	// Proves the client would indeed fail, and that the failure is not a not-found error.
	err := ctx.ClusterAPI.NonCachingClient.Get(
		context.TODO(),
		types.NamespacedName{Name: "eclipse-che-eclipse-che"},
		&mcfgv1.PinnedImageSet{},
	)
	assert.Error(t, err)
	assert.False(t, k8serrors.IsNotFound(err))

	p := &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: []string{image1}},
	}

	done, err := p.uninstallPinnedImageSet(ctx)
	assert.NoError(t, err)
	assert.True(t, done)
}

// TestTheSetTargetsTheWorkerPool covers the pool the set is aimed at. A set no pool selects is
// the dangerous case: the object is admitted, no pool ever consumes it, and no MachineConfigNode
// reports on it, so nothing on the cluster reveals that no node is pre-pulling. That is why the
// operator creates nothing at all rather than leaving a healthy looking object behind.
func TestTheSetTargetsTheWorkerPool(t *testing.T) {
	t.Run("Label the set for the worker pool", func(t *testing.T) {
		ctx := newTestCtxWithMachineConfigPools(newMachineConfigPool("worker"))

		p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1}}}
		test.EnsureReconcile(t, ctx, p.Reconcile)

		actual := &mcfgv1.PinnedImageSet{}
		exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
			context.TODO(), types.NamespacedName{Name: "eclipse-che-eclipse-che"}, actual)
		assert.NoError(t, err)
		assert.True(t, exists)
		assert.Equal(t, constants.MachineConfigPoolRole, actual.Labels[constants.MachineConfigPoolRoleLabelKey])
	})

	// A cluster with no worker pool is single node OpenShift, where every node is in the master
	// pool. Pinning is simply unavailable there and the community backend is the way to pre-pull,
	// which is covered by TestSingleNodeFallsBackToTheCommunityBackend. What matters here is only
	// that nothing is left behind.
	t.Run("Create nothing where no pool selects the worker role", func(t *testing.T) {
		ctx := newTestCtxWithMachineConfigPools(newMachineConfigPool("master"))

		p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1}}}
		test.EnsureReconcile(t, ctx, p.Reconcile)

		exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
			context.TODO(), types.NamespacedName{Name: "eclipse-che-eclipse-che"}, &mcfgv1.PinnedImageSet{})
		assert.NoError(t, err)
		assert.False(t, exists)
		assert.NotContains(t, ctx.CheCluster.Finalizers, finalizerName)
	})

	// The guard inside syncPinnedImageSet is reached only when the pool disappears between the
	// capability probe and the sync, so it is driven directly. It is the last thing standing
	// between a deleted pool and a set that no pool consumes, which nothing on the cluster
	// reports: the object is admitted, never applied, and absent from every MachineConfigNode.
	t.Run("Create nothing when the pool disappears mid reconcile", func(t *testing.T) {
		ctx := newTestCtxWithMachineConfigPools(newMachineConfigPool("master"))

		p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1}}}
		done, err := p.syncPinnedImageSet([]string{image1}, ctx)
		assert.NoError(t, err)
		assert.True(t, done)

		exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
			context.TODO(), types.NamespacedName{Name: "eclipse-che-eclipse-che"}, &mcfgv1.PinnedImageSet{})
		assert.NoError(t, err)
		assert.False(t, exists)
		assert.Equal(t, "misconfigured: worker", p.lastStatus[reportingKey(ctx)])
		assert.NotContains(t, ctx.CheCluster.Finalizers, finalizerName)
	})
}

// TestMachineConfigPoolWithMatchExpressions guards against matching the pool by name or by a
// plain label comparison. A custom pool commonly selects several roles at once, and the Machine
// Config Operator evaluates the pool's own selector against the labels the set carries.
func TestMachineConfigPoolWithMatchExpressions(t *testing.T) {
	machineConfigPool := &mcfgv1.MachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: "workspaces"},
		Spec: mcfgv1.MachineConfigPoolSpec{
			MachineConfigSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      constants.MachineConfigPoolRoleLabelKey,
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"worker", "workspaces"},
					},
				},
			},
		},
	}

	ctx := newTestCtxWithMachineConfigPools(machineConfigPool.DeepCopy())
	name, err := infrastructure.FindMachineConfigPool(ctx.ClusterAPI.NonCachingClient, constants.MachineConfigPoolRole)
	assert.NoError(t, err)
	assert.Equal(t, "workspaces", name)

	// A pool that selects neither the worker role nor anything else we carry must not match,
	// so that a cluster without a worker pool is reported rather than silently mistargeted.
	machineConfigPool.Spec.MachineConfigSelector.MatchExpressions[0].Values = []string{"infra"}

	ctx = newTestCtxWithMachineConfigPools(machineConfigPool.DeepCopy())
	name, err = infrastructure.FindMachineConfigPool(ctx.ClusterAPI.NonCachingClient, constants.MachineConfigPoolRole)
	assert.NoError(t, err)
	assert.Empty(t, name)
}

// TestPinningIsOptIn pins the decision that a capable cluster is not a consenting one. A
// PinnedImageSet is cluster-scoped and all-or-nothing, so an installation that never asked for
// pinning must behave exactly as it did before the backend existed, however capable the cluster.
func TestPinningIsOptIn(t *testing.T) {
	type testCase struct {
		name                 string
		enablePinnedImageSet bool
		expectPinnedImageSet bool
	}

	testCases := []testCase{
		{name: "Unset creates no set on a capable cluster", enablePinnedImageSet: false, expectPinnedImageSet: false},
		{name: "Opted in creates the set", enablePinnedImageSet: true, expectPinnedImageSet: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := newTestCtx(true)
			ctx.CheCluster.Spec.Components.ImagePuller.EnablePinnedImageSet = testCase.enablePinnedImageSet

			p := &PinnedImageSetReconciler{
				externalImages: &fakeExternalImagesProvider{images: []string{image1}},
			}

			test.EnsureReconcile(t, ctx, p.Reconcile)

			exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
				context.TODO(),
				types.NamespacedName{Name: "eclipse-che-eclipse-che"},
				&mcfgv1.PinnedImageSet{},
			)
			assert.NoError(t, err)
			assert.Equal(t, testCase.expectPinnedImageSet, exists)
		})
	}
}

// TestUnsatisfiablePinnedImageSetBackendIsReported keeps the fall back to the community backend
// explainable. Requesting the native backend where it cannot work is a misconfiguration, and
// without the message it is indistinguishable from never having asked for it.
func TestUnsatisfiablePinnedImageSetBackendIsReported(t *testing.T) {
	ctx := newTestCtxWithoutPinnedImageSetAPI(true)

	p := &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: []string{image1}},
	}

	test.EnsureReconcile(t, ctx, p.Reconcile)
	assert.Equal(t, "unsatisfiable", p.lastStatus[reportingKey(ctx)])

	// Asking for a backend that does work must not report anything.
	ctx = newTestCtx(true)

	p = &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: []string{image1}},
	}

	test.EnsureReconcile(t, ctx, p.Reconcile)
	assert.NotEqual(t, "unsatisfiable", p.lastStatus[reportingKey(ctx)])
}

// newTestCtx builds a context on a cluster that serves the PinnedImageSet API
// and runs a managed control plane.
func newTestCtx(enableImagePuller bool) *chetypes.DeployContext {
	cheCluster := &chev2.CheCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eclipse-che",
			Namespace: "eclipse-che",
		},
		Spec: chev2.CheClusterSpec{
			Components: chev2.CheClusterComponents{
				ImagePuller: chev2.ImagePuller{
					Enable: enableImagePuller,
					// Opted in, because pinning is off unless asked for and almost every
					// test in this package is about what happens once it is asked for.
					// The tests that care about the opt in itself set it themselves.
					EnablePinnedImageSet: true,
				},
			},
		},
	}

	machineConfigDaemon := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-config-daemon",
			Namespace: "openshift-machine-config-operator",
		},
	}

	ctx := test.NewCtxBuilder().
		WithCheCluster(cheCluster).
		WithObjects(machineConfigDaemon, newMachineConfigPool(constants.MachineConfigPoolRole)).
		Build()

	discoveryClient := ctx.ClusterAPI.DiscoveryClient.(*fakeDiscovery.FakeDiscovery)
	discoveryClient.Resources = append(
		discoveryClient.Resources,
		&metav1.APIResourceList{
			GroupVersion: mcfgv1.GroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "pinnedimagesets"},
			},
		},
	)

	return ctx
}

// newTestCtxWithMachineConfigPools builds a context whose only MachineConfigPools are the given
// ones, so that a test can describe the pool layout it needs, including none at all.
func newTestCtxWithMachineConfigPools(machineConfigPools ...client.Object) *chetypes.DeployContext {
	ctx := newTestCtx(true)

	defaultPool := &mcfgv1.MachineConfigPool{}
	err := ctx.ClusterAPI.NonCachingClient.Get(
		context.TODO(),
		types.NamespacedName{Name: constants.MachineConfigPoolRole},
		defaultPool,
	)
	utilruntime.Must(err)
	utilruntime.Must(ctx.ClusterAPI.NonCachingClient.Delete(context.TODO(), defaultPool))

	for _, machineConfigPool := range machineConfigPools {
		utilruntime.Must(ctx.ClusterAPI.NonCachingClient.Create(context.TODO(), machineConfigPool))
	}

	return ctx
}

// newTestCtxWithoutPinnedImageSetAPI builds a context for an OpenShift cluster that does not serve
// the PinnedImageSet API. Neither discovery nor the client scheme knows the type, so the client
// reproduces the no-matches failure the API server returns when the CRD is absent.
func newTestCtxWithoutPinnedImageSetAPI(enableImagePuller bool) *chetypes.DeployContext {
	ctx := newTestCtx(enableImagePuller)

	discoveryClient := ctx.ClusterAPI.DiscoveryClient.(*fakeDiscovery.FakeDiscovery)
	discoveryClient.Resources = []*metav1.APIResourceList{}

	scheme := runtime.NewScheme()
	utilruntime.Must(chev2.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ctx.CheCluster).Build()

	ctx.ClusterAPI.Scheme = scheme
	ctx.ClusterAPI.Client = cli
	ctx.ClusterAPI.NonCachingClient = cli
	ctx.ClusterAPI.ClientWrapper = k8s_client.NewK8sClient(cli, scheme)
	ctx.ClusterAPI.NonCachingClientWrapper = k8s_client.NewK8sClient(cli, scheme)

	return ctx
}

// TestAnEditedRoleLabelIsRestored keeps the role label inside what the operator reconciles.
// The label is not decoration: the Machine Config Operator matches it against a pool's
// machineConfigSelector, so an edited label silently retargets the pre-pull at another pool, or
// at none. Comparing only spec.pinnedImages would let that stand, since the image list is
// unchanged.
func TestAnEditedRoleLabelIsRestored(t *testing.T) {
	ctx := newTestCtxWithMachineConfigPools(newMachineConfigPool("worker"), newMachineConfigPool("infra"))

	p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1}}}
	test.EnsureReconcile(t, ctx, p.Reconcile)

	edited := &mcfgv1.PinnedImageSet{}
	_, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
		context.TODO(), types.NamespacedName{Name: "eclipse-che-eclipse-che"}, edited)
	assert.NoError(t, err)

	edited.Labels[constants.MachineConfigPoolRoleLabelKey] = "infra"
	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Update(context.TODO(), edited))

	test.EnsureReconcile(t, ctx, p.Reconcile)

	actual := &mcfgv1.PinnedImageSet{}
	_, err = ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
		context.TODO(), types.NamespacedName{Name: "eclipse-che-eclipse-che"}, actual)
	assert.NoError(t, err)
	assert.Equal(t, constants.MachineConfigPoolRole, actual.Labels[constants.MachineConfigPoolRoleLabelKey])
}
