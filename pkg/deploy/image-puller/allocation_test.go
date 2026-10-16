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
	"math/rand"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/assert"
)

const (
	digest1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digest2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	pinnable1 = "quay.io/che/image-1@" + digest1
	pinnable2 = "quay.io/che/image-2@" + digest2
	tagged1   = "quay.io/che/image-3:next"
	tagged2   = "quay.io/che/image-4:latest"
	malformed = "quay.io/che/image 5:latest"
)

// mixedInput deliberately contains every class the allocator distinguishes, so that one input
// exercises all three buckets whatever the capabilities are.
var mixedInput = []string{pinnable1, tagged1, pinnable2, tagged2, malformed}

// bothBackends and pinningOnly are the two capability sets most tests want: an opted in cluster
// that can pin, with and without the community operator installed. Spelled out once, because
// every field has to be set explicitly now that an unset one means "pinning was not asked for".
var (
	bothBackends = Capabilities{
		Enabled:                 true,
		PinnedImageSetRequested: true,
		PinnedImageSetUsable:    true,
		ImagePullerUsable:       true,
	}

	pinningOnly = Capabilities{
		Enabled:                 true,
		PinnedImageSetRequested: true,
		PinnedImageSetUsable:    true,
	}
)

func allCapabilities() []Capabilities {
	capabilities := make([]Capabilities, 0, 16)

	for _, enabled := range []bool{true, false} {
		for _, pinnedImageSetRequested := range []bool{true, false} {
			for _, pinnedImageSetUsable := range []bool{true, false} {
				for _, imagePullerUsable := range []bool{true, false} {
					capabilities = append(capabilities, Capabilities{
						Enabled:                 enabled,
						PinnedImageSetRequested: pinnedImageSetRequested,
						PinnedImageSetUsable:    pinnedImageSetUsable,
						ImagePullerUsable:       imagePullerUsable,
					})
				}
			}
		}
	}

	return capabilities
}

func (c Capabilities) String() string {
	return fmt.Sprintf("enabled=%t/pinningRequested=%t/pinnedImageSet=%t/imagePuller=%t",
		c.Enabled, c.PinnedImageSetRequested, c.PinnedImageSetUsable, c.ImagePullerUsable)
}

// TestAllocateOverTheCapabilityMatrix enumerates every combination of the inputs that decide a
// backend rather than listing the interesting ones, so that adding a capability forces every
// expectation here to be revisited instead of silently inheriting whatever the default is.
func TestAllocateOverTheCapabilityMatrix(t *testing.T) {
	type expectation struct {
		pinned    []string
		prepulled []string
		excluded  map[string]ExclusionReason
	}

	// expected is written as a decision table rather than 24 literal cases, because the
	// literal form hides which capability actually drives the outcome.
	expected := func(c Capabilities) expectation {
		if !c.Enabled {
			return expectation{excluded: map[string]ExclusionReason{
				pinnable1: ReasonPrePullDisabled,
				pinnable2: ReasonPrePullDisabled,
				tagged1:   ReasonPrePullDisabled,
				tagged2:   ReasonPrePullDisabled,
				malformed: ReasonPrePullDisabled,
			}}
		}

		canPin, canPrepull := c.backends()

		e := expectation{excluded: map[string]ExclusionReason{malformed: ReasonMalformedReference}}

		for _, image := range []string{pinnable1, pinnable2} {
			switch {
			case canPin:
				e.pinned = append(e.pinned, image)
			case canPrepull:
				e.prepulled = append(e.prepulled, image)
			default:
				e.excluded[image] = ReasonNoBackendAvailable
			}
		}

		for _, image := range []string{tagged1, tagged2} {
			switch {
			case canPrepull:
				e.prepulled = append(e.prepulled, image)
			case canPin:
				e.excluded[image] = ReasonNotPinnable
			default:
				e.excluded[image] = ReasonNoBackendAvailable
			}
		}

		return e
	}

	for _, capabilities := range allCapabilities() {
		t.Run(capabilities.String(), func(t *testing.T) {
			actual := Allocate(mixedInput, capabilities)
			want := expected(capabilities)

			assert.ElementsMatch(t, want.pinned, actual.Pinned)
			assert.ElementsMatch(t, want.prepulled, actual.Prepulled)

			actualExcluded := make(map[string]ExclusionReason, len(actual.Excluded))
			for _, exclusion := range actual.Excluded {
				actualExcluded[exclusion.Image] = exclusion.Reason
			}

			assert.Equal(t, want.excluded, actualExcluded)
		})
	}
}

