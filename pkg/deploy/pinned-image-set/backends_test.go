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
	"errors"
	"testing"

	chev1alpha1 "github.com/che-incubator/kubernetes-image-puller-operator/api/v1alpha1"
	"github.com/eclipse-che/che-operator/pkg/common/chetypes"
	"github.com/eclipse-che/che-operator/pkg/common/constants"
	"github.com/eclipse-che/che-operator/pkg/common/test"
	imagepuller "github.com/eclipse-che/che-operator/pkg/deploy/image-puller"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	fakeDiscovery "k8s.io/client-go/discovery/fake"
)

// The tests below cross the two reconcilers, because the property that matters is not a property
// of either one: each serves its own bucket without consulting the other, so an image lost
// between them, or served twice, is invisible from inside either package. They check that the
// allocation is *realised* — that what the two objects end up carrying is what was allocated —
// which is as far as a fake client can go. Whether the node then pulls the image is a cluster
// question and is covered by the audit in the plan, not here.

const taggedImage1 = "quay.io/che/image-1:latest"
const taggedImage2 = "quay.io/che/image-2:7.123.0"

// backends drives the two reconcilers over one image list, in the order the controller registers
// them — the native backend first, so that it does not depend on the community one succeeding —
// and reports what each actually provisioned. An absent object contributes nothing, which is the
// same thing as a backend serving no image.
func backends(t *testing.T, ctx *chetypes.DeployContext, images []string) (pinned []string, prepulled []string) {
	t.Helper()

	newPass(ctx)
	provider := &fakeExternalImagesProvider{images: images}

	p := &PinnedImageSetReconciler{externalImages: provider}
	test.EnsureReconcile(t, ctx, p.Reconcile)

	ip := imagepuller.NewImagePullerWithImagesProvider(provider)
	test.EnsureReconcile(t, ctx, ip.Reconcile)

	return readPinnedImageSet(t, ctx), readImagePuller(t, ctx)
}

// withoutImagePullerAPI takes the community operator off the cluster, leaving the native
// backend usable. That is the default OpenShift cluster: the Kubernetes Image Puller is a
// separate optional operator that nobody has to install.
func withoutImagePullerAPI(ctx *chetypes.DeployContext) *chetypes.DeployContext {
	discoveryClient := ctx.ClusterAPI.DiscoveryClient.(*fakeDiscovery.FakeDiscovery)

	kept := make([]*metav1.APIResourceList, 0, len(discoveryClient.Resources))
	for _, resourceList := range discoveryClient.Resources {
		resources := make([]metav1.APIResource, 0, len(resourceList.APIResources))
		for _, resource := range resourceList.APIResources {
			if resource.Name != "kubernetesimagepullers" {
				resources = append(resources, resource)
			}
		}

		resourceList.APIResources = resources
		kept = append(kept, resourceList)
	}

	discoveryClient.Resources = kept

	return ctx
}

// newPass starts a fresh reconcile pass over an existing context. The controller builds a new
// DeployContext for every pass, so the image list memo on it is per pass; a test that drives
// two passes over one context has to say so, or the second pass allocates from the first one's
// list. Deliberately not called between the two reconcilers — sharing the list across those is
// the whole point of the memo.
func newPass(ctx *chetypes.DeployContext) {
	ctx.PrePullImages = nil
	ctx.PrePullBackends = nil
}

// TestATagInTheListDoesNotCostTheDigestsTheirPinning is a regression test for a defect that
// made the whole feature a no-op on the cluster it was written for.
//
// The image puller reconciler used to fail the reconcile whenever an image went unserved for a
// reason it would have solved, including a plain tag reference it could have pre-pulled. On a
// default OpenShift cluster the community operator is not installed, so that condition was met
// by any tag in the list — and a released list carries eight of them today. Because the
// reconciler chain stops at the first reconciler that is not done, the PinnedImageSet
// reconciler never ran and not one digest referenced image was pinned.
//
// Two things had to change and both are asserted here: the reconciler only fails when no
// backend at all is usable, and the native backend is registered first so that it does not
// depend on the community one succeeding.
func TestATagInTheListDoesNotCostTheDigestsTheirPinning(t *testing.T) {
	ctx := withoutImagePullerAPI(newTestCtx(true))
	images := []string{image1, taggedImage1, image2}

	provider := &fakeExternalImagesProvider{images: images}

	ip := imagepuller.NewImagePullerWithImagesProvider(provider)
	_, done, err := ip.Reconcile(ctx)
	assert.NoError(t, err, "a tag with no community operator must not fail the reconcile")
	assert.True(t, done, "a reconciler that is not done stops the chain before anything is pinned")

	pinned, prepulled := backends(t, ctx, images)

	assert.Equal(t, []string{image1, image2}, pinned, "the digest referenced images must still be pinned")
	assert.Empty(t, prepulled, "there is no community backend to pre-pull the tag")
}

