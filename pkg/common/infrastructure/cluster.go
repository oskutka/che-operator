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

package infrastructure

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/eclipse-che/che-operator/pkg/common/constants"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

type Type int

const (
	Unknown Type = iota
	Kubernetes
	OpenShiftV4

	LeasesResources                = "leases"
	OAuthClientsResources          = "oauthclients"
	KubernetesImagePullerResources = "kubernetesimagepullers"
	ServiceMonitorResources        = "servicemonitors"
	PinnedImageSetResources        = "pinnedimagesets"

	machineConfigOperatorNamespace = "openshift-machine-config-operator"
	machineConfigDaemonName        = "machine-config-daemon"
)

var (
	infrastructure = Unknown

	isOpenShiftOAuthEnabled bool
	isLeaderElectionEnabled bool
	isServiceMonitorEnabled bool

	operatorNamespace string

	logger = ctrl.Log.WithName("infrastructure")
)

// GetOperatorNamespace returns the namespace where the operator is running.
// The result is cached to avoid repeated filesystem reads.
func GetOperatorNamespace() (string, error) {
	if operatorNamespace == "" {
		nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err == nil {
			operatorNamespace = strings.TrimSpace(string(nsBytes))
			return operatorNamespace, nil
		}

		// for the purpose of local run
		namespace, ok := os.LookupEnv("WATCH_NAMESPACE")
		if ok {
			operatorNamespace = namespace
			return operatorNamespace, nil
		}

		return "", fmt.Errorf("operator namespace is not set")
	}

	return operatorNamespace, nil
}

func IsOpenShift() bool {
	initializeIfNeeded()
	return infrastructure == OpenShiftV4
}

func IsOpenShiftOAuthEnabled() bool {
	initializeIfNeeded()
	return isOpenShiftOAuthEnabled
}

func IsOpenShiftExternalAuth() bool {
	initializeIfNeeded()
	return IsOpenShift() && !IsOpenShiftOAuthEnabled()
}

func IsLeaderElectionEnabled() bool {
	initializeIfNeeded()
	return isLeaderElectionEnabled
}

func IsKubernetesImagePullerEnabled(discovery discovery.DiscoveryInterface) bool {
	_, apiResources, err := discovery.ServerGroupsAndResources()
	if err != nil {
		logger.Error(err, "Failed to get API resources list")
		return false
	}

	return hasAPIResource(apiResources, KubernetesImagePullerResources)
}

func IsServiceMonitorEnabled() bool {
	initializeIfNeeded()
	return isServiceMonitorEnabled
}

// IsPinnedImageSetAPIAvailable returns true if the cluster serves the PinnedImageSet API.
// Callers that only read or delete the object must use this rather than IsPinnedImageSetEnabled:
// an object left over from an earlier configuration still has to be cleaned up on a cluster
// where the backend is no longer usable. Touching the object when the API is absent fails with
// a no-matches error, which is not a not-found error and therefore is not ignored.
func IsPinnedImageSetAPIAvailable(discovery discovery.DiscoveryInterface) bool {
	_, apiResources, err := discovery.ServerGroupsAndResources()
	if err != nil {
		logger.Error(err, "Failed to get API resources list")
		return false
	}

	return hasAPIResource(apiResources, PinnedImageSetResources)
}

// IsPinnedImageSetEnabled returns true if the cluster is able to actually reconcile a PinnedImageSet.
// Three things are required: the API being served, the machine-config-daemon being present, and a
// MachineConfigPool that selects the role the set is labelled with.
//
// The pool is part of the capability and not merely a precondition of creating the object, because
// a caller allocates images to this backend on the strength of the answer. Reporting the backend as
// usable where no pool would consume the set means those images are pinned by nobody and pre-pulled
// by nobody either, since the community backend was never offered them. Single node OpenShift is
// the case that matters: every node is in the master pool, so there is no worker pool at all.
func IsPinnedImageSetEnabled(discovery discovery.DiscoveryInterface, cli client.Client) bool {
	if !IsPinnedImageSetAPIAvailable(discovery) {
		return false
	}

	isMachineConfigDaemonAvailable, err := IsMachineConfigDaemonAvailable(cli)
	if err != nil {
		logger.Error(err, "Failed to look up the machine-config-daemon, considering PinnedImageSet as not usable")
		return false
	}

	if !isMachineConfigDaemonAvailable {
		return false
	}

	machineConfigPool, err := FindMachineConfigPool(cli, constants.MachineConfigPoolRole)
	if err != nil {
		logger.Error(err, "Failed to look up a MachineConfigPool, considering PinnedImageSet as not usable")
		return false
	}

	return machineConfigPool != ""
}

// FindMachineConfigPool returns the name of the pool that would select a PinnedImageSet labelled
// with the given role, or an empty string if no pool would. The pool selects the set rather than
// the other way round, so this evaluates each pool's own selector against the label the set would
// carry, which is what the Machine Config Operator does.
func FindMachineConfigPool(cli client.Client, role string) (string, error) {
	machineConfigPools := &mcfgv1.MachineConfigPoolList{}
	if err := cli.List(context.TODO(), machineConfigPools); err != nil {
		return "", fmt.Errorf("failed to list machineconfigpools: %w", err)
	}

	pinnedImageSetLabels := labels.Set{constants.MachineConfigPoolRoleLabelKey: role}

	for _, machineConfigPool := range machineConfigPools.Items {
		selector, err := metav1.LabelSelectorAsSelector(machineConfigPool.Spec.MachineConfigSelector)
		if err != nil {
			return "", fmt.Errorf("failed to parse the selector of machineconfigpool %s: %w", machineConfigPool.Name, err)
		}

		if selector.Matches(pinnedImageSetLabels) {
			return machineConfigPool.Name, nil
		}
	}

	return "", nil
}

