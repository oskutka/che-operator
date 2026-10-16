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
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eclipse-che/che-operator/pkg/common/chetypes"
	"github.com/eclipse-che/che-operator/pkg/common/constants"
	"github.com/eclipse-che/che-operator/pkg/common/diffs"
	k8s_client "github.com/eclipse-che/che-operator/pkg/common/k8s-client"
	"github.com/eclipse-che/che-operator/pkg/common/reconciler"
	"github.com/eclipse-che/che-operator/pkg/common/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ctrl "sigs.k8s.io/controller-runtime"

	chev1alpha1 "github.com/che-incubator/kubernetes-image-puller-operator/api/v1alpha1"
	"github.com/eclipse-che/che-operator/pkg/deploy"
)

var (
	logger = ctrl.Log.WithName("image-puller")
)

const (
	finalizerName = "kubernetesimagepullers.finalizers.che.eclipse.org"

	defaultConfigMapName  = "k8s-image-puller"
	defaultDeploymentName = "kubernetes-image-puller"
)

// ImagesProvider yields the images to pre-pull. Both backends consume the very same list, so
// that which images are pre-pulled never depends on which backend ends up serving them.
type ImagesProvider interface {
	Get(ctx *chetypes.DeployContext) ([]string, error)
}

type ImagePuller struct {
	reconciler.Reconcilable
	externalImages ImagesProvider
}

func NewImagePuller() *ImagePuller {
	return NewImagePullerWithImagesProvider(NewExternalImagesProvider())
}

// NewImagePullerWithImagesProvider builds the reconciler over a given list. It exists so that
// both reconcilers can be driven over one list and their two halves compared, which is the only
// way to check that every image ends up served by exactly one of them.
func NewImagePullerWithImagesProvider(externalImages ImagesProvider) *ImagePuller {
	return &ImagePuller{
		externalImages: externalImages,
	}
}

func (ip *ImagePuller) Reconcile(ctx *chetypes.DeployContext) (reconcile.Result, bool, error) {
	images, known, err := ImagesToPrePull(ctx, ip.externalImages)
	if err != nil {
		return reconcile.Result{}, false, err
	}

	if !known {
		return reconcile.Result{}, true, nil
	}

	capabilities := GetCapabilities(ctx)
	allocation := Allocate(images, capabilities)

	// Only a cluster where neither backend can serve anything is an error. Where the native
	// backend is usable, images it cannot carry are a partial result that the allocation log
	// reports, and failing here would stop the reconciler chain before the PinnedImageSet
	// reconciler ever ran, costing every digest referenced image its pinning over one tag.
	if allocation.HasNoUsableBackend() {
		errMsg := "kubernetes Image Puller is not installed, in order to enable the property admin should install the operator first"
		return reconcile.Result{}, false, errors.New(errMsg)
	}

	// The community backend removes itself once its share of the list empties, rather than
	// once the backend selection flips. On a cluster where every image is digest referenced
	// that means it is never provisioned at all, without the administrator changing anything.
	if len(allocation.Prepulled) == 0 {
		if done, err := ip.uninstallImagePuller(ctx); !done {
			return reconcile.Result{RequeueAfter: time.Second}, false, err
		}

		return reconcile.Result{}, true, nil
	}

	if done, err := ip.syncKubernetesImagePuller(allocation.Prepulled, ctx); !done {
		return reconcile.Result{RequeueAfter: time.Second}, false, err
	}

	return reconcile.Result{}, true, nil
}

func (ip *ImagePuller) Finalize(ctx *chetypes.DeployContext) bool {
	done, err := ip.uninstallImagePuller(ctx)
	if err != nil {
		logger.Error(err, "Failed to uninstall Kubernetes Image Puller")
	}
	return done
}

func (ip *ImagePuller) uninstallImagePuller(ctx *chetypes.DeployContext) (bool, error) {
	// Keep it here for backward compatibility
	if err := deploy.DeleteFinalizer(ctx, finalizerName); err != nil {
		return false, err
	}

	// Through the capabilities, so that the answer comes from the memo on the reconcile pass
	// rather than from a second discovery enumeration.
	if GetCapabilities(ctx).ImagePullerUsable {
		if done, err := deploy.DeleteByKeyWithClient(
			ctx.ClusterAPI.NonCachingClient,
			types.NamespacedName{
				Namespace: ctx.CheCluster.Namespace,
				Name:      getImagePullerCustomResourceName(ctx)},
			&chev1alpha1.KubernetesImagePuller{},
		); !done {
			return false, err
		}
	}

	return true, nil
}