// TestAllocationIsAPartitionOfItsInput is the conservation law. Every image goes somewhere and
// nothing goes to two places, over randomly generated input. It is what stops a future
// "and skip these as well" edit from dropping images out of the accounting unnoticed, which is
// precisely how the non-digest remainder used to disappear.
func TestAllocationIsAPartitionOfItsInput(t *testing.T) {
	capabilities := allCapabilities()

	property := func(seed int64, imageCount uint8) bool {
		random := rand.New(rand.NewSource(seed))
		images := randomImages(random, int(imageCount))
		c := capabilities[random.Intn(len(capabilities))]

		allocation := Allocate(images, c)

		deduplicated := make(map[string]bool, len(images))
		for _, image := range images {
			if image != "" {
				deduplicated[image] = true
			}
		}

		// Conservation: the buckets account for the deduplicated input exactly.
		if allocation.Total() != len(deduplicated) {
			t.Logf("total %d != %d deduplicated inputs, capabilities %s", allocation.Total(), len(deduplicated), c)
			return false
		}

		// Disjointness and completeness: every image appears in exactly one bucket.
		placements := make(map[string]int, len(deduplicated))
		for _, image := range allocation.Pinned {
			placements[image]++
		}

		for _, image := range allocation.Prepulled {
			placements[image]++
		}

		for _, exclusion := range allocation.Excluded {
			placements[exclusion.Image]++
		}

		for image := range deduplicated {
			if placements[image] != 1 {
				t.Logf("image %s appears %d times, capabilities %s", image, placements[image], c)
				return false
			}
		}

		return len(placements) == len(deduplicated)
	}

	assert.NoError(t, quick.Check(property, &quick.Config{MaxCount: 500}))
}

// randomImages builds a list mixing every reference class, including duplicates and blanks.
func randomImages(random *rand.Rand, count int) []string {
	images := make([]string, 0, count)

	for i := 0; i < count; i++ {
		switch random.Intn(5) {
		case 0:
			images = append(images, fmt.Sprintf("quay.io/che/image-%d@%s", random.Intn(count+1), digest1))
		case 1:
			images = append(images, fmt.Sprintf("quay.io/che/image-%d:tag", random.Intn(count+1)))
		case 2:
			images = append(images, fmt.Sprintf("quay.io/che/image %d:tag", random.Intn(count+1)))
		case 3:
			images = append(images, "")
		default:
			images = append(images, pinnable1)
		}
	}

	return images
}

// TestAllocateAtTheCapBoundary covers the PinnedImageSet `maxItems`. Overflowing images are
// handed to the community backend rather than dropped, so an admin with a long list loses
// garbage-collection exemption for the tail but still gets it pre-pulled.
func TestAllocateAtTheCapBoundary(t *testing.T) {
	for _, count := range []int{MaxPinnedImages - 1, MaxPinnedImages, MaxPinnedImages + 1} {
		t.Run(fmt.Sprintf("%d images", count), func(t *testing.T) {
			images := make([]string, 0, count)
			for i := 0; i < count; i++ {
				images = append(images, fmt.Sprintf("quay.io/che/image-%d@%s", i, digest1))
			}

			overflow := max(count-MaxPinnedImages, 0)

			withBoth := Allocate(images, bothBackends)
			assert.Len(t, withBoth.Pinned, count-overflow)
			assert.Len(t, withBoth.Prepulled, overflow)
			assert.Empty(t, withBoth.Excluded)

			// Without the community backend there is nowhere for the overflow to go, and it
			// has to be reported rather than silently trimmed to fit.
			pinnedOnly := Allocate(images, pinningOnly)
			assert.Len(t, pinnedOnly.Pinned, count-overflow)
			assert.Len(t, pinnedOnly.Excluded, overflow)

			for _, exclusion := range pinnedOnly.Excluded {
				assert.Equal(t, ReasonPinnedImageSetFull, exclusion.Reason)
			}
		})
	}
}