// IsMachineConfigDaemonAvailable returns true if the machine-config-daemon runs on the cluster nodes.
// It is the daemon that actually pulls and pins the images, so without it a PinnedImageSet is admitted
// and then silently ignored: no status, no event, no condition anywhere on the cluster. Hosted control
// planes (ROSA HCP, ARO HCP) are the common case, since the Machine Config Operator is excluded from
// the hosted payload and the daemon never runs on the nodes. The daemon is looked up directly rather
// than inferred from the control plane topology, so that any other cluster flavour missing it is
// covered as well. See https://issues.redhat.com/browse/OCPBUGS-123525.
//
// The lookup is deliberately for a DaemonSet specifically. The openshift-machine-config-operator
// namespace does exist on a hosted cluster, and even holds Services named machine-config-daemon and
// machine-config-controller with no workloads behind them, so a namespace or Service based probe
// would report the backend as usable on exactly the platform this guard exists for.
func IsMachineConfigDaemonAvailable(cli client.Client) (bool, error) {
	key := types.NamespacedName{Namespace: machineConfigOperatorNamespace, Name: machineConfigDaemonName}

	if err := cli.Get(context.TODO(), key, &appsv1.DaemonSet{}); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get daemonset %s/%s: %w", key.Namespace, key.Name, err)
	}

	return true, nil
}

// PinnedImageSetName is the name the set carries for a given CheCluster. A PinnedImageSet is
// cluster-scoped and several CheClusters may exist on one cluster, so the name is qualified
// with the namespace. Shared, rather than derived where it is used, because the capability
// check below and the reconciler that creates the object have to agree on it exactly.
func PinnedImageSetName(cheClusterNamespace string, cheClusterName string) string {
	return cheClusterNamespace + "-" + cheClusterName
}

// IsPinnedImageSetNameTaken reports whether a set of this CheCluster's name already exists and
// records a different CheCluster as its owner.
//
// Joining the namespace and the name with a dash does not produce a unique string: foo/bar-baz
// and foo-bar/baz both give foo-bar-baz. Two CheClusters sharing the object would each rewrite
// the other's image list on every pass, and since the Machine Config Operator merges every set
// targeting a pool, the whole worker pool would roll for each flip, on both tenants.
//
// A set written before this operator recorded ownership carries neither label and is reported
// as untaken, so that an upgrade adopts it rather than orphaning it.
func IsPinnedImageSetNameTaken(cli client.Client, cheClusterNamespace string, cheClusterName string) (bool, error) {
	pinnedImageSet := &mcfgv1.PinnedImageSet{}
	key := types.NamespacedName{Name: PinnedImageSetName(cheClusterNamespace, cheClusterName)}

	if err := cli.Get(context.TODO(), key, pinnedImageSet); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get pinnedimageset %s: %w", key.Name, err)
	}

	namespace, hasNamespace := pinnedImageSet.Labels[constants.CheClusterNamespaceLabelKey]
	name, hasName := pinnedImageSet.Labels[constants.CheClusterNameLabelKey]
	if !hasNamespace || !hasName {
		return false, nil
	}

	return namespace != cheClusterNamespace || name != cheClusterName, nil
}

func SetOpenShiftOAuthEnabledForTesting(enabled bool) {
	isOpenShiftOAuthEnabled = enabled
}

func InitializeForTesting(desiredInfrastructure Type) {
	infrastructure = desiredInfrastructure

	if IsOpenShift() {
		isOpenShiftOAuthEnabled = true
		operatorNamespace = "openshift-operators"
	} else {
		isOpenShiftOAuthEnabled = false
		operatorNamespace = "eclipse-che"
	}

	isLeaderElectionEnabled = true
	isServiceMonitorEnabled = true
}

func initializeIfNeeded() {
	if infrastructure != Unknown {
		return
	}

	kubeCfg, err := config.GetConfig()
	if err != nil {
		panic("Failed to get kubeconfig")
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(kubeCfg)
	if err != nil {
		panic("Failed to create discovery client")
	}

	apiGroups, apiResources, err := discoveryClient.ServerGroupsAndResources()
	if err != nil {
		panic("Failed to get API Groups and Resources")
	}

	if hasAPIGroup(apiGroups, "config.openshift.io") {
		infrastructure = OpenShiftV4
		isOpenShiftOAuthEnabled = hasAPIResource(apiResources, OAuthClientsResources)
	} else {
		infrastructure = Kubernetes
		isOpenShiftOAuthEnabled = false
	}

	isLeaderElectionEnabled = hasAPIResource(apiResources, LeasesResources)
	isServiceMonitorEnabled = hasAPIResource(apiResources, ServiceMonitorResources)
}

func hasAPIGroup(source []*metav1.APIGroup, apiName string) bool {
	return slices.ContainsFunc(source, func(g *metav1.APIGroup) bool {
		return g.Name == apiName
	})
}

func hasAPIResource(resources []*metav1.APIResourceList, resourceName string) bool {
	for _, resource := range resources {
		for _, r := range resource.APIResources {
			if r.Name == resourceName {
				return true
			}
		}
	}

	return false
}
