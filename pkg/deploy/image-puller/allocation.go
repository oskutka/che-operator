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

package imagepuller

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/eclipse-che/che-operator/pkg/common/chetypes"
	"github.com/eclipse-che/che-operator/pkg/common/infrastructure"
)

// MaxPinnedImages is the `maxItems` the PinnedImageSet CRD enforces on `spec.pinnedImages`.
const MaxPinnedImages = 500

// maxPinnedImageReferenceLength is the `maxLength` the CRD enforces on a single entry.
const maxPinnedImageReferenceLength = 447

// The two expressions below are copied verbatim from the two CEL rules the API server applies
// to `spec.pinnedImages[*].name`, verified against the live CRD on OpenShift 4.22. Both matter
// equally: failing either one is rejected at admission for the *whole object*, so a single
// unacceptable entry means nothing in the set is pinned. Anything that would fail them has to
// be kept out of the set and handed to the community backend, which has no such restriction.
var (
	// digestReferenceReg is the first rule, applied to the part after the '@'.
	digestReferenceReg = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

	// imageNameReg is the second rule, applied to the part before the '@'. It is stricter
	// than a registry actually is, and rejects references that pull perfectly well:
	// a dotless host (`myregistry/image`, `localhost:5000/image`), and a path of more than
	// one namespace segment (`quay.io/a/b/image`). Both are ordinary in a mirrored or
	// internal registry, which is why this is checked rather than assumed.
	imageNameReg = regexp.MustCompile(`^([a-zA-Z0-9-]+\.)+[a-zA-Z0-9-]+(:[0-9]{2,5})?/([a-zA-Z0-9-_]{0,61}/)?[a-zA-Z0-9-_.]*?$`)
)

// ExclusionReason says why an image is pre-pulled by neither backend. It is the only part of
// the accounting that cannot be recovered from the cluster afterwards: the two backends record
// what they serve, but nothing records what nobody serves, and an admin-supplied image list
// means /tmp/external_images.txt no longer describes the input either. Hence the logging.
type ExclusionReason string

const (
	// ReasonPrePullDisabled covers every image when pre-pulling is switched off. It never
	// reaches the log, since the reconcilers do not report an allocation they did not make.
	ReasonPrePullDisabled ExclusionReason = "PrePullDisabled"

	// ReasonNoBackendAvailable means neither backend can run on this cluster.
	ReasonNoBackendAvailable ExclusionReason = "NoBackendAvailable"

	// ReasonNotPinnable means the image is not referenced by digest, so a PinnedImageSet
	// cannot carry it, and the community backend that could pre-pull it is not usable.
	ReasonNotPinnable ExclusionReason = "NotPinnableAndNoImagePuller"

	// ReasonPinnedImageSetFull means the image did not fit under MaxPinnedImages and there
	// was no community backend to fall back to.
	ReasonPinnedImageSetFull ExclusionReason = "PinnedImageSetFull"

	// ReasonUnsupportedReferenceShape means the image is referenced by digest but the
	// reference itself is one a PinnedImageSet refuses: a dotless registry host, a path with
	// more than one namespace segment, or a reference over 447 characters. The image pulls
	// normally, so this is a limitation of the native backend rather than a bad reference.
	ReasonUnsupportedReferenceShape ExclusionReason = "UnsupportedImageReferenceShape"

	// ReasonMalformedReference means the entry is not an image reference at all. Full
	// admission-shape validation (dotless hosts, multi-segment paths) belongs here too.
	ReasonMalformedReference ExclusionReason = "MalformedImageReference"
)

// Exclusion is an image nobody pre-pulls, and why.
type Exclusion struct {
	Image  string
	Reason ExclusionReason
}

// Allocation is the complete accounting of an image list across the two backends. Every input
// image lands in exactly one bucket, so that an image can never be dropped unnoticed: the
// buckets are a partition of the deduplicated input, and `Total` is the figure to compare
// against the input length.
type Allocation struct {
	// Pinned is served by a PinnedImageSet. Those images are exempt from kubelet garbage
	// collection unconditionally, at no runtime cost.
	Pinned []string

	// Prepulled is served by the community Kubernetes Image Puller. Those images are also
	// safe from garbage collection, but only as a side effect: the DaemonSet holds each one
	// with a running `sleep` container, and kubelet will not reclaim an image that is in use.
	// So the protection costs a container per image per node and lapses whenever the pod
	// does. Verified on a live cluster 2026-10-05 — do not restate this as "pinned images
	// survive GC and pre-pulled ones do not", which is the obvious guess and is wrong.
	Prepulled []string

	// Excluded is pre-pulled by nobody and paid for on first workspace start.
	Excluded []Exclusion
}

