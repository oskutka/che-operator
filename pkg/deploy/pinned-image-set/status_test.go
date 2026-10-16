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
	"bytes"
	"context"
	"testing"

	chev2 "github.com/eclipse-che/che-operator/api/v2"
	"github.com/eclipse-che/che-operator/pkg/common/test"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/stretchr/testify/assert"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	zaplog "sigs.k8s.io/controller-runtime/pkg/log/zap"

	"go.uber.org/zap/zapcore"
)

const pinnedImageSetName = "eclipse-che-eclipse-che"

// podmanInspectError is the verbatim error OpenShift 4.22 reports on a node that cannot
// resolve one of the pinned images.
const podmanInspectError = `failed to execute podman manifest inspect for ` +
	`"registry.access.redhat.com/ubi9/nonexistent@sha256:dead00000000000000000000000000000000000000000000000000000000beef": ` +
	`exit status 125`

func newMachineConfigNode(name string, setName string, failure string) *mcfgv1.MachineConfigNode {
	return &mcfgv1.MachineConfigNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: mcfgv1.MachineConfigNodeStatus{
			PinnedImageSets: []mcfgv1.MachineConfigNodeStatusPinnedImageSet{
				{Name: setName, LastFailedGenerationError: failure},
			},
		},
	}
}

func TestGetDegradedMessage(t *testing.T) {
	type testCase struct {
		name     string
		objects  []client.Object
		expected string
	}

	testCases := []testCase{
		{
			name:     "Healthy when there is no node at all",
			objects:  []client.Object{},
			expected: "",
		},
		{
			name: "Healthy when the node reconciled the set",
			// The Machine Config Operator clears the error once the set reconciles, so an empty
			// error is what a recovered node looks like.
			objects:  []client.Object{newMachineConfigNode("worker-0", pinnedImageSetName, "")},
			expected: "",
		},
		{
			name:     "Degraded with the per node cause",
			objects:  []client.Object{newMachineConfigNode("worker-0", pinnedImageSetName, podmanInspectError)},
			expected: "node worker-0: " + podmanInspectError,
		},
		{
			name: "Every failing node is reported, in a stable order",
			objects: []client.Object{
				newMachineConfigNode("worker-2", pinnedImageSetName, podmanInspectError),
				newMachineConfigNode("worker-0", pinnedImageSetName, podmanInspectError),
			},
			expected: "node worker-0: " + podmanInspectError + "; node worker-2: " + podmanInspectError,
		},
		{
			name:     "Errors reported for another set are ignored",
			objects:  []client.Object{newMachineConfigNode("worker-0", "other-set", podmanInspectError)},
			expected: "",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := newTestCtx(true)
			for _, object := range testCase.objects {
				assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Create(context.TODO(), object))
			}

			p := &PinnedImageSetReconciler{}

			actual, err := p.getDegradedMessage(ctx)
			assert.NoError(t, err)
			assert.Equal(t, testCase.expected, actual)
		})
	}
}

// TestDegradationDoesNotFailReconcile pins the decision that pre-pulling is an optimization:
// a degraded set must not stop the reconcile chain.
func TestDegradationDoesNotFailReconcile(t *testing.T) {
	ctx := newTestCtx(true)
	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Create(
		context.TODO(),
		newMachineConfigNode("worker-0", pinnedImageSetName, podmanInspectError),
	))

	p := &PinnedImageSetReconciler{
		externalImages: &fakeExternalImagesProvider{images: []string{image1}},
	}

	_, done, err := p.Reconcile(ctx)
	assert.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, "degraded: node worker-0: "+podmanInspectError, p.lastStatus[reportingKey(ctx)])
}

// TestStatusIsLoggedOnlyOnChange keeps the reconcile loop from logging the same line on every pass.
func TestStatusIsLoggedOnlyOnChange(t *testing.T) {
	ctx := newTestCtx(true)
	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Create(
		context.TODO(),
		newMachineConfigNode("worker-0", pinnedImageSetName, podmanInspectError),
	))

	p := &PinnedImageSetReconciler{}

	p.logPinnedImageSetStatus(ctx)
	assert.Equal(t, "degraded: node worker-0: "+podmanInspectError, p.lastStatus[reportingKey(ctx)])

	// Unchanged status, so nothing new is recorded.
	assert.False(t, p.setLastStatus(ctx, p.lastStatus[reportingKey(ctx)]))
	assert.True(t, p.setLastStatus(ctx, "degraded: "))
}

// TestStatusOnClusterWithoutMachineConfigNodes guards the reporting against a cluster that does
// not serve the resource. Listing one the cluster does not serve fails with a no-matches error,
// which must neither fail the reconcile nor be logged on every pass.
func TestStatusOnClusterWithoutMachineConfigNodes(t *testing.T) {
	ctx := newTestCtx(true)

	// PinnedImageSet is registered but MachineConfigNode deliberately is not, so the client
	// reproduces the no-matches failure the API server returns for an unserved resource.
	scheme := runtime.NewScheme()
	utilruntime.Must(chev2.AddToScheme(scheme))
	scheme.AddKnownTypes(mcfgv1.GroupVersion, &mcfgv1.PinnedImageSet{}, &mcfgv1.PinnedImageSetList{})
	metav1.AddToGroupVersion(scheme, mcfgv1.GroupVersion)

	noNodesClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	// Proves the premise: listing the unserved resource fails, and not with a not-found error.
	err := noNodesClient.List(context.TODO(), &mcfgv1.MachineConfigNodeList{})
	assert.Error(t, err)
	assert.False(t, k8serrors.IsNotFound(err))

	ctx.ClusterAPI.NonCachingClient = noNodesClient

	p := &PinnedImageSetReconciler{}

	p.logPinnedImageSetStatus(ctx)
	assert.Contains(t, p.lastStatus[reportingKey(ctx)], "error: ")
	assert.Contains(t, p.lastStatus[reportingKey(ctx)], "machineconfignodes")

	// Repeated evaluation of the same failure must not be reported again.
	previous := p.lastStatus[reportingKey(ctx)]
	p.logPinnedImageSetStatus(ctx)
	assert.Equal(t, previous, p.lastStatus[reportingKey(ctx)])
	assert.False(t, p.setLastStatus(ctx, previous))
}

