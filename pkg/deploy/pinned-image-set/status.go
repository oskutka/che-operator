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
	"sort"
	"strings"

	"github.com/eclipse-che/che-operator/pkg/common/chetypes"
	imagepuller "github.com/eclipse-che/che-operator/pkg/deploy/image-puller"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
)

// A PinnedImageSet carries no status of its own, so the outcome has to be read back from the
// Machine Config Operator. Pinning is all-or-nothing per set: the machine-config-daemon inspects
// every image in the set before pulling any of them, so a single unreachable or unauthenticated
// image leaves nothing pinned at all. Resolving that is the administrator's responsibility, and
// the failure is otherwise invisible from Eclipse Che, hence the logging.
//
// The outcome is read from MachineConfigNode rather than MachineConfigPool. The pool does define a
// `PinnedImageSetsDegraded` condition, but it reports only the controller's failure to populate
// `spec.pinnedImageSets`, not the prefetch itself, and it stays False while every node in the pool
// fails to pull. Verified against OpenShift 4.22: with all three workers reporting
// `PinnedImageSetsDegraded=True/PrefetchFailed`, the pool stayed False indefinitely. The
// per node `lastFailedGenerationError` is therefore the only usable signal. It is scoped to this
// set, and the Machine Config Operator clears it once the set reconciles, so no staleness guard
// is needed.
//
// The degradation is deliberately not turned into a reconcile error. Pre-pulling is an
// optimization, workspaces still start without it, and the failure never degrades the pool, so it
// does not block MachineConfig rollouts either.
func (p *PinnedImageSetReconciler) logPinnedImageSetStatus(ctx *chetypes.DeployContext) {
	message, err := p.getDegradedMessage(ctx)

	// Everything below is logged only on change, since the reconcile loop runs far more often than
	// the status changes. That applies to the failures too: a cluster that never serves
	// MachineConfigNode would otherwise log on every single pass.
	if err != nil {
		// Reporting must never interfere with reconciling, so this is logged rather than returned.
		if p.setLastStatus(ctx, "error: "+err.Error()) {
			logger.Error(err, "Failed to evaluate the PinnedImageSet status", "pinnedImageSet", getPinnedImageSetName(ctx))
		}

		return
	}

	previous := p.lastStatus[reportingKey(ctx)]

	if !p.setLastStatus(ctx, degradedStatusPrefix+message) {
		return
	}

	if message == "" {
		// Only a degradation that was actually reported is worth reporting the end of. The
		// recorded status starts empty and healthy records as degradedStatusPrefix with
		// nothing after it, so without this the first healthy sync of a brand new set
		// announces a recovery from a state nobody was ever told about — on a fresh install,
		// the one line in the log that looks like something went wrong.
		//
		// A recovery from "error: " or "misconfigured: " is deliberately silent too: those
		// are not degradations, they were logged when they happened, and this line would
		// misdescribe them.
		if strings.HasPrefix(previous, degradedStatusPrefix) {
			logger.Info("PinnedImageSet is no longer degraded", "pinnedImageSet", getPinnedImageSetName(ctx))
		}

		return
	}

	logger.Info(
		"PinnedImageSet is degraded, no image from the set is being pre-pulled. "+
			"Every image must be pullable by every node in the pool, resolving this is up to the administrator",
		"pinnedImageSet", getPinnedImageSetName(ctx),
		"details", message,
	)
}

// logUnsatisfiableBackendRequest explains why a cluster that opted into pinning is not pinning.
// Without it the operator would quietly install the community backend instead, which looks
// identical to never having opted in — and since pinning is off by default, nothing on the
// cluster distinguishes the two.
func (p *PinnedImageSetReconciler) logUnsatisfiableBackendRequest(ctx *chetypes.DeployContext) {
	if !imagepuller.IsPinnedImageSetBackendRequestUnsatisfiable(ctx) {
		return
	}

	// The two reasons need different remedies, so they are reported apart. A name already
	// owned is fixed by renaming a CheCluster; nothing about the cluster is wrong, and saying
	// it is would send an administrator looking at the machine-config-daemon.
	if ctx.PrePullBackends != nil && ctx.PrePullBackends.PinnedImageSetNameTaken {
		if p.setLastStatus(ctx, "nameTaken") {
			logger.Info(
				"enablePinnedImageSet is set but another CheCluster already owns a PinnedImageSet of "+
					"this name, so the Kubernetes Image Puller pre-pulls these images instead. The name "+
					"joins the namespace and the name with a dash and two CheClusters can produce the "+
					"same one; renaming either resolves it",
				"pinnedImageSet", getPinnedImageSetName(ctx),
			)
		}

		return
	}

	if p.setLastStatus(ctx, "unsatisfiable") {
		logger.Info(
			"enablePinnedImageSet is set but this cluster cannot reconcile a PinnedImageSet, " +
				"so the Kubernetes Image Puller is used instead. The cluster must serve the " +
				"PinnedImageSet API and run the machine-config-daemon on its nodes",
		)
	}
}

// degradedStatusPrefix marks the recorded status as describing the degradation of a set that
// exists, which is what separates it from the misconfigured, unsatisfiable and error states
// sharing the same field.
const degradedStatusPrefix = "degraded: "

