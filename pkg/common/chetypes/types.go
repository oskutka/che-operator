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

package chetypes

import (
	"context"

	chev2 "github.com/eclipse-che/che-operator/api/v2"
	k8sclient "github.com/eclipse-che/che-operator/pkg/common/k8s-client"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type DeployContext struct {
	CheCluster              *chev2.CheCluster
	ClusterAPI              ClusterAPI
	Proxy                   *Proxy
	Authentication          *Authentication
	IsSelfSignedCertificate bool
	CheHost                 string
	DWONamespace            string
	Context                 context.Context

	// PrePullImages is the image list both pre-pull backends consume, resolved at most once
	// per reconcile pass. Memoized here, next to Proxy and Authentication, rather than in
	// either reconciler: the two run separately and each holds its own provider, so without
	// this they would allocate from two different dashboard responses. An image that moved
	// between the two fetches would be served by neither backend, or by both.
	//
	// Nil until something asks for it, so pre-pulling that is switched off still never calls
	// the dashboard at all.
	PrePullImages *PrePullImages

	// PrePullBackends records which pre-pull backends this cluster can actually run, resolved
	// at most once per reconcile pass. Only the cluster side is held here; what the CheCluster
	// asks for is read fresh every time, being free.
	//
	// Memoized because answering costs a full discovery enumeration per backend, plus a
	// MachineConfigPool list and a DaemonSet read, and both reconcilers ask more than once per
	// pass. Nil until something asks.
	PrePullBackends *PrePullBackends
}

// PrePullBackends is what the cluster, as opposed to the CheCluster, allows.
type PrePullBackends struct {
	PinnedImageSetUsable bool
	ImagePullerUsable    bool

	// PinnedImageSetNameTaken records the one reason pinning is unusable that is about this
	// CheCluster rather than about the cluster: another CheCluster already owns a set of the
	// name this one would use. Kept apart from PinnedImageSetUsable only so that the reason
	// can be reported accurately, the remedy being a rename rather than a cluster change.
	PinnedImageSetNameTaken bool
}

// PrePullImages is a resolved pre-pull image list.
type PrePullImages struct {
	Images []string

	// Known distinguishes an empty list from one that could not be determined. The two
	// backends treat them very differently — empty means withdraw, unknown means leave the
	// cluster as it is — so collapsing them would tear both backends down over a transient
	// dashboard error.
	Known bool
}

type ClusterAPI struct {
	Client                  client.Client // We should ClientWrapper instead wherever it possible
	NonCachingClient        client.Client // We should NonCachingClientWrapper instead wherever it possible
	DiscoveryClient         discovery.DiscoveryInterface
	Scheme                  *runtime.Scheme
	ClientWrapper           *k8sclient.K8sClientWrapper
	NonCachingClientWrapper *k8sclient.K8sClientWrapper
}

type Authentication struct {
	GroupsClaim    string
	GroupsPrefix   string
	UsernameClaim  string
	UsernamePrefix string

	IssuerURL string
	IssuerCA  string

	ClientId     string
	ClientSecret []byte
}

type Proxy struct {
	HttpProxy    string
	HttpUser     string
	HttpPassword string
	HttpHost     string
	HttpPort     string

	HttpsProxy    string
	HttpsUser     string
	HttpsPassword string
	HttpsHost     string
	HttpsPort     string

	NoProxy          string
	TrustedCAMapName string
}