// TestNoBackendAtAllIsStillAnError keeps the narrowing from going too far. Where neither backend
// can run, an administrator who asked for pre-pulling gets none of it, and the long standing
// error is the only thing that says so.
func TestNoBackendAtAllIsStillAnError(t *testing.T) {
	ctx := withoutImagePullerAPI(newTestCtxWithoutPinnedImageSetAPI(true))

	ip := imagepuller.NewImagePullerWithImagesProvider(&fakeExternalImagesProvider{images: []string{image1}})
	_, done, err := ip.Reconcile(ctx)

	assert.Error(t, err)
	assert.False(t, done)
}

// TestPinningOnlyIsNotBlockedByAMissingImagePuller covers an opted in cluster with no community
// operator installed. Pinning what can be pinned is a partial result, not a failure, so the
// images it cannot carry are left to be pulled on demand rather than failing the reconcile.
func TestPinningOnlyIsNotBlockedByAMissingImagePuller(t *testing.T) {
	ctx := withoutImagePullerAPI(newTestCtx(true))

	images := []string{image1, taggedImage1}
	provider := &fakeExternalImagesProvider{images: images}

	ip := imagepuller.NewImagePullerWithImagesProvider(provider)
	_, done, err := ip.Reconcile(ctx)
	assert.NoError(t, err)
	assert.True(t, done)

	pinned, _ := backends(t, ctx, images)
	assert.Equal(t, []string{image1}, pinned)
}

// reconcileBoth drives the two reconcilers over a provider that may fail, and returns what each
// one reported. Unlike backends it does not assert that they finish, because the behaviour under
// test is precisely whether they do.
func reconcileBoth(t *testing.T, ctx *chetypes.DeployContext, provider *fakeExternalImagesProvider) (pinnedDone bool, pinnedErr error, prepullDone bool, prepullErr error) {
	t.Helper()

	newPass(ctx)

	p := &PinnedImageSetReconciler{externalImages: provider}
	_, pinnedDone, pinnedErr = p.Reconcile(ctx)

	ip := imagepuller.NewImagePullerWithImagesProvider(provider)
	_, prepullDone, prepullErr = ip.Reconcile(ctx)

	return pinnedDone, pinnedErr, prepullDone, prepullErr
}

// TestAnUnknownImageListLeavesBothBackendsAlone is a regression test for two defects in the
// escape hatch, one of which the split introduced.
//
// CHE_OPERATOR__GET_EXTERNAL_IMAGES__IGNORE_FAILURES exists so that a failure to fetch the image
// list does not block the rest of the reconcile. It was honoured by the image puller only, so on
// OpenShift the PinnedImageSet reconciler failed the chain anyway and the hatch did nothing.
//
// The second defect is the one that makes copying the check across insufficient. The provider
// returns an empty slice along with its error, and a backend now withdraws when its own share of
// the list empties — so simply carrying on would have read "the fetch failed" as "there is
// nothing to pre-pull" and deleted *both* objects over a transient dashboard error, unpinning
// every image on every node. Pre-epic the uninstall only happened when the feature was switched
// off, so that would have been a new way to lose a working configuration.
func TestAnUnknownImageListLeavesBothBackendsAlone(t *testing.T) {
	t.Setenv("CHE_OPERATOR__GET_EXTERNAL_IMAGES__IGNORE_FAILURES", "true")

	ctx := newTestCtx(true)

	pinned, prepulled := backends(t, ctx, []string{image1, taggedImage1})
	assert.Equal(t, []string{image1}, pinned)
	assert.Equal(t, []string{taggedImage1}, prepulled)

	failing := &fakeExternalImagesProvider{err: errors.New("dashboard is unreachable")}
	pinnedDone, pinnedErr, prepullDone, prepullErr := reconcileBoth(t, ctx, failing)

	assert.NoError(t, pinnedErr, "the escape hatch has to unblock the OpenShift chain too")
	assert.True(t, pinnedDone, "a reconciler that is not done stops the chain, which is what the hatch exists to avoid")
	assert.NoError(t, prepullErr)
	assert.True(t, prepullDone)

	pinned, prepulled = readPinnedImageSet(t, ctx), readImagePuller(t, ctx)
	assert.Equal(t, []string{image1}, pinned, "an unreachable dashboard must not unpin anything")
	assert.Equal(t, []string{taggedImage1}, prepulled, "an unreachable dashboard must not remove the DaemonSet")
}

// TestAFailedFetchStillFailsWithoutTheEscapeHatch keeps the default strict. The hatch is opt-in
// precisely because a silently missing image list is worth a failed reconcile.
func TestAFailedFetchStillFailsWithoutTheEscapeHatch(t *testing.T) {
	ctx := newTestCtx(true)
	failing := &fakeExternalImagesProvider{err: errors.New("dashboard is unreachable")}

	pinnedDone, pinnedErr, prepullDone, prepullErr := reconcileBoth(t, ctx, failing)

	assert.Error(t, pinnedErr)
	assert.False(t, pinnedDone)
	assert.Error(t, prepullErr)
	assert.False(t, prepullDone)
}