func TestAllocateDegenerateInputs(t *testing.T) {
	capabilities := bothBackends

	t.Run("No images at all", func(t *testing.T) {
		allocation := Allocate(nil, capabilities)
		assert.Equal(t, 0, allocation.Total())
		assert.Empty(t, allocation.Pinned)
		assert.Empty(t, allocation.Prepulled)
		assert.Empty(t, allocation.Excluded)
	})

	t.Run("Blank entries are not counted as excluded", func(t *testing.T) {
		allocation := Allocate([]string{"", "   ", pinnable1}, capabilities)
		assert.Equal(t, 1, allocation.Total())
		assert.Equal(t, []string{pinnable1}, allocation.Pinned)
	})

	t.Run("Duplicates are collapsed, keeping the first occurrence", func(t *testing.T) {
		allocation := Allocate([]string{pinnable1, tagged1, pinnable1, tagged1}, capabilities)
		assert.Equal(t, []string{pinnable1}, allocation.Pinned)
		assert.Equal(t, []string{tagged1}, allocation.Prepulled)
	})

	t.Run("Surrounding whitespace is not a different image", func(t *testing.T) {
		allocation := Allocate([]string{pinnable1, "  " + pinnable1 + "  "}, capabilities)
		assert.Equal(t, 1, allocation.Total())
	})

	t.Run("Input order is preserved within a bucket", func(t *testing.T) {
		allocation := Allocate([]string{pinnable2, tagged2, pinnable1, tagged1}, capabilities)
		assert.Equal(t, []string{pinnable2, pinnable1}, allocation.Pinned)
		assert.Equal(t, []string{tagged2, tagged1}, allocation.Prepulled)
	})

	t.Run("Every image is digest referenced", func(t *testing.T) {
		allocation := Allocate([]string{pinnable1, pinnable2}, capabilities)
		assert.Len(t, allocation.Pinned, 2)
		assert.Empty(t, allocation.Prepulled)
	})

	t.Run("No image is digest referenced", func(t *testing.T) {
		allocation := Allocate([]string{tagged1, tagged2}, capabilities)
		assert.Empty(t, allocation.Pinned)
		assert.Len(t, allocation.Prepulled, 2)
	})
}

// TestIsReferencedByDigest guards the one rule that decides which backend an image can use.
func TestIsReferencedByDigest(t *testing.T) {
	assert.True(t, IsReferencedByDigest(pinnable1))
	assert.False(t, IsReferencedByDigest("quay.io/che/image-1:latest"))
	assert.False(t, IsReferencedByDigest("quay.io/che/image-1"))
	assert.False(t, IsReferencedByDigest("quay.io/che/image-1@sha256:abc"))
	assert.False(t, IsReferencedByDigest("quay.io/che/image-1@md5:"+digest1[7:]))
	assert.False(t, IsReferencedByDigest("quay.io/che/image-1@"+digest1+"@"+digest2))
}

// TestIsPinnable is a transcription of the live CRD. Every case below was run against the
// PinnedImageSet admission on OpenShift 4.22 (cluster-bot-2026-10-02-091058, server-side dry
// run) and this function has to agree with it, because disagreeing in the permissive direction
// does not cost one image: a single rejected entry takes the whole set down, so every other
// image stops being pinned too.
func TestIsPinnable(t *testing.T) {
	testCases := []struct {
		name     string
		image    string
		pinnable bool
	}{
		{
			name:     "A plain digest reference, which is what a released install consists of",
			image:    "quay.io/che/image-1@" + digest1,
			pinnable: true,
		},
		{
			name:     "Underscores and dots in the name are accepted",
			image:    "quay.io/che/image_1.x@" + digest1,
			pinnable: true,
		},
		{
			name:     "One namespace segment is the most the shape rule allows",
			image:    "registry.redhat.io/devspaces/udi-rhel9@" + digest1,
			pinnable: true,
		},
		{
			name:     "A deeper path is rejected, though it pulls perfectly well",
			image:    "quay.io/a/b/image-1@" + digest1,
			pinnable: false,
		},
		{
			name:     "A dotless host is rejected, which rules out an unqualified internal registry",
			image:    "myregistry/image-1@" + digest1,
			pinnable: false,
		},
		{
			name:     "localhost with a port is rejected too, since the host still has no dot",
			image:    "localhost:5000/che/image-1@" + digest1,
			pinnable: false,
		},
		{
			name:     "A tag is not a digest",
			image:    "quay.io/che/image-1:latest",
			pinnable: false,
		},
		{
			name:     "A reference over the 447 character limit",
			image:    "quay.io/che/" + strings.Repeat("a", 448-len("quay.io/che/@"+digest1)+1) + "@" + digest1,
			pinnable: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.pinnable, IsPinnable(testCase.image))
		})
	}
}

// TestUnpinnableDigestsGoToTheImagePuller is the consequence that matters. An image the native
// backend refuses on shape grounds is still perfectly pullable, so the community backend takes
// it, and — crucially — it never reaches the set it would have invalidated.
func TestUnpinnableDigestsGoToTheImagePuller(t *testing.T) {
	deepPath := "quay.io/a/b/image-1@" + digest1

	allocation := Allocate(
		[]string{pinnable1, deepPath},
		bothBackends,
	)

	assert.Equal(t, []string{pinnable1}, allocation.Pinned)
	assert.Equal(t, []string{deepPath}, allocation.Prepulled)
	assert.Empty(t, allocation.Excluded)

	// With nowhere to put it, it is excluded under its own reason rather than under the one
	// that tells an administrator to supply a digest they have already supplied.
	allocation = Allocate(
		[]string{pinnable1, deepPath},
		pinningOnly,
	)

	assert.Equal(t, []string{pinnable1}, allocation.Pinned)
	assert.Equal(
		t,
		[]Exclusion{{Image: deepPath, Reason: ReasonUnsupportedReferenceShape}},
		allocation.Excluded,
	)
}

