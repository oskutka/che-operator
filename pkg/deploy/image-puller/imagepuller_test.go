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
	"os"
	"path/filepath"

	"sigs.k8s.io/controller-runtime/pkg/client"

	chev1alpha1 "github.com/che-incubator/kubernetes-image-puller-operator/api/v1alpha1"
	"github.com/eclipse-che/che-operator/pkg/deploy"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/errors"

	"testing"

	chev2 "github.com/eclipse-che/che-operator/api/v2"
	"github.com/eclipse-che/che-operator/pkg/common/constants"
	"github.com/eclipse-che/che-operator/pkg/common/test"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestImagePullerConfiguration(t *testing.T) {
	type testCase struct {
		name                string
		cheCluster          *chev2.CheCluster
		initObjects         []client.Object
		testCaseFilePath    string
		expectedImagePuller *chev1alpha1.KubernetesImagePuller
	}

	testCases := []testCase{
		{
			name: "case #1: KubernetesImagePuller with defaults",
			cheCluster: InitCheCluster(chev2.ImagePuller{
				Enable: true,
			}),
			testCaseFilePath: "image-puller-resources-test/imagepuller_testcase_1.json",
			expectedImagePuller: InitImagePuller(chev1alpha1.KubernetesImagePullerSpec{
				DeploymentName: defaultDeploymentName,
				ConfigMapName:  defaultConfigMapName,
				Images:         "image-1-0=image_1;image-2-1=image_2;",
			}),
		},
		{
			// An administrator supplied list is now split between the two backends like any
			// other, so the community backend is given its share rather than the raw field.
			// The images are unchanged, but the entry names are regenerated, since they are
			// derived from the reference and carry nothing an administrator chose.
			name: "case #2: KubernetesImagePuller with custom configuration",
			cheCluster: InitCheCluster(chev2.ImagePuller{
				Enable: true,
				Spec: chev1alpha1.KubernetesImagePullerSpec{
					ConfigMapName:    "custom-config-map",
					ImagePullerImage: "custom-image",
					DeploymentName:   "custom-deployment",
					Images:           "image=image_url;",
				}}),
			testCaseFilePath: "image-puller-resources-test/imagepuller_testcase_1.json",
			expectedImagePuller: InitImagePuller(chev1alpha1.KubernetesImagePullerSpec{
				ConfigMapName:    "custom-config-map",
				ImagePullerImage: "custom-image",
				DeploymentName:   "custom-deployment",
				Images:           "image=image_url;",
			}),
		},
		{
			name: "case #3: KubernetesImagePuller already exists",
			cheCluster: InitCheCluster(chev2.ImagePuller{
				Enable: true,
			}),
			testCaseFilePath: "image-puller-resources-test/imagepuller_testcase_2.json",
			initObjects: []client.Object{
				InitImagePuller(chev1alpha1.KubernetesImagePullerSpec{
					DeploymentName: defaultDeploymentName,
					ConfigMapName:  defaultConfigMapName,
				}),
			},
			expectedImagePuller: InitImagePuller(chev1alpha1.KubernetesImagePullerSpec{
				DeploymentName: defaultDeploymentName,
				ConfigMapName:  defaultConfigMapName,
				Images:         "image-1-0=image_1;image-2-1=image_2;image-3-2=image_3;",
			}),
		},
		{
			name: "case #4: KubernetesImagePuller already exists and updated with custom configuration",
			cheCluster: InitCheCluster(chev2.ImagePuller{
				Enable: true,
				Spec: chev1alpha1.KubernetesImagePullerSpec{
					ConfigMapName:    "custom-config-map",
					ImagePullerImage: "custom-image",
					DeploymentName:   "custom-deployment",
					Images:           "image=image_url;",
				}}),
			testCaseFilePath: "image-puller-resources-test/imagepuller_testcase_1.json",
			initObjects: []client.Object{
				InitImagePuller(chev1alpha1.KubernetesImagePullerSpec{
					DeploymentName: defaultDeploymentName,
					ConfigMapName:  defaultConfigMapName,
				}),
			},
			expectedImagePuller: InitImagePuller(chev1alpha1.KubernetesImagePullerSpec{
				ConfigMapName:    "custom-config-map",
				ImagePullerImage: "custom-image",
				DeploymentName:   "custom-deployment",
				Images:           "image=image_url;",
			}),
		},
		{
			name: "case #5: Delete KubernetesImagePuller",
			cheCluster: InitCheCluster(chev2.ImagePuller{
				Enable: false,
			}),
			testCaseFilePath: "image-puller-resources-test/imagepuller_testcase_1.json",
			initObjects: []client.Object{
				InitImagePuller(chev1alpha1.KubernetesImagePullerSpec{
					DeploymentName: defaultDeploymentName,
					ConfigMapName:  defaultConfigMapName,
				}),
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := test.NewCtxBuilder().WithCheCluster(testCase.cheCluster).WithObjects(testCase.initObjects...).Build()

			ip := &ImagePuller{
				externalImages: &ExternalImagesProvider{
					imagesFilePath: filepath.Join(os.TempDir(), externalImagesStoreFileName),
					fetchRawDataFunc: func(url string) ([]byte, error) {
						return os.ReadFile(testCase.testCaseFilePath)
					},
				},
			}

			test.EnsureReconcile(t, ctx, ip.Reconcile)

			actualImagePuller := &chev1alpha1.KubernetesImagePuller{}
			err := ctx.ClusterAPI.Client.Get(context.TODO(), types.NamespacedName{Namespace: "eclipse-che", Name: "eclipse-che-image-puller"}, actualImagePuller)
			if testCase.cheCluster.Spec.Components.ImagePuller.Enable {
				assert.NoError(t, err)

				diff := cmp.Diff(
					testCase.expectedImagePuller,
					actualImagePuller,
					cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion", "OwnerReferences"),
					cmpopts.IgnoreFields(chev1alpha1.KubernetesImagePuller{}, "TypeMeta"))
				if diff != "" {
					t.Errorf("Expected KubernetesImagePuller and KubernetesImagePuller returned from API server differ (-want, +got): %v", diff)
				}
			} else {
				assert.True(t, errors.IsNotFound(err))
			}
		})
	}
}

