// Copyright The Platform Mesh Authors.
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha1

// Status represents the status of a resource.
type Status string

const (
	// StatusUnknown indicates that the resource has not been processed
	// yet.
	StatusUnknown Status = "Unknown"

	// StatusEmpty indicates that the resource has no status.
	StatusEmpty Status = ""

	// StatusProvisioning indicates that the resource is being
	// provisioned by a provider.
	StatusProvisioning Status = "Provisioning"

	// StatusAvailable indicates that the provider has finished
	// provisioning the resource and it is available for use.
	StatusAvailable Status = "Available"

	// StatusDegraded indicates that the resource is in a degraded
	// state.
	StatusDegraded Status = "Degraded"

	// StatusDeleting indicates that the resource is being deleted.
	StatusDeleting Status = "Deleting"

	// StatusFailed indicates that the resource has failed.
	StatusFailed Status = "Failed"
)

// Continue returns true if the status indicates that processing
// can continue.
func (s Status) Continue() bool {
	switch s {
	case StatusEmpty, StatusUnknown, StatusProvisioning:
		// Empty blocks because it was likely just created.
		// Unknown blocks because it has not been processed yet.
		// Provisioning blocks because it is still being provisioned.
		return false
	default:
		// All other states (Available, Degraded, Deleting, Failed)
		// allow processing to continue, as their conditions and
		// possibly related resources may need to be synced.
		return true
	}
}