// TestOneCheClusterDoesNotSuppressAnother is a regression test for state that was shared when
// the objects it described were not.
//
// The controller builds one PinnedImageSetReconciler at start up and reuses it for every
// CheCluster it watches, and the manager is not restricted to a namespace. lastStatus and
// lastAllocation were plain fields on that singleton, so the change detection was global: the
// second CheCluster to reach a given status was told nothing, because the first had already
// recorded the same string.
//
// Degradation is the case that makes this matter rather than merely untidy. Two tenants whose
// sets fail for the same reason — a registry outage, an unpullable digest shared between them —
// produce byte-identical status strings, so the collision is likeliest exactly when the report
// is most needed. And a PinnedImageSet carries no status of its own: this log line is the only
// place the failure surfaces at all, so suppressing it loses the information outright.
func TestOneCheClusterDoesNotSuppressAnother(t *testing.T) {
	first := newTestCtx(true)

	second := newTestCtx(true)
	second.CheCluster.Namespace = "eclipse-che-2"

	p := &PinnedImageSetReconciler{}

	assert.True(t, p.setLastStatus(first, "degraded: "+podmanInspectError))
	assert.True(
		t,
		p.setLastStatus(second, "degraded: "+podmanInspectError),
		"an identical status in another namespace is still this namespace's first report",
	)

	// Each is suppressed by its own history, which is the behaviour the single field was for.
	assert.False(t, p.setLastStatus(first, "degraded: "+podmanInspectError))
	assert.False(t, p.setLastStatus(second, "degraded: "+podmanInspectError))

	// And they recover independently.
	assert.True(t, p.setLastStatus(first, "degraded: "))
	assert.False(t, p.setLastStatus(second, "degraded: "+podmanInspectError))
}

// TestFinalizeForgetsTheCheCluster stops the two maps outliving the objects they describe. A
// CheCluster recreated under the same name must report from scratch rather than inherit the
// state of the one that was deleted, which would suppress its first report.
func TestFinalizeForgetsTheCheCluster(t *testing.T) {
	ctx := newTestCtx(true)

	p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1}}}
	test.EnsureReconcile(t, ctx, p.Reconcile)
	assert.NotEmpty(t, p.lastAllocation[reportingKey(ctx)])

	assert.True(t, p.Finalize(ctx))
	assert.Empty(t, p.lastStatus[reportingKey(ctx)])
	assert.Empty(t, p.lastAllocation[reportingKey(ctx)])
}

// captureLog redirects this package's logger into a buffer for the duration of one test.
// Needed because the behaviour under test is the log itself: lastStatus records that a
// transition happened, not whether it was worth telling anyone about.
func captureLog(t *testing.T) *bytes.Buffer {
	buf := &bytes.Buffer{}
	previous := logger

	logger = zaplog.New(zaplog.WriteTo(buf), zaplog.Level(zapcore.Level(-2))).WithName("pinned-image-set")
	t.Cleanup(func() { logger = previous })

	return buf
}

// TestAFirstHealthyPinReportsNoRecovery covers the line a fresh install sees. The recorded
// status starts empty and a healthy set records as "degraded: " with nothing after it, so the
// two differ on the very first sync and the transition looks real. Announcing a recovery from
// a degradation that was never reported is the single most alarming line in the log of an
// install where nothing is wrong.
func TestAFirstHealthyPinReportsNoRecovery(t *testing.T) {
	log := captureLog(t)

	ctx := newTestCtx(true)
	p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1}}}
	test.EnsureReconcile(t, ctx, p.Reconcile)

	assert.NotContains(t, log.String(), "no longer degraded",
		"a set that was never reported degraded must not report a recovery")
	assert.NotContains(t, log.String(), "is degraded")
}

// TestARealRecoveryIsStillReported is the other half: suppressing the spurious line must not
// cost the genuine one, which is the only notice that pre-pulling started working again.
func TestARealRecoveryIsStillReported(t *testing.T) {
	log := captureLog(t)

	ctx := newTestCtx(true)
	p := &PinnedImageSetReconciler{externalImages: &fakeExternalImagesProvider{images: []string{image1}}}

	node := newMachineConfigNode("worker-0", pinnedImageSetName, podmanInspectError)
	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Create(context.TODO(), node))

	test.EnsureReconcile(t, ctx, p.Reconcile)
	assert.Contains(t, log.String(), "is degraded")

	log.Reset()

	node.Status.PinnedImageSets[0].LastFailedGenerationError = ""
	assert.NoError(t, ctx.ClusterAPI.NonCachingClient.Update(context.TODO(), node))

	newPass(ctx)
	test.EnsureReconcile(t, ctx, p.Reconcile)
	assert.Contains(t, log.String(), "no longer degraded")
}
