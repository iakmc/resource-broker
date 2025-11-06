/*
Copyright 2025.
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

package e2e

import (
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/cluster"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mctrl "sigs.k8s.io/multicluster-runtime"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	"sigs.k8s.io/multicluster-runtime/providers/single"

	examplev1alpha1 "github.com/platform-mesh/resource-broker/api/example/v1alpha1"
	"github.com/platform-mesh/resource-broker/cmd/manager"
	"github.com/platform-mesh/resource-broker/test/utils"
)

func init() {
	log.SetLogger(zap.New(zap.UseDevMode(true)))
	runtime.Must(examplev1alpha1.AddToScheme(scheme.Scheme))
}

// ManagerOptions returns the default manager options for tests.
func ManagerOptions() mctrl.Options {
	return mctrl.Options{
		LeaderElectionID: rand.Text(),
		Metrics: metricsserver.Options{
			BindAddress: "0", // disable metrics
		},
		Controller: ctrlconfig.Controller{
			SkipNameValidation: ptr.To(true), // skip name validation of controller metrics for tests
		},
	}
}

type Frame struct {
	Coordination *ControlPlane
	Compute      *ControlPlane
	Consumer     *ControlPlane
	Provider     *ControlPlane
}

func NewFrame(tb testing.TB) *Frame {
	tb.Helper()

	f := new(Frame)
	f.Coordination = startControlPlane(tb)
	f.Compute = startControlPlane(tb)
	f.Consumer = startControlPlane(tb)
	f.Provider = startControlPlane(tb)
	return f
}

func (f *Frame) Options(tb testing.TB) manager.Options {
	return manager.Options{
		Name:         tb.Name(),
		Local:        f.Coordination.Config,
		Compute:      f.Compute.Cluster.GetClient(),
		Coordination: f.Coordination.Provider(),
		Consumer:     f.Consumer.Provider(),
		Provider:     f.Provider.Provider(),
		MgrOptions:   ManagerOptions(),
	}
}

type ControlPlane struct {
	Env     *envtest.Environment
	Config  *rest.Config
	Cluster cluster.Cluster
}

func startControlPlane(tb testing.TB) *ControlPlane {
	tb.Helper()

	c := &ControlPlane{}
	c.Env, c.Config = utils.DefaultEnvTest(tb)

	var err error
	c.Cluster, err = cluster.New(c.Config)
	require.NoError(tb, err)

	go func() {
		assert.NoError(tb, c.Cluster.Start(tb.Context()))
	}()

	return c
}

func (c *ControlPlane) Provider() multicluster.Provider {
	return single.New("cluster", c.Cluster)
}
