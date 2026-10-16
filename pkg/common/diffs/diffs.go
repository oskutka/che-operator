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

package diffs

import (
	"maps"
	"reflect"

	chev1alpha1 "github.com/che-incubator/kubernetes-image-puller-operator/api/v1alpha1"
	"github.com/eclipse-che/che-operator/pkg/common/constants"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	securityv1 "github.com/openshift/api/security/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var Role = cmp.Options{
	cmpopts.IgnoreFields(rbac.Role{}, "TypeMeta", "ObjectMeta"),
}

var RoleBinding = cmp.Options{
	cmpopts.IgnoreFields(rbac.RoleBinding{}, "TypeMeta", "ObjectMeta"),
}

var ClusterRole = cmp.Options{
	cmpopts.IgnoreFields(rbac.ClusterRole{}, "TypeMeta", "ObjectMeta"),
}

var ClusterRoleBinding = cmp.Options{
	cmpopts.IgnoreFields(rbac.ClusterRoleBinding{}, "TypeMeta", "ObjectMeta"),
}

var SecurityContextConstraints = cmp.Options{
	cmpopts.IgnoreFields(securityv1.SecurityContextConstraints{}, "TypeMeta", "ObjectMeta", "Priority"),
}

var ConfigMapEnsureLabels = cmp.Options{
	cmpopts.IgnoreFields(corev1.ConfigMap{}, "TypeMeta"),
	cmp.Comparer(func(x, y metav1.ObjectMeta) bool {
		return maps.Equal(x.Labels, y.Labels)
	}),
}

var Service = cmp.Options{
	cmpopts.IgnoreFields(corev1.Service{}, "TypeMeta", "ObjectMeta", "Status"),
	cmp.Comparer(func(x, y corev1.ServiceSpec) bool {
		return maps.Equal(x.Selector, y.Selector) && reflect.DeepEqual(x.Ports, y.Ports)
	}),
}

var KubernetesImagePullerDiffOpts = cmp.Options{
	cmpopts.IgnoreFields(chev1alpha1.KubernetesImagePuller{}, "TypeMeta", "ObjectMeta", "Status"),
	cmpopts.IgnoreFields(chev1alpha1.KubernetesImagePullerSpec{}, "ImagePullerImage"),
}

// PinnedImageSet compares the role label as well as the spec, because for this object the label
// is not decoration: the Machine Config Operator matches it against a pool's
// `machineConfigSelector` to decide which nodes pre-pull. Ignoring ObjectMeta wholesale would
// leave a set that still names the old pool whenever the role changes under an unchanged image
// list, and nothing on the cluster would report it — the set stays admitted and healthy looking
// while the intended nodes pre-pull nothing.
var PinnedImageSet = cmp.Options{
	cmpopts.IgnoreFields(mcfgv1.PinnedImageSet{}, "TypeMeta"),
	cmpMetadata([]string{
		constants.MachineConfigPoolRoleLabelKey,
		constants.CheClusterNamespaceLabelKey,
		constants.CheClusterNameLabelKey,
	}, nil),

	// spec.pinnedImages is declared listType=map with listMapKey=name, which is the API's way
	// of saying the entries are keyed by name and their order carries no meaning. Comparing
	// them in order would report a reordering as a difference and rewrite the object to undo
	// it — a write, a logged update and a printed diff for two objects that say the same
	// thing. Nothing reorders the list today, since this operator writes it with Update rather
	// than Apply and the list it builds is already deterministic, so this is about not
	// depending on that: an administrator applying the object, or any future use of
	// server-side apply, may legitimately return the entries in another order.
	cmpopts.SortSlices(func(x, y mcfgv1.PinnedImageRef) bool { return x.Name < y.Name }),
}

func Ingress(labels []string, annotations []string) cmp.Options {
	return cmp.Options{
		cmpopts.IgnoreFields(networking.Ingress{}, "TypeMeta", "Status"),
		cmpMetadata(labels, annotations),
	}
}

// ConfigMap respects existed labels and annotations
func ConfigMap(labelKeys []string, annotationKeys []string) cmp.Options {
	return cmp.Options{
		cmpopts.IgnoreFields(corev1.ConfigMap{}, "TypeMeta"),
		cmpMetadata(labelKeys, annotationKeys),
	}
}

var NetworkPolicy = cmp.Options{
	cmpopts.IgnoreFields(networking.NetworkPolicy{}, "TypeMeta", "ObjectMeta"),
}

var ServiceMonitor = cmp.Options{
	cmpopts.IgnoreFields(monitoringv1.ServiceMonitor{}, "TypeMeta", "ObjectMeta"),
}

func cmpMetadata(labels []string, annotations []string) cmp.Option {
	return cmp.Comparer(func(x, y metav1.ObjectMeta) bool {
		for _, label := range labels {
			if x.Labels[label] != y.Labels[label] {
				return false
			}
		}

		for _, annotation := range annotations {
			if x.Annotations[annotation] != y.Annotations[annotation] {
				return false
			}
		}

		return true
	})
}