// TestAnAdminSuppliedListSurvivesAFailedFetch covers the case where the failed fetch is simply
// irrelevant. An administrator supplied list replaces the computed one, so nothing about it
// depends on the endpoint that failed and pre-pulling proceeds in full rather than pausing.
func TestAnAdminSuppliedListSurvivesAFailedFetch(t *testing.T) {
	t.Setenv("CHE_OPERATOR__GET_EXTERNAL_IMAGES__IGNORE_FAILURES", "true")

	ctx := newTestCtx(true)
	ctx.CheCluster.Spec.Components.ImagePuller.Spec.Images = "a=" + image1 + ";b=" + taggedImage1 + ";"
	assert.NoError(t, ctx.ClusterAPI.Client.Update(context.TODO(), ctx.CheCluster))

	failing := &fakeExternalImagesProvider{err: errors.New("dashboard is unreachable")}
	_, pinnedErr, _, prepullErr := reconcileBoth(t, ctx, failing)
	assert.NoError(t, pinnedErr)
	assert.NoError(t, prepullErr)

	assert.Equal(t, []string{image1}, readPinnedImageSet(t, ctx))
	assert.Equal(t, []string{taggedImage1}, readImagePuller(t, ctx))
}

func readPinnedImageSet(t *testing.T, ctx *chetypes.DeployContext) []string {
	t.Helper()

	pinnedImageSet := &mcfgv1.PinnedImageSet{}
	exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
		context.TODO(),
		// Derived the way the code derives it, so that a test moving the CheCluster reads the
		// set that CheCluster would actually own rather than silently reading nothing.
		types.NamespacedName{Name: getPinnedImageSetName(ctx)},
		pinnedImageSet,
	)
	assert.NoError(t, err)

	images := make([]string, 0)
	if !exists {
		return images
	}

	for _, pinnedImage := range pinnedImageSet.Spec.PinnedImages {
		images = append(images, string(pinnedImage.Name))
	}

	return images
}

func readImagePuller(t *testing.T, ctx *chetypes.DeployContext) []string {
	t.Helper()

	kubernetesImagePuller := &chev1alpha1.KubernetesImagePuller{}
	exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
		context.TODO(),
		// Derived from the CheCluster, the way the community backend names its object, so
		// that a test moving the CheCluster reads that CheCluster's object rather than
		// silently reading nothing and passing.
		types.NamespacedName{
			Namespace: ctx.CheCluster.Namespace,
			Name:      ctx.CheCluster.Name + "-image-puller",
		},
		kubernetesImagePuller,
	)
	assert.NoError(t, err)

	if !exists {
		return make([]string, 0)
	}

	return imagepuller.ImagesFromSpecField(kubernetesImagePuller.Spec.Images)
}

// TestAllocationIsRealised is the central assertion of the split: the allocation is only a
// decision, and a decision that no object carries pre-pulls nothing. It compares what the two
// objects hold against what Allocate decided, so that a bucket silently dropped on its way into
// a spec cannot pass.
func TestAllocationIsRealised(t *testing.T) {
	testCases := []struct {
		name   string
		images []string
	}{
		{
			name:   "A mixed list, which is the case the split exists for",
			images: []string{image1, taggedImage1, image2, taggedImage2},
		},
		{
			name:   "Digest references only, as a released default install is",
			images: []string{image1, image2},
		},
		{
			name:   "Tag references only, as an administrator supplied list may be",
			images: []string{taggedImage1, taggedImage2},
		},
		{
			name:   "Duplicates, which must not be pre-pulled twice",
			images: []string{image1, image1, taggedImage1, taggedImage1},
		},
		{
			name:   "No images at all",
			images: []string{},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := newTestCtx(true)

			expected := imagepuller.Allocate(testCase.images, imagepuller.GetCapabilities(ctx))
			pinned, prepulled := backends(t, ctx, testCase.images)

			assert.Equal(t, expected.Pinned, pinned)
			assert.Equal(t, expected.Prepulled, prepulled)
		})
	}
}

// TestNoImageIsServedTwice guards the one failure the split makes possible that neither backend
// could produce alone. Pre-pulling an image twice is not merely wasteful: under a PinnedImageSet
// the image is exempt from garbage collection, so a duplicate entry in the community backend
// would keep a DaemonSet alive for an image that needs nobody to hold it.
func TestNoImageIsServedTwice(t *testing.T) {
	images := []string{image1, taggedImage1, image2, taggedImage2, image1}

	ctx := newTestCtx(true)
	pinned, prepulled := backends(t, ctx, images)

	served := make(map[string]int, len(images))
	for _, image := range append(append([]string{}, pinned...), prepulled...) {
		served[image]++
	}

	for image, count := range served {
		assert.Equalf(t, 1, count, "image %s is served %d times", image, count)
	}

	// Nothing may be lost either: on this cluster both backends are usable, so between them
	// they have to account for every distinct image in the input.
	assert.Len(t, served, 4)
}

