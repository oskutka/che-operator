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
	"time"

	"github.com/eclipse-che/che-operator/pkg/common/chetypes"
	"github.com/eclipse-che/che-operator/pkg/common/constants"
	"github.com/eclipse-che/che-operator/pkg/common/diffs"
	"github.com/eclipse-che/che-operator/pkg/common/infrastructure"
	k8s_client "github.com/eclipse-che/che-operator/pkg/common/k8s-client"
	"github.com/eclipse-che/che-operator/pkg/common/reconciler"
	"github.com/eclipse-che/che-operator/pkg/common/utils"
	"github.com/eclipse-che/che-operator/pkg/deploy"
	imagepuller "github.com/eclipse-che/che-operator/pkg/deploy/image-puller"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var logger = ctrl.Log.WithName("pinned-image-set")

const finalizerName = "pinnedimagesets.finalizers.che.eclipse.org"

type PinnedImageSetReconciler struct {
	reconciler.Reconcilable

	// externalImages fetches the list when nothing has yet. The community backend holds a
	// provider of its own, so this is not the instance that resolves the list on every pass —
	// whichever reconciler runs first resolves it and the other reads the memo off the
	// DeployContext. That, and not a shared instance, is what makes the two allocate from the
	// same list; two live providers would simply fetch twice and could disagree.
	externalImages imagepuller.ImagesProvider

	// lastStatus suppresses repeating the same status on every reconcile. Keyed by CheCluster,
	// because the controller constructs one reconciler at start up and reuses it for every
	// CheCluster it watches, and the manager is not restricted to a namespace. A single field
	// would let one namespace suppress another's transition whenever the two strings agree,
	// which is exactly when they are both degraded — and this log line is the only place a
	// degraded set is reported at all, so the second tenant would simply never be told.
	lastStatus map[string]string

	// lastAllocation does the same for the allocation summary. It is kept apart from
	// lastStatus, which the degraded and misconfigured paths already share, so that the two
	// cannot suppress each other.
	lastAllocation map[string]string
}

// reportingKey identifies the CheCluster a suppressed log line belongs to.
func reportingKey(ctx *chetypes.DeployContext) string {
	return ctx.CheCluster.Namespace + "/" + ctx.CheCluster.Name
}

func NewPinnedImageSetReconciler() *PinnedImageSetReconciler {
	return &PinnedImageSetReconciler{
		externalImages: imagepuller.NewExternalImagesProvider(),
	}
}

func (p *PinnedImageSetReconciler) Reconcile(ctx *chetypes.DeployContext) (reconcile.Result, bool, error) {
	images, known, err := imagepuller.ImagesToPrePull(ctx, p.externalImages)
	if err != nil {
		return reconcile.Result{}, false, err
	}

	// An unknown list is not an empty one. Withdrawing the set here would unpin every image on
	// every node because the dashboard was briefly unreachable.
	if !known {
		return reconcile.Result{}, true, nil
	}

	capabilities := imagepuller.GetCapabilities(ctx)
	allocation := imagepuller.Allocate(images, capabilities)

	// Reported from here, and before the early return below, so that the accounting is logged
	// once per reconcile and on every cluster. Leaving it to whichever backend provisions
	// itself would mean two reconcilers reporting totals that disagree while a change rolls
	// out, and no total at all on a cluster that pre-pulls nothing.
	p.logAllocation(ctx, allocation, capabilities)
	p.logUnsatisfiableBackendRequest(ctx)

	if len(allocation.Pinned) == 0 {
		if done, err := p.uninstallPinnedImageSet(ctx); !done {
			return reconcile.Result{RequeueAfter: time.Second}, false, err
		}

		return reconcile.Result{}, true, nil
	}

	if done, err := p.syncPinnedImageSet(allocation.Pinned, ctx); !done {
		return reconcile.Result{RequeueAfter: time.Second}, false, err
	}

	return reconcile.Result{}, true, nil
}

func (p *PinnedImageSetReconciler) Finalize(ctx *chetypes.DeployContext) bool {
	done, err := p.uninstallPinnedImageSet(ctx)
	if err != nil {
		logger.Error(err, "Failed to uninstall PinnedImageSet")
	}

	if done {
		p.forgetLastStatus(ctx)
	}

	return done
}

