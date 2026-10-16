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
	"github.com/eclipse-che/che-operator/pkg/common/chetypes"
)

// The two backends are no longer mutually exclusive. Which one serves a given image is decided
// per image by Allocate, so neither reconciler asks "is this my backend?" any more: each one
// looks at its own bucket and provisions itself when that bucket is non-empty. That is what
// lets a cluster pin the digest referenced images and still pre-pull the rest.

// IsPinnedImageSetBackendRequestUnsatisfiable returns true if pinning was asked for but the
// cluster cannot reconcile a PinnedImageSet. It exists so that the fall back to the community
// backend is explainable rather than silent, which matters more now that pinning is opt in:
// nothing on the cluster distinguishes "nobody asked" from "asked for and unavailable".
func IsPinnedImageSetBackendRequestUnsatisfiable(ctx *chetypes.DeployContext) bool {
	capabilities := GetCapabilities(ctx)

	return capabilities.Enabled &&
		capabilities.PinnedImageSetRequested &&
		!capabilities.PinnedImageSetUsable
}