// TestEmptyHalfRemovesItsObject covers the uninstall side, which is where the old mutually
// exclusive design was wrong. A backend now withdraws when its own share of the list empties,
// not when an administrator flips a backend field, so a list that drifts to all digests removes
// the DaemonSet without anybody changing the configuration.
func TestEmptyHalfRemovesItsObject(t *testing.T) {
	t.Run("A list that becomes all digests removes the Kubernetes Image Puller", func(t *testing.T) {
		ctx := newTestCtx(true)

		pinned, prepulled := backends(t, ctx, []string{image1, taggedImage1})
		assert.Equal(t, []string{image1}, pinned)
		assert.Equal(t, []string{taggedImage1}, prepulled)

		pinned, prepulled = backends(t, ctx, []string{image1, image2})
		assert.Equal(t, []string{image1, image2}, pinned)
		assert.Empty(t, prepulled)

		exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
			context.TODO(),
			types.NamespacedName{Namespace: "eclipse-che", Name: "eclipse-che-image-puller"},
			&chev1alpha1.KubernetesImagePuller{},
		)
		assert.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("A list that becomes all tags removes the PinnedImageSet", func(t *testing.T) {
		ctx := newTestCtx(true)

		pinned, _ := backends(t, ctx, []string{image1, taggedImage1})
		assert.Equal(t, []string{image1}, pinned)

		pinned, prepulled := backends(t, ctx, []string{taggedImage1, taggedImage2})
		assert.Empty(t, pinned)
		assert.Equal(t, []string{taggedImage1, taggedImage2}, prepulled)

		exists, err := ctx.ClusterAPI.ClientWrapper.GetIgnoreNotFound(
			context.TODO(),
			types.NamespacedName{Name: "eclipse-che-eclipse-che"},
			&mcfgv1.PinnedImageSet{},
		)
		assert.NoError(t, err)
		assert.False(t, exists)
	})
}

// TestAdminSuppliedListIsSplitToo is the case Ilya's own Sandbox configuration falls into: a
// hand written list mixing both reference formats. It used to be passed through untouched, so
// on the native backend it would have pre-pulled nothing at all.
func TestAdminSuppliedListIsSplitToo(t *testing.T) {
	ctx := newTestCtx(true)
	ctx.CheCluster.Spec.Components.ImagePuller.Spec.Images = "a=" + image1 + ";b=" + taggedImage1 + ";"

	// Persisted rather than only set in memory. Reconcilers reload the CheCluster when they
	// touch its finalizers, so an in-memory-only spec survives or not depending on which
	// reconciler happens to run first — which is a property of the fixture, not of the code
	// under test.
	assert.NoError(t, ctx.ClusterAPI.Client.Update(context.TODO(), ctx.CheCluster))

	// The computed list is deliberately different, since an administrator supplied list
	// replaces it rather than adding to it.
	pinned, prepulled := backends(t, ctx, []string{image2, taggedImage2})

	assert.Equal(t, []string{image1}, pinned)
	assert.Equal(t, []string{taggedImage1}, prepulled)
}

// TestAllocationIsReportedWhereNothingIsPinned covers the cluster the accounting matters most
// on. Where the native backend cannot run, no PinnedImageSet exists to read the pinned count
// back from, so the summary is the only record that the images went somewhere.
func TestAllocationIsReportedWhereNothingIsPinned(t *testing.T) {
	ctx := newTestCtx(true)

	// Only the community backend is served here. The PinnedImageSet API is simply absent,
	// which is the case on every OpenShift version before 4.20 and on Kubernetes.
	discoveryClient := ctx.ClusterAPI.DiscoveryClient.(*fakeDiscovery.FakeDiscovery)
	discoveryClient.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "che.eclipse.org/v1alpha1",
			APIResources: []metav1.APIResource{{Name: "kubernetesimagepullers"}},
		},
	}

	p := &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: []string{image1, taggedImage1}},
	}

	test.EnsureReconcile(t, ctx, p.Reconcile)

	// total/pinned/prepulled/excluded.
	assert.Equal(t, "2/0/2/0", p.lastAllocation[reportingKey(ctx)])
}

// TestAllocationIsNotReportedWhenPrePullingIsOff keeps the summary meaningful. An operator that
// announced an allocation of zero on every cluster that never asked for pre-pulling would make
// the line worthless as a signal.
func TestAllocationIsNotReportedWhenPrePullingIsOff(t *testing.T) {
	ctx := newTestCtx(false)

	p := &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: []string{image1, taggedImage1}},
	}

	test.EnsureReconcile(t, ctx, p.Reconcile)
	assert.Empty(t, p.lastAllocation)
}