// TestSpecFieldRoundTrip pins the only durable record of what the community backend was asked
// to pre-pull. The names convertToSpecField derives are lossy — they are truncated, lowercased
// and stripped of illegal characters — so the parser has to recover the images and nothing else.
func TestSpecFieldRoundTrip(t *testing.T) {
	testCases := []struct {
		name   string
		images []string
	}{
		{
			name:   "Plain references",
			images: []string{"quay.io/che/image-1:latest", "docker.io/library/ubi:9.7"},
		},
		{
			name:   "Digest references, which contain no '=' but are long",
			images: []string{"quay.io/che/image-1@" + digest1, "quay.io/che/image-2@" + digest2},
		},
		{
			name: "Images whose last path segment collides, so only the index keeps names unique",
			images: []string{
				"quay.io/che-incubator/che-code:next",
				"quay.io/eclipse/che-code:next",
			},
		},
		{
			name: "A name that cannot be converted to RFC 1123 at all, which falls back to 'image'",
			images: []string{
				"quay.io/che/_____:latest",
				"quay.io/che/image-1:latest",
			},
		},
		{
			name:   "A single image",
			images: []string{"quay.io/che/image-1:latest"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.images, ImagesFromSpecField(convertToSpecField(testCase.images, nil)))
		})
	}
}