// TestAdmissionRulesHaveNotDrifted holds the two expressions above to the vendored API type
// they were copied from. They cannot be derived from it at compile time, because the type
// carries them as kubebuilder markers rather than as code — `machineconfiguration/v1` exports
// no validation function at all — so the next best thing is to fail loudly when a dependency
// bump changes a rule underneath us.
//
// This is worth a test rather than a comment because of what being wrong costs. Being too
// strict loses pinning for one image; being too permissive lets one entry into the set that
// the API server rejects, and rejection is per object, so every other image stops being
// pinned too.
func TestAdmissionRulesHaveNotDrifted(t *testing.T) {
	const vendoredType = "../../../vendor/github.com/openshift/api/machineconfiguration/v1/types_machineosconfig.go"

	source, err := os.ReadFile(vendoredType)
	if err != nil {
		t.Fatalf("failed to read the vendored API type: %v", err)
	}

	markers := markersFor(string(source), "type ImageDigestFormat string")
	if len(markers) == 0 {
		t.Fatalf("found no markers on ImageDigestFormat in %s, the type may have been renamed", vendoredType)
	}

	assert.Equal(
		t,
		[]string{digestReferenceReg.String(), imageNameReg.String()},
		rulesFrom(markers),
		"the CEL validation on ImageDigestFormat no longer matches what Allocate filters on",
	)
	assert.Contains(t, markers, fmt.Sprintf("+kubebuilder:validation:MaxLength=%d", maxPinnedImageReferenceLength))
}

// TestSetSizeLimitsHaveNotDrifted holds the other two limits the allocator encodes to the same
// vendored source. They are enforced on the list rather than on an entry, and failing either
// is rejected per object, so one image too many does not cost that image its pinning — it
// costs every image in the set, exactly like a malformed reference.
//
// MaxItems is what MaxPinnedImages exists to stay under, and MinItems is why an empty set is
// deleted rather than emptied. Neither can be derived at compile time, the markers being
// comments, so the next best thing is to fail loudly when a dependency bump moves one.
func TestSetSizeLimitsHaveNotDrifted(t *testing.T) {
	const vendoredType = "../../../vendor/github.com/openshift/api/machineconfiguration/v1/types_pinnedimageset.go"

	source, err := os.ReadFile(vendoredType)
	if err != nil {
		t.Fatalf("failed to read the vendored API type: %v", err)
	}

	markers := markersFor(string(source), "PinnedImages []PinnedImageRef `json:\"pinnedImages\"`")
	if len(markers) == 0 {
		t.Fatalf("found no markers on PinnedImages in %s, the field may have been renamed", vendoredType)
	}

	assert.Contains(t, markers, fmt.Sprintf("+kubebuilder:validation:MaxItems=%d", MaxPinnedImages),
		"the cap the allocator fills up to no longer matches the one the API server enforces")
	assert.Contains(t, markers, "+kubebuilder:validation:MinItems=1",
		"an empty set is deleted rather than emptied because the API server refuses one")
}

// markersFor returns the comment markers immediately preceding a declaration.
func markersFor(source string, declaration string) []string {
	markers := make([]string, 0)

	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)

		switch {
		case line == declaration:
			return markers

		case strings.HasPrefix(line, "// +kubebuilder:"):
			markers = append(markers, strings.TrimPrefix(line, "// "))

		case !strings.HasPrefix(line, "//"):
			// Any non comment line ends the run of markers that belongs to a declaration.
			markers = markers[:0]
		}
	}

	return nil
}

// rulesFrom pulls the regular expressions out of the XValidation markers. The markers hold CEL,
// and CEL in turn holds the expression inside a quoted string literal, so the doubled
// backslashes have to be undone to recover the expression a regexp engine sees.
func rulesFrom(markers []string) []string {
	matchesReg := regexp.MustCompile(`matches\('(.*?)'\)`)
	rules := make([]string, 0)

	for _, marker := range markers {
		for _, match := range matchesReg.FindAllStringSubmatch(marker, -1) {
			rules = append(rules, strings.ReplaceAll(match[1], `\\`, `\`))
		}
	}

	return rules
}