// TestAllocationIsReportedOnChangeOnly stops the reconcile loop, which runs far more often than
// the image list changes, from repeating the same line indefinitely.
func TestAllocationIsReportedOnChangeOnly(t *testing.T) {
	ctx := newTestCtx(true)

	p := &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: []string{image1, taggedImage1}},
	}

	capabilities := imagepuller.GetCapabilities(ctx)
	allocation := imagepuller.Allocate([]string{image1, taggedImage1}, capabilities)

	p.logAllocation(ctx, allocation, capabilities)
	assert.Equal(t, "2/1/1/0", p.lastAllocation[reportingKey(ctx)])

	// Reported once, and the record does not move while the counts do not.
	p.logAllocation(ctx, allocation, capabilities)
	assert.Equal(t, "2/1/1/0", p.lastAllocation[reportingKey(ctx)])

	changed := imagepuller.Allocate([]string{image1, image2, taggedImage1}, capabilities)
	p.logAllocation(ctx, changed, capabilities)
	assert.Equal(t, "3/2/1/0", p.lastAllocation[reportingKey(ctx)])
}

// TestExcludedImagesAreReported covers the third bucket, which is the one nothing on the cluster
// records. Both backends persist what they serve; an image neither takes leaves no trace at all,
// so if the reason is not logged it cannot be recovered afterwards.
func TestExcludedImagesAreReported(t *testing.T) {
	ctx := withoutImagePullerAPI(newTestCtx(true))

	capabilities := imagepuller.GetCapabilities(ctx)
	allocation := imagepuller.Allocate([]string{image1, taggedImage1, taggedImage2}, capabilities)

	// Pinning is the only backend here, so the tags reach nobody.
	assert.Len(t, allocation.Excluded, 2)
	for _, exclusion := range allocation.Excluded {
		assert.Equal(t, imagepuller.ReasonNotPinnable, exclusion.Reason)
	}

	p := &PinnedImageSetReconciler{}
	p.logAllocation(ctx, allocation, capabilities)
	assert.Equal(t, "3/1/0/2", p.lastAllocation[reportingKey(ctx)])
}

// TestSingleNodeFallsBackToTheCommunityBackend is a regression test for a hole the split opened
// between the two reconcilers, on a platform neither of them was wrong about on its own.
//
// Whether a pool selects the worker role used to be checked only where the set is created, while
// the capability that drives allocation asked for the API and the machine-config-daemon alone.
// Both are present on single node OpenShift — only the worker pool is not, because every node is
// in the master pool. So every digest referenced image was allocated to a backend that then
// created nothing, the community backend was never offered them, and an administrator who set
// enable: true got no pre-pulling at all. Allocating by one answer and provisioning by another is
// what made it silent: each reconciler did exactly what it was told.
func TestSingleNodeFallsBackToTheCommunityBackend(t *testing.T) {
	ctx := newTestCtxWithMachineConfigPools(newMachineConfigPool("master"))

	pinned, prepulled := backends(t, ctx, []string{image1, taggedImage1})

	assert.Empty(t, pinned, "no pool would consume a set here, so nothing can be pinned")
	assert.Equal(t, []string{image1, taggedImage1}, prepulled, "every image must still reach a backend")
}

// TestSingleNodeWithNoCommunityBackendIsAnError completes the pair. Where pinning is unavailable
// for want of a pool and the community operator is not installed either, an administrator who
// asked for pre-pulling genuinely gets none, and that is the one case the long standing error
// still has to be raised for.
func TestSingleNodeWithNoCommunityBackendIsAnError(t *testing.T) {
	ctx := withoutImagePullerAPI(newTestCtxWithMachineConfigPools(newMachineConfigPool("master")))

	ip := imagepuller.NewImagePullerWithImagesProvider(&fakeExternalImagesProvider{images: []string{image1}})
	_, done, err := ip.Reconcile(ctx)

	assert.Error(t, err)
	assert.False(t, done)
}

// TestDisabledPrePullingTearsDownWithoutTheDashboard is a regression test for a defect that
// outlived the feature that caused it: the image list was fetched before anything checked
// whether pre-pulling was enabled at all.
//
// The provider reads the dashboard over HTTP. So an administrator setting `enable: false`
// while the dashboard was unreachable got a failed reconcile instead of a teardown, and both
// backends kept serving images for a feature that was switched off. The PinnedImageSet is the
// part that makes this worth fixing rather than tolerating: it is cluster-scoped, so what
// lingers is not confined to the CheCluster's namespace.
//
// Deleting the CheCluster was never affected — Finalize calls the uninstall directly and does
// not consult the provider — so this is about turning the feature off, not about removing it.
func TestDisabledPrePullingTearsDownWithoutTheDashboard(t *testing.T) {
	ctx := newTestCtx(true)

	// Provision both backends while the dashboard is healthy.
	pinned, prepulled := backends(t, ctx, []string{image1, taggedImage1})
	assert.Equal(t, []string{image1}, pinned)
	assert.Equal(t, []string{taggedImage1}, prepulled)

	// Now switch pre-pulling off and break the dashboard in the same breath.
	ctx.CheCluster.Spec.Components.ImagePuller.Enable = false
	newPass(ctx)
	provider := &fakeExternalImagesProvider{err: errors.New("dashboard is unreachable")}

	p := &PinnedImageSetReconciler{externalImages: provider}
	_, pinnedDone, pinnedErr := p.Reconcile(ctx)
	assert.NoError(t, pinnedErr, "a disabled feature must not fail the reconcile over a dashboard it does not need")
	assert.True(t, pinnedDone, "a reconciler that is not done stops every reconciler registered after it")

	ip := imagepuller.NewImagePullerWithImagesProvider(provider)
	_, prepullDone, prepullErr := ip.Reconcile(ctx)
	assert.NoError(t, prepullErr)
	assert.True(t, prepullDone)

	assert.Empty(t, readPinnedImageSet(t, ctx), "the cluster-scoped PinnedImageSet must be gone")
	assert.Empty(t, readImagePuller(t, ctx), "the community backend must be gone")

	// The point is not that the error was tolerated, it is that the dashboard was never
	// asked. Tolerating the error would leave the fix hostage to whatever the provider does
	// on the way to failing.
	assert.Equal(t, 0, provider.calls, "a disabled feature must not call the dashboard at all")
}