// syncKubernetesImagePuller provisions the community backend for the images allocated to it.
// Those are passed in rather than taken from the CheCluster, because an administrator supplied
// list is now split between the two backends as well, so the list the community backend runs
// is a subset of the one that was configured.
func (ip *ImagePuller) syncKubernetesImagePuller(prepulledImages []string, ctx *chetypes.DeployContext) (bool, error) {
	imagePuller := &chev1alpha1.KubernetesImagePuller{
		TypeMeta: metav1.TypeMeta{
			APIVersion: chev1alpha1.GroupVersion.String(),
			Kind:       "KubernetesImagePuller",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      getImagePullerCustomResourceName(ctx),
			Namespace: ctx.CheCluster.Namespace,
			Labels: map[string]string{
				constants.KubernetesComponentLabelKey: constants.KubernetesImagePullerComponentName,
				constants.KubernetesPartOfLabelKey:    constants.CheEclipseOrg,
				constants.KubernetesManagedByLabelKey: deploy.GetManagedByLabel(),
			},
		},
		Spec: *ctx.CheCluster.Spec.Components.ImagePuller.Spec.DeepCopy(),
	}

	// Set default values to avoid syncing object on every loop
	// See https://github.com/che-incubator/kubernetes-image-puller-operator/blob/main/controllers/kubernetesimagepuller_controller.go
	imagePuller.Spec.ConfigMapName = utils.GetValue(imagePuller.Spec.ConfigMapName, defaultConfigMapName)
	imagePuller.Spec.DeploymentName = utils.GetValue(imagePuller.Spec.DeploymentName, defaultDeploymentName)
	// Read before the field is overwritten: Spec was copied from the CheCluster above, so at
	// this point it still holds whatever the administrator wrote.
	imagePuller.Spec.Images = convertToSpecField(prepulledImages, namesFromSpecField(imagePuller.Spec.Images))

	if err := controllerutil.SetControllerReference(ctx.CheCluster, imagePuller, ctx.ClusterAPI.Scheme); err != nil {
		return false, err
	}

	err := ctx.ClusterAPI.NonCachingClientWrapper.Sync(
		context.TODO(),
		imagePuller,
		&k8s_client.SyncOptions{
			DiffOpts: diffs.KubernetesImagePullerDiffOpts,
		})

	return err == nil, fmt.Errorf("failed to sync KubernetesImagePuller %s/%s: %w", imagePuller.GetNamespace(), imagePuller.GetName(), err)
}

func getImagePullerCustomResourceName(ctx *chetypes.DeployContext) string {
	return ctx.CheCluster.Name + "-image-puller"
}

// convertToSpecField serialises the images for the community backend. preferredNames carries
// the names an administrator already chose, keyed by image, and may be nil.
//
// Those names are kept rather than regenerated because the field is now rewritten on every
// reconcile instead of only when empty, and the community backend turns each name into a
// container name in its DaemonSet. Deriving our own would rename every container the first
// time an upgraded operator reconciled, rolling the DaemonSet and briefly dropping the very
// images it exists to hold, for a cluster that changed no configuration.
func convertToSpecField(images []string, preferredNames map[string]string) string {
	specField := ""
	for index, image := range images {
		name, found := preferredNames[image]
		if !found {
			imageName, _ := utils.GetImageNameAndTag(image)
			imageNameEntries := strings.Split(imageName, "/")

			var err error
			if name, err = convertToRFC1123(imageNameEntries[len(imageNameEntries)-1]); err != nil {
				name = "image"
			}

			// Adding index makes the name unique
			name = fmt.Sprintf("%s-%d", name, index)
		}

		specField += fmt.Sprintf("%s=%s;", name, image)
	}

	return specField
}

// namesFromSpecField recovers the name an administrator gave each image, so that rewriting the
// field does not rename what they named. Entries carrying no name of their own are absent from
// the result and are named by convertToSpecField instead.
func namesFromSpecField(specField string) map[string]string {
	names := make(map[string]string)

	for _, entry := range strings.Split(specField, ";") {
		name, image, found := strings.Cut(strings.TrimSpace(entry), "=")
		if !found {
			continue
		}

		// First occurrence wins, because Allocate collapses duplicates the same way. Letting
		// the last win would name the surviving image after the entry that was discarded.
		name, image = strings.TrimSpace(name), strings.TrimSpace(image)
		if _, named := names[image]; !named && name != "" && image != "" {
			names[image] = name
		}
	}

	return names
}

// ImagesFromSpecField is the inverse of convertToSpecField. It exists because
// `spec.images` is the only durable record of what the community backend was asked to
// pre-pull, so reading the accounting back, whether in a test or from a live cluster,
// means parsing it. The names are discarded: they are derived from the image reference
// and carry no information of their own.
func ImagesFromSpecField(specField string) []string {
	images := make([]string, 0)

	for _, entry := range strings.Split(specField, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// An image reference may itself contain '=' in a tag, so only the first one splits.
		// An entry carrying no name at all is taken to be the image, rather than skipped. This
		// field is no longer passed through untouched — it is parsed, split between the two
		// backends and written back — so anything the parser declines to recognise is removed
		// from the cluster by the next reconcile, with nothing in the accounting to say so.
		// Returning it keeps it in the allocation, where it is either served by a backend or
		// reported as ReasonMalformedReference, which is the outcome worth having either way.
		_, image, found := strings.Cut(entry, "=")
		if !found {
			image = entry
		}

		if image = strings.TrimSpace(image); image != "" {
			images = append(images, image)
		}
	}

	return images
}

// convertToRFC1123 converts input string to RFC 1123 format ([a-z0-9]([-a-z0-9]*[a-z0-9])?) max 63 characters, if possible
func convertToRFC1123(str string) (string, error) {
	result := strings.ToLower(str)
	if len(str) > validation.DNS1123LabelMaxLength {
		result = result[:validation.DNS1123LabelMaxLength]
	}

	// Remove illegal trailing characters
	i := len(result) - 1
	for i >= 0 && !isRFC1123Char(result[i]) {
		i -= 1
	}
	result = result[:i+1]

	result = strings.ReplaceAll(result, "_", "-")

	if errs := validation.IsDNS1123Label(result); len(errs) > 0 {
		return "", fmt.Errorf("cannot convert the following string to RFC 1123 format: %s", str)
	}
	return result, nil
}

func isRFC1123Char(ch byte) bool {
	errs := validation.IsDNS1123Label(string(ch))
	return len(errs) == 0
}
