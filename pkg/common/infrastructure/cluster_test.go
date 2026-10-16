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

// The tests live in an external test package because the test client pulls in api/v2,
// which imports the infrastructure package itself.
package infrastructure_test

import (
	"testing"

	"github.com/eclipse-che/che-operator/pkg/common/constants"
	"github.com/eclipse-che/che-operator/pkg/common/infrastructure"
	testclient "github.com/eclipse-che/che-operator/pkg/common/test/test-client"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newMachineConfigDaemon() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DaemonSet",
			APIVersion: appsv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machine-config-daemon",
			Namespace: "openshift-machine-config-operator",
		},
	}
}

func newMachineConfigPool(role string) *mcfgv1.MachineConfigPool {
	return &mcfgv1.MachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: role},
		Spec: mcfgv1.MachineConfigPoolSpec{
			MachineConfigSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{constants.MachineConfigPoolRoleLabelKey: role},
			},
		},
	}
}

func TestIsPinnedImageSetEnabled(t *testing.T) {
	type testCase struct {
		name              string
		pinnedImageSetAPI bool
		initObjs          []client.Object
		expected          bool
	}

	testCases := []testCase{
		{
			name:              "Enabled when the API is served, the daemon runs and a worker pool exists",
			pinnedImageSetAPI: true,
			initObjs:          []client.Object{newMachineConfigDaemon(), newMachineConfigPool("worker")},
			expected:          true,
		},
		{
			name:              "Disabled without a machine-config-daemon, even though the API is served",
			pinnedImageSetAPI: true,
			initObjs:          []client.Object{newMachineConfigPool("worker")},
			expected:          false,
		},
		{
			name:              "Disabled when the API is not served",
			pinnedImageSetAPI: false,
			initObjs:          []client.Object{newMachineConfigDaemon(), newMachineConfigPool("worker")},
			expected:          false,
		},
		// Single node OpenShift. Everything the backend needs is present except somewhere to
		// aim the set, and reporting it as usable costs those images both backends: they are
		// allocated to a set no pool consumes, and never offered to the community operator.
		{
			name:              "Disabled where no pool selects the worker role",
			pinnedImageSetAPI: true,
			initObjs:          []client.Object{newMachineConfigDaemon(), newMachineConfigPool("master")},
			expected:          false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			infrastructure.InitializeForTesting(infrastructure.OpenShiftV4)

			fakeClient, discoveryClient, _ := testclient.GetTestClients(testCase.initObjs...)

			discoveryClient.Resources = []*metav1.APIResourceList{}
			if testCase.pinnedImageSetAPI {
				discoveryClient.Resources = []*metav1.APIResourceList{
					{
						GroupVersion: "machineconfiguration.openshift.io/v1",
						APIResources: []metav1.APIResource{
							{Name: infrastructure.PinnedImageSetResources},
						},
					},
				}
			}

			actual := infrastructure.IsPinnedImageSetEnabled(discoveryClient, fakeClient)

			assert.Equal(t, testCase.expected, actual)
		})
	}
}

// TestIsPinnedImageSetAPIAvailableIgnoresMachineConfigDaemon guards the distinction the cleanup path
// relies on: a leftover object still has to be deletable on a cluster where the backend is not usable.
func TestIsPinnedImageSetAPIAvailableIgnoresMachineConfigDaemon(t *testing.T) {
	infrastructure.InitializeForTesting(infrastructure.OpenShiftV4)

	fakeClient, discoveryClient, _ := testclient.GetTestClients()
	discoveryClient.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "machineconfiguration.openshift.io/v1",
			APIResources: []metav1.APIResource{
				{Name: infrastructure.PinnedImageSetResources},
			},
		},
	}

	assert.True(t, infrastructure.IsPinnedImageSetAPIAvailable(discoveryClient))
	assert.False(t, infrastructure.IsPinnedImageSetEnabled(discoveryClient, fakeClient))

	discoveryClient.Resources = []*metav1.APIResourceList{}
	assert.False(t, infrastructure.IsPinnedImageSetAPIAvailable(discoveryClient))
}

func TestIsMachineConfigDaemonAvailable(t *testing.T) {
	infrastructure.InitializeForTesting(infrastructure.OpenShiftV4)

	fakeClient, _, _ := testclient.GetTestClients()

	isAvailable, err := infrastructure.IsMachineConfigDaemonAvailable(fakeClient)
	assert.NoError(t, err)
	assert.False(t, isAvailable)

	fakeClient, _, _ = testclient.GetTestClients(newMachineConfigDaemon())

	isAvailable, err = infrastructure.IsMachineConfigDaemonAvailable(fakeClient)
	assert.NoError(t, err)
	assert.True(t, isAvailable)
}