// TestAnUnknownImageListIsSharedAcrossBothBackends is the mirror of
// TestBothBackendsAllocateFromOneList, for the degraded path.
//
// When the escape hatch swallows a failed fetch, the list is *unknown*, and that verdict has
// to reach the second reconciler as well. Its own provider might succeed where the first one
// failed — the two read the dashboard a moment apart — and if it did, one backend would hold
// the cluster steady on an unknown list while the other rewrote itself from a fresh one. The
// images would then be split against two different inputs, which is the very thing the hatch
// exists to avoid.
func TestAnUnknownImageListIsSharedAcrossBothBackends(t *testing.T) {
	t.Setenv("CHE_OPERATOR__GET_EXTERNAL_IMAGES__IGNORE_FAILURES", "true")

	ctx := newTestCtx(true)

	pinned, prepulled := backends(t, ctx, []string{image1, taggedImage1})
	assert.Equal(t, []string{image1}, pinned)
	assert.Equal(t, []string{taggedImage1}, prepulled)

	newPass(ctx)

	failing := &fakeExternalImagesProvider{err: errors.New("dashboard is unreachable")}
	p := &PinnedImageSetReconciler{externalImages: failing}
	_, pinnedDone, pinnedErr := p.Reconcile(ctx)
	assert.NoError(t, pinnedErr)
	assert.True(t, pinnedDone)

	// The community reconciler's own provider would have succeeded, with a different list.
	healthy := &fakeExternalImagesProvider{images: []string{taggedImage2}}
	ip := imagepuller.NewImagePullerWithImagesProvider(healthy)
	_, prepullDone, prepullErr := ip.Reconcile(ctx)
	assert.NoError(t, prepullErr)
	assert.True(t, prepullDone)

	assert.Equal(t, 0, healthy.calls, "the unknown verdict must carry to the second reconciler, not be re-fetched")
	assert.Equal(t, []string{image1}, readPinnedImageSet(t, ctx), "an unknown list must leave the cluster as it is")
	assert.Equal(t, []string{taggedImage1}, readImagePuller(t, ctx), "an unknown list must not be rewritten from a luckier fetch")
}

// driftingImagesProvider returns a different list on each call, which is what two live
// providers reading a dashboard mid-change amount to.
type driftingImagesProvider struct {
	lists [][]string
	calls int
}

func (p *driftingImagesProvider) Get(ctx *chetypes.DeployContext) ([]string, error) {
	index := min(p.calls, len(p.lists)-1)
	p.calls++

	return p.lists[index], nil
}

// TestBothBackendsAllocateFromOneList is a regression test for a defect no other test in this
// package could have caught, because they all hand the two reconcilers the same provider.
//
// Production does not. Each reconciler is constructed with its own ExternalImagesProvider, and
// Get fetches from the dashboard afresh every call — so a pass used to allocate twice, from two
// responses. If the list changed in between, an image could be pinned by one allocation and
// prepulled by neither, or served by both at once. Sharing a provider instance would not have
// fixed it either; the resolved list is what has to be shared, which is why it is memoized on
// the DeployContext the controller rebuilds each pass.
func TestBothBackendsAllocateFromOneList(t *testing.T) {
	ctx := newTestCtx(true)
	provider := &driftingImagesProvider{lists: [][]string{
		{image1, taggedImage1},
		{image2, taggedImage2},
	}}

	p := &PinnedImageSetReconciler{externalImages: provider}
	test.EnsureReconcile(t, ctx, p.Reconcile)

	// A second provider, as the controller wires it, drifted to an entirely different list.
	second := &driftingImagesProvider{lists: [][]string{{image2, taggedImage2}}}
	ip := imagepuller.NewImagePullerWithImagesProvider(second)
	test.EnsureReconcile(t, ctx, ip.Reconcile)

	assert.Equal(t, 1, provider.calls, "the list must be resolved once per pass, not once per reconciler")
	assert.Equal(t, 0, second.calls, "the second reconciler must read the memo rather than fetch again")

	assert.Equal(t, []string{image1}, readPinnedImageSet(t, ctx))
	assert.Equal(t, []string{taggedImage1}, readImagePuller(t, ctx),
		"the community backend must serve the complement of what was pinned, from the same list")
}