func TestImagesFromSpecField(t *testing.T) {
	assert.Empty(t, ImagesFromSpecField(""))
	assert.Empty(t, ImagesFromSpecField(";;"))

	// Hand written values do not necessarily carry the trailing separator our own writer adds.
	assert.Equal(t, []string{"image_1"}, ImagesFromSpecField("image-1=image_1"))

	// Whitespace around hand written entries is not part of the image reference.
	assert.Equal(t, []string{"image_1", "image_2"}, ImagesFromSpecField(" image-1 = image_1 ; image-2=image_2 ;"))

	// An entry without a name is taken to be the image. The alternative, skipping it, loses it
	// silently: the field is rewritten from the parsed result, so an unrecognised entry is gone
	// from the cluster next reconcile and appears in no bucket of the allocation either.
	assert.Equal(t, []string{"image_1"}, ImagesFromSpecField("image_1;"))
	assert.Equal(t, []string{"image_1", "image_2"}, ImagesFromSpecField("image_1;named=image_2;"))
}

// TestAnAdministratorKeepsTheirOwnImageNames covers the field being rewritten on every
// reconcile rather than only when empty. The names become container names in the community
// backend's DaemonSet, so regenerating them would roll it on an upgrade that changed nothing.
func TestAnAdministratorKeepsTheirOwnImageNames(t *testing.T) {
	const written = "my-own-name=quay.io/che/image-1:latest;another=quay.io/che/image-2:latest;"

	names := namesFromSpecField(written)
	images := ImagesFromSpecField(written)

	assert.Equal(t, written, convertToSpecField(images, names), "rewriting an untouched list must not change it")

	// Only the half the community backend still serves, as an opted in cluster would write it.
	assert.Equal(t,
		"another=quay.io/che/image-2:latest;",
		convertToSpecField([]string{"quay.io/che/image-2:latest"}, names))

	// An image the administrator never named falls back to a derived name, and the derived
	// names stay unique among themselves.
	assert.Equal(t,
		"my-own-name=quay.io/che/image-1:latest;image-3-1=quay.io/che/image-3:latest;",
		convertToSpecField([]string{"quay.io/che/image-1:latest", "quay.io/che/image-3:latest"}, names))
}

// TestNamesFromSpecFieldIgnoresWhatItCannotName guards the lookup against the same hand
// written input ImagesFromSpecField has to tolerate.
func TestNamesFromSpecFieldIgnoresWhatItCannotName(t *testing.T) {
	assert.Empty(t, namesFromSpecField(""))
	assert.Empty(t, namesFromSpecField("image_1;"))
	assert.Empty(t, namesFromSpecField("=image_1;"))
	assert.Equal(t, map[string]string{"image_1": "name"}, namesFromSpecField(" name = image_1 ;"))

	// Allocate keeps the first of a repeated image, so the first name has to survive with it.
	assert.Equal(t,
		map[string]string{"image_1": "first"},
		namesFromSpecField("first=image_1;second=image_1;"))
}

func InitCheCluster(imagePuller chev2.ImagePuller) *chev2.CheCluster {
	return &chev2.CheCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eclipse-che",
			Namespace: "eclipse-che",
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "org.eclipse.che/v2",
			Kind:       "CheCluster",
		},
		Spec: chev2.CheClusterSpec{
			Components: chev2.CheClusterComponents{
				ImagePuller: imagePuller,
			},
		},
	}
}

func InitImagePuller(kubernetesImagePullerSpec chev1alpha1.KubernetesImagePullerSpec) *chev1alpha1.KubernetesImagePuller {
	return &chev1alpha1.KubernetesImagePuller{
		TypeMeta: metav1.TypeMeta{
			APIVersion: chev1alpha1.GroupVersion.String(),
			Kind:       "KubernetesImagePuller",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eclipse-che-image-puller",
			Namespace: "eclipse-che",
			Labels: map[string]string{
				constants.KubernetesComponentLabelKey: constants.KubernetesImagePullerComponentName,
				constants.KubernetesPartOfLabelKey:    constants.CheEclipseOrg,
				constants.KubernetesManagedByLabelKey: deploy.GetManagedByLabel(),
			},
		},
		Spec: kubernetesImagePullerSpec,
	}
}