// Total is the number of images accounted for, which must equal the deduplicated input length.
func (a Allocation) Total() int {
	return len(a.Pinned) + len(a.Prepulled) + len(a.Excluded)
}

// HasNoUsableBackend reports whether pre-pulling was asked for and no backend at all can serve
// it, which is the only condition that still deserves the long standing "the Kubernetes Image
// Puller is not installed" error.
//
// Deliberately narrower than "some image went unserved". An image the native backend cannot
// carry is a partial result, not a failure: the digest referenced images around it are still
// pinned. Failing the reconcile for it would be worse than the gap it reports, because the
// reconciler chain stops at the first reconciler that is not done, and the PinnedImageSet
// reconciler runs in that same chain — so one tag in the list would cost every other image its
// pinning. A released list carries tag references today, so that is the default path, not an
// edge case. The partial cases are reported through the allocation log instead.
func (a Allocation) HasNoUsableBackend() bool {
	for _, exclusion := range a.Excluded {
		if exclusion.Reason == ReasonNoBackendAvailable {
			return true
		}
	}

	return false
}

// Capabilities is what the cluster and the CheCluster allow, resolved once so that allocation
// itself stays a pure function of its inputs and can be tested without a cluster.
type Capabilities struct {
	// Enabled mirrors `spec.components.imagePuller.enable`.
	Enabled bool

	// PinnedImageSetRequested mirrors `spec.components.imagePuller.enablePinnedImageSet`.
	// Pinning is opt in: a PinnedImageSet is cluster-scoped and all-or-nothing, so a cluster
	// that never asked for one must never get one, however capable it is.
	PinnedImageSetRequested bool

	// PinnedImageSetUsable is true when the cluster can actually reconcile a PinnedImageSet,
	// which needs both the API and the machine-config-daemon.
	PinnedImageSetUsable bool

	// ImagePullerUsable is true when the community Kubernetes Image Puller Operator is installed.
	ImagePullerUsable bool
}

// GetCapabilities resolves what this cluster and this CheCluster allow.
//
// What the cluster allows is resolved at most once per reconcile pass and memoized on the
// DeployContext. Each answer costs a full discovery enumeration, and this is called from both
// reconcilers and more than once within each, so without the memo an opted out cluster spent
// most of its pre-pull reconciling asking the API server questions whose answer it already had.
// What the CheCluster asks for is read every time: it is two field reads, and memoizing it
// would only risk serving a stale spec.
func GetCapabilities(ctx *chetypes.DeployContext) Capabilities {
	requested := ctx.CheCluster.Spec.Components.ImagePuller.EnablePinnedImageSet

	if ctx.PrePullBackends == nil {
		backends := &chetypes.PrePullBackends{
			PinnedImageSetUsable: infrastructure.IsPinnedImageSetEnabled(
				ctx.ClusterAPI.DiscoveryClient,
				ctx.ClusterAPI.NonCachingClient,
			),
			ImagePullerUsable: infrastructure.IsKubernetesImagePullerEnabled(ctx.ClusterAPI.DiscoveryClient),
		}

		// Whether another CheCluster already owns the name is asked only where pinning was
		// asked for and could otherwise happen: it costs a read, and a cluster that never
		// opted in would be paying it on every pass to learn something it cannot use.
		//
		// It belongs here rather than at the point the set is written, because it decides
		// which backend serves an image. Answered later, the digest referenced images would
		// already have been allocated to a set that is then not created, and the community
		// backend — which allocates from the same capabilities — would not take them either,
		// leaving them pre-pulled by nobody.
		if backends.PinnedImageSetUsable && requested {
			nameTaken, err := infrastructure.IsPinnedImageSetNameTaken(
				ctx.ClusterAPI.NonCachingClient,
				ctx.CheCluster.Namespace,
				ctx.CheCluster.Name,
			)
			if err != nil {
				// Unusable on a failed read, as everywhere else here. Pre-pulling then falls
				// to the community backend, which serves every image rather than none.
				logger.Error(err, "Failed to establish who owns the PinnedImageSet name, considering pinning unusable")
			}

			backends.PinnedImageSetNameTaken = nameTaken || err != nil
			backends.PinnedImageSetUsable = !backends.PinnedImageSetNameTaken
		}

		ctx.PrePullBackends = backends
	}

	return Capabilities{
		Enabled:                 ctx.CheCluster.Spec.Components.ImagePuller.Enable,
		PinnedImageSetRequested: requested,
		PinnedImageSetUsable:    ctx.PrePullBackends.PinnedImageSetUsable,
		ImagePullerUsable:       ctx.PrePullBackends.ImagePullerUsable,
	}
}