// countingDiscovery counts full API enumerations, which is what makes capability detection
// expensive: the operator's discovery client is not cached, so each one is a request per API
// group on the cluster.
type countingDiscovery struct {
	discovery.DiscoveryInterface
	calls *int
}

func (c countingDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	*c.calls++

	return c.DiscoveryInterface.ServerGroupsAndResources()
}

// TestCapabilitiesAreResolvedOncePerPass puts a ceiling on how often a reconcile pass may ask
// the cluster what it is capable of. Both reconcilers ask, and each asks more than once —
// Reconcile itself, the unsatisfiable-request log, and the teardown paths — so without the memo
// on the DeployContext the count rose with every new caller and nothing would have noticed.
//
// The budget matters most on the opted out path, which is every cluster by default and which
// gets no feature out of the traffic at all.
func TestCapabilitiesAreResolvedOncePerPass(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		optIn  bool
		budget int
	}{
		// One enumeration per backend probe, and nothing beyond that. In particular the
		// opted out path must not cost more than the opted in one: it used to, by probing
		// the API again to tear down a set it had never created.
		{name: "Opted out, the default", optIn: false, budget: 2},
		{name: "Opted in", optIn: true, budget: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := newTestCtx(testCase.optIn)
			calls := 0
			ctx.ClusterAPI.DiscoveryClient = countingDiscovery{ctx.ClusterAPI.DiscoveryClient, &calls}

			provider := &fakeExternalImagesProvider{images: []string{image1, taggedImage1}}

			p := &PinnedImageSetReconciler{externalImages: provider}
			test.EnsureReconcile(t, ctx, p.Reconcile)

			ip := imagepuller.NewImagePullerWithImagesProvider(provider)
			test.EnsureReconcile(t, ctx, ip.Reconcile)

			assert.LessOrEqual(t, calls, testCase.budget,
				"a reconcile pass must not re-enumerate the API for an answer it already has")
		})
	}
}

// TestTeardownIsSkippedWithoutTheFinalizer covers what an install that never opted in does to
// the cluster, which should be nothing. The finalizer is the operator's record that it has a
// set to remove; without one, probing the API and deleting a cluster scoped object it never
// created is work on every reconcile whose only possible outcome is a failure that stops the
// reconciler chain for a feature nobody switched on.
func TestTeardownIsSkippedWithoutTheFinalizer(t *testing.T) {
	ctx := newTestCtx(false)
	calls := 0
	ctx.ClusterAPI.DiscoveryClient = countingDiscovery{ctx.ClusterAPI.DiscoveryClient, &calls}

	p := &PinnedImageSetReconciler{}

	done, err := p.uninstallPinnedImageSet(ctx)
	assert.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, 0, calls, "nothing of ours exists, so the cluster must not be asked anything")
}

// TestTeardownStillRunsWithTheFinalizer is the other half. The gate must not cost the teardown
// that an administrator opting back out depends on, nor the one that reclaims a set left by an
// older build across an upgrade.
func TestTeardownStillRunsWithTheFinalizer(t *testing.T) {
	ctx := newTestCtx(true)

	pinned, _ := backends(t, ctx, []string{image1, taggedImage1})
	assert.Equal(t, []string{image1}, pinned)
	assert.Contains(t, ctx.CheCluster.Finalizers, finalizerName)

	ctx.CheCluster.Spec.Components.ImagePuller.EnablePinnedImageSet = false
	newPass(ctx)

	p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1, taggedImage1}}}
	test.EnsureReconcile(t, ctx, p.Reconcile)

	assert.Empty(t, readPinnedImageSet(t, ctx))
	assert.NotContains(t, ctx.CheCluster.Finalizers, finalizerName)
}

// renameCheCluster moves the CheCluster of an existing context, so that two contexts can be
// built whose PinnedImageSet names collide.
func renameCheCluster(t *testing.T, ctx *chetypes.DeployContext, namespace string, name string) {
	t.Helper()

	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Delete(context.TODO(), ctx.CheCluster))

	ctx.CheCluster.Namespace = namespace
	ctx.CheCluster.Name = name
	ctx.CheCluster.ResourceVersion = ""

	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Create(context.TODO(), ctx.CheCluster))
}

