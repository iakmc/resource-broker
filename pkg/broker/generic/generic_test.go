/*
Copyright The Platform Mesh Authors.
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package generic

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mctrl "sigs.k8s.io/multicluster-runtime"
)

func TestSanitizeClusterName(t *testing.T) {
	t.Parallel()

	t.Run("produces DNS-safe output", func(t *testing.T) {
		t.Parallel()

		// Cluster names can contain special characters
		name := "consumer#/kubeconfigs/consumer/kubeconfig+default"
		result := SanitizeClusterName(name)

		assert.Regexp(t, `^[a-f0-9]{12}$`, result,
			"sanitized name should be 12 hex characters")
	})

	t.Run("deterministic", func(t *testing.T) {
		t.Parallel()

		name := "consumer-team-a"
		assert.Equal(t, SanitizeClusterName(name), SanitizeClusterName(name))
	})

	t.Run("different inputs produce different outputs", func(t *testing.T) {
		t.Parallel()

		assert.NotEqual(t,
			SanitizeClusterName("consumer-team-a"),
			SanitizeClusterName("consumer-team-b"),
		)
	})
}

func TestProviderNamespacedName(t *testing.T) {
	t.Parallel()

	t.Run("prefixes hashed consumer name to resource name", func(t *testing.T) {
		t.Parallel()

		gr := &objectReconcileTask{
			consumerName: "consumer-team-a",
			req: mctrl.Request{
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: "default",
						Name:      "my-cert",
					},
				},
			},
		}

		result := gr.providerNamespacedName()
		expected := SanitizeClusterName("consumer-team-a") + "-my-cert"
		assert.Equal(t, expected, result.Name)
		assert.Equal(t, "default", result.Namespace)
	})

	t.Run("namespace is preserved unchanged", func(t *testing.T) {
		t.Parallel()

		gr := &objectReconcileTask{
			consumerName: "consumer-eu",
			req: mctrl.Request{
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: "prod-ns",
						Name:      "db-backup",
					},
				},
			},
		}

		result := gr.providerNamespacedName()
		assert.Equal(t, "prod-ns", result.Namespace)
	})

	t.Run("uses consumerObjName when set (provider-side request)", func(t *testing.T) {
		t.Parallel()

		prefix := SanitizeClusterName("consumer-team-a")
		gr := &objectReconcileTask{
			consumerName: "consumer-team-a",
			req: mctrl.Request{
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{
						// This is the prefixed name from the provider side
						Namespace: "default",
						Name:      prefix + "-my-cert",
					},
				},
			},
			consumerObjName: &types.NamespacedName{
				Namespace: "default",
				Name:      "my-cert",
			},
		}

		result := gr.providerNamespacedName()
		assert.Equal(t, prefix+"-my-cert", result.Name)
		assert.Equal(t, "default", result.Namespace)
	})

	t.Run("two consumers same resource produce different provider names", func(t *testing.T) {
		t.Parallel()

		nn := types.NamespacedName{Namespace: "default", Name: "my-cert"}

		grA := &objectReconcileTask{
			consumerName: "consumer-team-a",
			req: mctrl.Request{
				Request: reconcile.Request{NamespacedName: nn},
			},
		}
		grB := &objectReconcileTask{
			consumerName: "consumer-team-b",
			req: mctrl.Request{
				Request: reconcile.Request{NamespacedName: nn},
			},
		}

		resultA := grA.providerNamespacedName()
		resultB := grB.providerNamespacedName()

		assert.NotEqual(t, resultA.Name, resultB.Name,
			"different consumers must produce different provider names")
		// Namespace stays the same for both
		assert.Equal(t, resultA.Namespace, resultB.Namespace)
	})

	t.Run("handles cluster names with special characters", func(t *testing.T) {
		t.Parallel()

		gr := &objectReconcileTask{
			consumerName: "consumer#/kubeconfigs/consumer/kubeconfig+default",
			req: mctrl.Request{
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: "default",
						Name:      "my-cert",
					},
				},
			},
		}

		result := gr.providerNamespacedName()
		// Name should be valid DNS: only lowercase hex + dash + original name
		assert.Regexp(t, `^[a-f0-9]{12}-my-cert$`, result.Name)
	})
}

func TestConsumerNamespacedName(t *testing.T) {
	t.Parallel()

	t.Run("returns req.NamespacedName when consumerObjName is nil", func(t *testing.T) {
		t.Parallel()

		gr := &objectReconcileTask{
			req: mctrl.Request{
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: "default",
						Name:      "my-cert",
					},
				},
			},
		}

		result := gr.consumerNamespacedName()
		assert.Equal(t, "my-cert", result.Name)
		assert.Equal(t, "default", result.Namespace)
	})

	t.Run("returns consumerObjName when set", func(t *testing.T) {
		t.Parallel()

		gr := &objectReconcileTask{
			req: mctrl.Request{
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: "default",
						Name:      "abc123-my-cert",
					},
				},
			},
			consumerObjName: &types.NamespacedName{
				Namespace: "default",
				Name:      "my-cert",
			},
		}

		result := gr.consumerNamespacedName()
		assert.Equal(t, "my-cert", result.Name)
		assert.Equal(t, "default", result.Namespace)
	})
}