// backends reports which backends may be used. Pinning needs both an administrator who asked
// for it and a cluster that can do it; the community backend needs only to be installed.
//
// The two are independent rather than exclusive, which is what lets an opted in cluster pin
// the digest referenced images and still pre-pull the rest. Turning pinning on therefore never
// takes pre-pulling away from an image, and turning it off is exactly the behaviour of every
// release before it existed.
func (c Capabilities) backends() (canPin bool, canPrepull bool) {
	return c.PinnedImageSetRequested && c.PinnedImageSetUsable, c.ImagePullerUsable
}

// ResolveImages returns the list the backends have to serve. An admin-supplied
// `spec.components.imagePuller.spec.images` replaces the computed list wholesale rather than
// adding to it, so it is the input in its own right. Merging the two instead would silently
// re-add our images for every admin who deliberately narrowed theirs.
func ResolveImages(ctx *chetypes.DeployContext, externalImages []string) []string {
	if images := strings.TrimSpace(ctx.CheCluster.Spec.Components.ImagePuller.Spec.Images); images != "" {
		return ImagesFromSpecField(images)
	}

	return externalImages
}

// ImagesToPrePull returns the list the backends have to serve, and whether it is known at all.
//
// Both reconcilers call this rather than the provider directly, because the escape hatch below
// is a policy about pre-pulling as a whole: a cluster where one backend carries on past a failed
// fetch while the other fails the reconcile is a cluster where the escape hatch does nothing,
// since the chain stops at the first reconciler that is not done either way.
//
// `known` is false when the list could not be determined and the administrator asked for that
// not to block the rest of the reconcile. The distinction from an empty list matters and is the
// whole reason this returns three values: a backend withdraws when its share of the list empties,
// so treating "we could not find out" as "there is nothing to pre-pull" would tear both backends
// down over a transient dashboard error. An unknown list means leave the cluster as it is.
func ImagesToPrePull(ctx *chetypes.DeployContext, provider ImagesProvider) ([]string, bool, error) {
	// Resolved at most once per reconcile pass. Both reconcilers call this, each with its own
	// provider, and each provider fetches afresh — so without the memo the two would allocate
	// from two different dashboard responses and an image that moved between them would be
	// served by neither backend, or by both. Sharing a provider instance would not have been
	// enough; the result is what has to be shared.
	if ctx.PrePullImages != nil {
		return ctx.PrePullImages.Images, ctx.PrePullImages.Known, nil
	}

	// Switched off needs no list, and must not wait for one. The provider reads the dashboard
	// over HTTP, so fetching first lets an unreachable dashboard fail the reconcile of a
	// feature nobody enabled — and the work that reconcile would have done is precisely the
	// teardown of whatever the feature left behind. An administrator turning pre-pulling off
	// while the dashboard is down would be left with the objects they asked to be rid of.
	//
	// The empty list is deliberately `known`. There genuinely is nothing to pre-pull, which is
	// not the same as the fetch having failed, and the two reconcilers have to be able to tell
	// the difference: an unknown list leaves the cluster alone, an empty one withdraws.
	if !ctx.CheCluster.Spec.Components.ImagePuller.Enable {
		return memoizeImagesToPrePull(ctx, nil, true)
	}

	externalImages, err := provider.Get(ctx)
	if err != nil {
		// Previously, failures to get external images didn't block reconciliation.
		// Actually it should not happen, because all requests go to dashboard service (not external url).
		// Added a workaround env var to ignore failures in case of any possible issues.
		if os.Getenv("CHE_OPERATOR__GET_EXTERNAL_IMAGES__IGNORE_FAILURES") != "true" {
			return nil, false, fmt.Errorf("failed to get external images: %w", err)
		}

		logger.Error(err, "Failed to get external images")

		// An administrator supplied list does not come from the fetch that just failed, so it
		// is still the whole truth and pre-pulling can proceed normally.
		if strings.TrimSpace(ctx.CheCluster.Spec.Components.ImagePuller.Spec.Images) == "" {
			return memoizeImagesToPrePull(ctx, nil, false)
		}
	}

	return memoizeImagesToPrePull(ctx, ResolveImages(ctx, externalImages), true)
}