// maxReportedExclusions caps the per image lines, so that a misconfiguration affecting a long
// list cannot flood the log. The summary line always carries the true count.
const maxReportedExclusions = 10

// logAllocation reports how the image list was split between the two backends. It is the only
// record of the third bucket: the two backends persist what they serve, in `spec.pinnedImages`
// and `spec.images` respectively, but nothing on the cluster records what neither of them took,
// and `/tmp/external_images.txt` stops describing the input as soon as an administrator sets
// their own list. Without this line "no image was pre-pulled" and "there was nothing to
// pre-pull" are indistinguishable from outside the operator.
//
// It is owned by this reconciler rather than the community one, because this one runs on every
// cluster and reports before deciding whether to provision anything. Leaving it to whichever
// backend installs itself would mean two reconcilers reporting totals that disagree while a
// change rolls out, and no total at all where nothing is pre-pulled.
func (p *PinnedImageSetReconciler) logAllocation(
	ctx *chetypes.DeployContext,
	allocation imagepuller.Allocation,
	capabilities imagepuller.Capabilities,
) {
	// Pre-pulling that was never asked for has no allocation to report.
	if !capabilities.Enabled {
		return
	}

	// Logged only on change, since the reconcile loop runs far more often than the list does.
	// Keyed on the counts rather than on the images, so that a digest changing under an
	// unchanged tag does not produce an identical looking line.
	summary := fmt.Sprintf("%d/%d/%d/%d",
		allocation.Total(), len(allocation.Pinned), len(allocation.Prepulled), len(allocation.Excluded))

	key := reportingKey(ctx)
	if summary == p.lastAllocation[key] {
		return
	}

	if p.lastAllocation == nil {
		p.lastAllocation = map[string]string{}
	}

	p.lastAllocation[key] = summary

	// Named, because on a cluster with more than one CheCluster the counts alone say nothing
	// about whose images they are.
	logger.Info(
		"Image pre-pull allocation",
		"cheCluster", key,
		"total", allocation.Total(),
		"pinned", len(allocation.Pinned),
		"prepulled", len(allocation.Prepulled),
		"excluded", len(allocation.Excluded),
	)

	// The counts say how bad it is, the reasons say what to do about it, and the reason is
	// precisely the part that cannot be recovered from the cluster afterwards.
	for i, exclusion := range allocation.Excluded {
		if i == maxReportedExclusions {
			logger.Info(
				"More images are not pre-pulled than are reported here",
				"reported", maxReportedExclusions,
				"omitted", len(allocation.Excluded)-maxReportedExclusions,
			)

			break
		}

		logger.Info(
			"Image is not pre-pulled by any backend and will be pulled when a workspace first needs it",
			"cheCluster", key,
			"image", exclusion.Image,
			"reason", string(exclusion.Reason),
		)
	}
}

// setLastStatus records the status for this CheCluster and reports whether it differs from the
// one previously recorded for the same CheCluster.
func (p *PinnedImageSetReconciler) setLastStatus(ctx *chetypes.DeployContext, status string) bool {
	key := reportingKey(ctx)
	if status == p.lastStatus[key] {
		return false
	}

	if p.lastStatus == nil {
		p.lastStatus = map[string]string{}
	}

	p.lastStatus[key] = status

	return true
}

// forgetLastStatus drops what was recorded for a CheCluster that is going away, so that the two
// maps cannot outlive the objects they describe and a CheCluster recreated under the same name
// starts reporting from scratch rather than inheriting the previous one's last known state.
func (p *PinnedImageSetReconciler) forgetLastStatus(ctx *chetypes.DeployContext) {
	key := reportingKey(ctx)

	delete(p.lastStatus, key)
	delete(p.lastAllocation, key)
}

// getDegradedMessage returns a human readable description of why the set is not being applied,
// or an empty string if it is healthy.
func (p *PinnedImageSetReconciler) getDegradedMessage(ctx *chetypes.DeployContext) (string, error) {
	nodeErrors, err := getNodeErrors(ctx)
	if err != nil {
		return "", err
	}

	return strings.Join(nodeErrors, "; "), nil
}

// getNodeErrors returns the per node errors reported for the set, one entry per failing node.
// A node that never saw the set, or that reconciled it successfully, reports no error for it.
func getNodeErrors(ctx *chetypes.DeployContext) ([]string, error) {
	machineConfigNodes := &mcfgv1.MachineConfigNodeList{}
	if err := ctx.ClusterAPI.NonCachingClient.List(context.TODO(), machineConfigNodes); err != nil {
		return nil, fmt.Errorf("failed to list machineconfignodes: %w", err)
	}

	pinnedImageSetName := getPinnedImageSetName(ctx)
	nodeErrors := make([]string, 0)

	for _, machineConfigNode := range machineConfigNodes.Items {
		for _, pinnedImageSet := range machineConfigNode.Status.PinnedImageSets {
			if pinnedImageSet.Name == pinnedImageSetName && pinnedImageSet.LastFailedGenerationError != "" {
				nodeErrors = append(nodeErrors, fmt.Sprintf("node %s: %s", machineConfigNode.Name, pinnedImageSet.LastFailedGenerationError))
			}
		}
	}

	sort.Strings(nodeErrors)

	return nodeErrors, nil
}