// syncPinnedImageSet provisions the native backend for the images allocated to it. The caller
// guarantees the list is non-empty and within the CRD's `maxItems`, since `spec.pinnedImages`
// has a `minItems` of 1 and an over long set is rejected outright.
func (p *PinnedImageSetReconciler) syncPinnedImageSet(pinnedImages []string, ctx *chetypes.DeployContext) (bool, error) {
	// Re-checked here even though the absence of a pool already makes the backend unusable, and
	// so empties this list before the caller gets here. The two reads are a reconcile apart and
	// a pool can be deleted in between, and the object this guards against is the silent kind: a
	// set no pool selects is admitted, never applied, and never mentioned by any
	// MachineConfigNode, so nothing on the cluster would reveal that no node pre-pulls.
	machineConfigPool, err := infrastructure.FindMachineConfigPool(
		ctx.ClusterAPI.NonCachingClient,
		constants.MachineConfigPoolRole,
	)
	if err != nil {
		return false, fmt.Errorf("failed to find a MachineConfigPool for role %s: %w", constants.MachineConfigPoolRole, err)
	}

	if machineConfigPool == "" {
		if p.setLastStatus(ctx, "misconfigured: "+constants.MachineConfigPoolRole) {
			logger.Info(
				"No MachineConfigPool selects the worker role, so no node would pre-pull the images",
				"role", constants.MachineConfigPoolRole,
			)
		}

		return p.uninstallPinnedImageSet(ctx)
	}

	if err := deploy.AppendFinalizer(ctx, finalizerName); err != nil {
		return false, fmt.Errorf("failed to append finalizer %s: %w", finalizerName, err)
	}

	pinnedImageSet := newPinnedImageSet(ctx, pinnedImages)

	// A PinnedImageSet is cluster-scoped, so it cannot carry an owner reference
	// to the namespaced CheCluster. Clean up is driven by the finalizer instead.
	err = ctx.ClusterAPI.NonCachingClientWrapper.Sync(
		context.TODO(),
		pinnedImageSet,
		&k8s_client.SyncOptions{
			DiffOpts: diffs.PinnedImageSet,
		})
	if err != nil {
		return false, fmt.Errorf("failed to sync PinnedImageSet %s: %w", pinnedImageSet.GetName(), err)
	}

	// Reported from here rather than from Reconcile, so that the outcome of the set is only
	// evaluated on the path where a set actually exists. The paths that create nothing report
	// their own reason, and reading a status back for an absent set would overwrite it.
	p.logPinnedImageSetStatus(ctx)

	return true, nil
}

func (p *PinnedImageSetReconciler) uninstallPinnedImageSet(ctx *chetypes.DeployContext) (bool, error) {
	// The finalizer is this operator's record that it has a set to clean up: it is appended
	// before the set is created and released only once the set is gone. Without it there is
	// nothing of ours on the cluster, and the work below would be a discovery enumeration and
	// a cluster scoped delete on every single reconcile of every install that never opted in.
	//
	// That the delete would fail harmlessly is not enough. A failure here is a failure, so it
	// stops the reconciler chain, and everything registered after this point — the community
	// backend, container build, the console link, metrics — would stop with it on a cluster
	// that never asked for pinning. Missing RBAC after an upgrade is the realistic way in.
	//
	// The trade off is that a set whose finalizer was stripped by hand is no longer reclaimed.
	// No path in this operator produces that state.
	if !utils.Contains(ctx.CheCluster.Finalizers, finalizerName) {
		return true, nil
	}

	// Most supported OpenShift versions do not serve this API at all. Touching an object whose
	// CRD is absent fails with a no-matches error rather than a not-found one, which would stall
	// the reconcile chain and block CheCluster deletion, so the API has to be probed first —
	// and that covers the ownership read below as much as the delete itself.
	if infrastructure.IsPinnedImageSetAPIAvailable(ctx.ClusterAPI.DiscoveryClient) {
		// A set of this name belonging to another CheCluster is not ours to delete. The
		// finalizer is still released below, since holding it would leave this CheCluster
		// undeletable over an object it never owned.
		nameTaken, err := infrastructure.IsPinnedImageSetNameTaken(
			ctx.ClusterAPI.NonCachingClient,
			ctx.CheCluster.Namespace,
			ctx.CheCluster.Name,
		)
		if err != nil {
			return false, err
		}

		if !nameTaken {
			err := ctx.ClusterAPI.NonCachingClientWrapper.DeleteByKeyIgnoreNotFound(
				context.TODO(),
				types.NamespacedName{Name: getPinnedImageSetName(ctx)},
				&mcfgv1.PinnedImageSet{},
			)
			if err != nil {
				return false, fmt.Errorf("failed to delete PinnedImageSet %s: %w", getPinnedImageSetName(ctx), err)
			}
		}
	}

	// Removed unconditionally, so that a CheCluster is never left undeletable
	// on a cluster that stopped serving the API.
	if err := deploy.DeleteFinalizer(ctx, finalizerName); err != nil {
		return false, fmt.Errorf("failed to delete finalizer %s: %w", finalizerName, err)
	}

	return true, nil
}

func newPinnedImageSet(ctx *chetypes.DeployContext, images []string) *mcfgv1.PinnedImageSet {
	pinnedImages := make([]mcfgv1.PinnedImageRef, 0, len(images))
	for _, image := range images {
		pinnedImages = append(pinnedImages, mcfgv1.PinnedImageRef{Name: mcfgv1.ImageDigestFormat(image)})
	}

	labels := deploy.GetLabels(constants.PinnedImageSetComponentName)
	labels[constants.MachineConfigPoolRoleLabelKey] = constants.MachineConfigPoolRole
	labels[constants.CheClusterNamespaceLabelKey] = ctx.CheCluster.Namespace
	labels[constants.CheClusterNameLabelKey] = ctx.CheCluster.Name

	return &mcfgv1.PinnedImageSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: mcfgv1.GroupVersion.String(),
			Kind:       "PinnedImageSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   getPinnedImageSetName(ctx),
			Labels: labels,
		},
		Spec: mcfgv1.PinnedImageSetSpec{
			PinnedImages: pinnedImages,
		},
	}
}

// getPinnedImageSetName qualifies the name with the namespace, since the object is cluster-scoped
// and several CheClusters may exist on the same cluster.
func getPinnedImageSetName(ctx *chetypes.DeployContext) string {
	return infrastructure.PinnedImageSetName(ctx.CheCluster.Namespace, ctx.CheCluster.Name)
}