// memoizeImagesToPrePull records the resolved list on the reconcile pass and returns it.
//
// A failed fetch is deliberately not recorded. It stops the chain where it happens, so there
// is no second reader to keep consistent, and caching it would turn one transient dashboard
// error into a pass that cannot recover.
func memoizeImagesToPrePull(ctx *chetypes.DeployContext, images []string, known bool) ([]string, bool, error) {
	ctx.PrePullImages = &chetypes.PrePullImages{Images: images, Known: known}

	return images, known, nil
}

// Allocate splits images across the two backends. Duplicates are collapsed, keeping the first
// occurrence, and the order of the input is preserved within each bucket.
func Allocate(images []string, capabilities Capabilities) Allocation {
	allocation := Allocation{
		Pinned:    make([]string, 0, len(images)),
		Prepulled: make([]string, 0, len(images)),
		Excluded:  make([]Exclusion, 0),
	}

	canPin, canPrepull := capabilities.backends()
	seen := make(map[string]bool, len(images))

	for _, image := range images {
		// An empty entry is not an image that failed to be pre-pulled, so it is dropped
		// before the accounting starts rather than counted as excluded.
		image = strings.TrimSpace(image)
		if image == "" || seen[image] {
			continue
		}

		seen[image] = true

		switch {
		case !capabilities.Enabled:
			allocation.exclude(image, ReasonPrePullDisabled)

		case !isImageReference(image):
			allocation.exclude(image, ReasonMalformedReference)

		case canPin && IsPinnable(image) && len(allocation.Pinned) < MaxPinnedImages:
			allocation.Pinned = append(allocation.Pinned, image)

		case canPrepull:
			// Everything the native backend cannot take, including digest images beyond the
			// cap, still gets pre-pulled if the community backend is there to take it.
			allocation.Prepulled = append(allocation.Prepulled, image)

		default:
			allocation.exclude(image, reasonFor(image, canPin, len(allocation.Pinned)))
		}
	}

	return allocation
}

func (a *Allocation) exclude(image string, reason ExclusionReason) {
	a.Excluded = append(a.Excluded, Exclusion{Image: image, Reason: reason})
}

// reasonFor distinguishes the ways an image ends up served by nobody, so that the log says
// which of them it was. They need different fixes: install the community operator, shorten
// the list, or supply a digest.
func reasonFor(image string, canPin bool, pinnedCount int) ExclusionReason {
	if !canPin {
		return ReasonNoBackendAvailable
	}

	if IsPinnable(image) && pinnedCount >= MaxPinnedImages {
		return ReasonPinnedImageSetFull
	}

	// A digest reference that still cannot be pinned is the confusing case, since the one
	// thing an administrator knows to do about pinning is supply a digest. Saying so keeps
	// the log from looking like the operator ignored a perfectly good reference.
	if IsReferencedByDigest(image) {
		return ReasonUnsupportedReferenceShape
	}

	return ReasonNotPinnable
}

// IsPinnable reports whether a PinnedImageSet can carry the image. Being referenced by digest
// is necessary but not sufficient: the reference also has to satisfy the shape rule and the
// length limit, and a set containing one entry that does not is rejected in its entirety.
func IsPinnable(image string) bool {
	if len(image) > maxPinnedImageReferenceLength {
		return false
	}

	parts := strings.Split(image, "@")
	if len(parts) != 2 {
		return false
	}

	return digestReferenceReg.MatchString(parts[1]) && imageNameReg.MatchString(parts[0])
}

// IsReferencedByDigest reports whether the image is referenced by digest, which is a weaker
// question than IsPinnable. It is what separates "this image could never be pinned" from
// "this image could have been pinned but did not fit", so that the two are not reported alike.
func IsReferencedByDigest(image string) bool {
	parts := strings.Split(image, "@")
	if len(parts) != 2 {
		return false
	}

	return digestReferenceReg.MatchString(parts[1])
}

// isImageReference rejects entries that are not image references at all. It is deliberately
// permissive: the stricter admission-shape rules the PinnedImageSet CRD applies are a separate
// piece of work, and rejecting an image here means not pre-pulling it, so the cost of being
// wrong is higher than the cost of letting the API server decide.
func isImageReference(image string) bool {
	if strings.ContainsAny(image, " \t\n") {
		return false
	}

	return !strings.HasPrefix(image, "@") && !strings.HasPrefix(image, ":") && !strings.HasPrefix(image, "/")
}