// TestTwoCheClustersDoNotFightOverOneSet covers the one way two CheClusters can collide on a
// cluster-scoped object: the set is named by joining the namespace and the name with a dash,
// so `foo/bar-baz` and `foo-bar/baz` both ask for `foo-bar-baz`.
//
// Overwriting would not be a quiet wrong answer. Each operator would rewrite the other's image
// list on every pass, and because the Machine Config Operator merges every set targeting a
// pool, the whole worker pool would roll for each flip, indefinitely, for both tenants.
func TestTwoCheClustersDoNotFightOverOneSet(t *testing.T) {
	first := newTestCtx(true)
	renameCheCluster(t, first, "foo", "bar-baz")

	p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1}}}
	test.EnsureReconcile(t, first, p.Reconcile)
	assert.Equal(t, []string{image1}, readPinnedImageSet(t, first))

	// A second CheCluster deriving the same name, reconciled against the same cluster.
	second := newTestCtx(true)
	renameCheCluster(t, second, "foo-bar", "baz")
	second.ClusterAPI = first.ClusterAPI
	assert.Equal(t, getPinnedImageSetName(first), getPinnedImageSetName(second), "the premise of this test")

	// Driven over a digest and a tag, because losing the name must not cost the digest its
	// pre-pulling. Pinning is simply unavailable to this CheCluster, which is the same
	// situation as a cluster that cannot pin at all, and there the community backend takes
	// everything. Deciding this after allocation instead left the digest allocated to a set
	// that was never created and refused by the community backend, so nobody pre-pulled it.
	lost := []string{image2, taggedImage2}

	q := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: lost}}
	_, done, err := q.Reconcile(second)
	assert.NoError(t, err, "a collision must not stop the reconciler chain of the CheCluster that lost")
	assert.True(t, done)

	ip := imagepuller.NewImagePullerWithImagesProvider(&fakeExternalImagesProvider{images: lost})
	test.EnsureReconcile(t, second, ip.Reconcile)

	assert.Equal(t, []string{image1}, readPinnedImageSet(t, second),
		"the set must still hold the first CheCluster's images")
	assert.ElementsMatch(t, lost, readImagePuller(t, second),
		"every image of the CheCluster that lost the name must still be pre-pulled, digests included")
	assert.NotContains(t, second.CheCluster.Finalizers, finalizerName,
		"nothing was created, so there is nothing to finalize")

	// And the loser must not delete the winner's set when it is finalized.
	assert.True(t, q.Finalize(second))
	assert.Equal(t, []string{image1}, readPinnedImageSet(t, second))
}

// TestASetWithoutOwnershipLabelsIsAdopted keeps the collision guard from stranding the sets
// written before the operator recorded an owner. Refusing those would orphan a cluster-scoped
// object across an upgrade, with the finalizer released and nothing left to reclaim it.
func TestASetWithoutOwnershipLabelsIsAdopted(t *testing.T) {
	ctx := newTestCtx(true)

	legacy := &mcfgv1.PinnedImageSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:   getPinnedImageSetName(ctx),
			Labels: map[string]string{constants.MachineConfigPoolRoleLabelKey: constants.MachineConfigPoolRole},
		},
		Spec: mcfgv1.PinnedImageSetSpec{
			PinnedImages: []mcfgv1.PinnedImageRef{{Name: mcfgv1.ImageDigestFormat(image2)}},
		},
	}
	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Create(context.TODO(), legacy))

	p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1}}}
	test.EnsureReconcile(t, ctx, p.Reconcile)

	assert.Equal(t, []string{image1}, readPinnedImageSet(t, ctx), "the unowned set must be taken over")
	assert.Contains(t, ctx.CheCluster.Finalizers, finalizerName)
}

// TestAReorderedSetIsNotRewritten covers spec.pinnedImages being declared listType=map with
// listMapKey=name: the entries are keyed by name and their order means nothing, so two
// orderings of the same names are the same set and must not produce a write.
//
// Not a loop if it were got wrong — this operator writes with Update rather than Apply, so its
// own order wins and the next pass agrees. It would be one needless write, one "Object
// updated" line and one printed diff each time something else touched the object, which is
// exactly the noise an administrator would chase.
func TestAReorderedSetIsNotRewritten(t *testing.T) {
	ctx := newTestCtx(true)
	p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1, image2}}}
	test.EnsureReconcile(t, ctx, p.Reconcile)

	stored := &mcfgv1.PinnedImageSet{}
	key := types.NamespacedName{Name: getPinnedImageSetName(ctx)}
	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Get(context.TODO(), key, stored))
	assert.Len(t, stored.Spec.PinnedImages, 2, "the premise: more than one entry to reorder")

	// Reordered by something other than this operator, which is the only way it can happen.
	stored.Spec.PinnedImages[0], stored.Spec.PinnedImages[1] = stored.Spec.PinnedImages[1], stored.Spec.PinnedImages[0]
	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Update(context.TODO(), stored))

	unchanged := stored.ResourceVersion

	newPass(ctx)
	test.EnsureReconcile(t, ctx, p.Reconcile)

	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Get(context.TODO(), key, stored))
	assert.Equal(t, unchanged, stored.ResourceVersion,
		"the same names in another order are the same set, so there is nothing to write")

	// A genuine change to the names must still be written, or the guard above would be a way
	// of never updating the set at all.
	p.externalImages = &fakeExternalImagesProvider{images: []string{image1}}
	newPass(ctx)
	test.EnsureReconcile(t, ctx, p.Reconcile)

	assert.Equal(t, []string{image1}, readPinnedImageSet(t, ctx))
}
