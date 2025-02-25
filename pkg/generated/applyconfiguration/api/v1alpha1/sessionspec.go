/*
Copyright The Kubernetes Authors.

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

// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1alpha1

// SessionSpecApplyConfiguration represents a declarative configuration of the SessionSpec type for use
// with apply.
type SessionSpecApplyConfiguration struct {
	UserReference    *UserReferenceApplyConfiguration    `json:"userReference,omitempty"`
	GameReference    *GameReferenceApplyConfiguration    `json:"gameReference,omitempty"`
	PairingReference *PairingReferenceApplyConfiguration `json:"pairingReference,omitempty"`
	GatewayReference *GatewayReferenceApplyConfiguration `json:"gateway,omitempty"`
	Config           *SessionInfoApplyConfiguration      `json:"config,omitempty"`
}

// SessionSpecApplyConfiguration constructs a declarative configuration of the SessionSpec type for use with
// apply.
func SessionSpec() *SessionSpecApplyConfiguration {
	return &SessionSpecApplyConfiguration{}
}

// WithUserReference sets the UserReference field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the UserReference field is set to the value of the last call.
func (b *SessionSpecApplyConfiguration) WithUserReference(value *UserReferenceApplyConfiguration) *SessionSpecApplyConfiguration {
	b.UserReference = value
	return b
}

// WithGameReference sets the GameReference field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the GameReference field is set to the value of the last call.
func (b *SessionSpecApplyConfiguration) WithGameReference(value *GameReferenceApplyConfiguration) *SessionSpecApplyConfiguration {
	b.GameReference = value
	return b
}

// WithPairingReference sets the PairingReference field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the PairingReference field is set to the value of the last call.
func (b *SessionSpecApplyConfiguration) WithPairingReference(value *PairingReferenceApplyConfiguration) *SessionSpecApplyConfiguration {
	b.PairingReference = value
	return b
}

// WithGatewayReference sets the GatewayReference field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the GatewayReference field is set to the value of the last call.
func (b *SessionSpecApplyConfiguration) WithGatewayReference(value *GatewayReferenceApplyConfiguration) *SessionSpecApplyConfiguration {
	b.GatewayReference = value
	return b
}

// WithConfig sets the Config field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Config field is set to the value of the last call.
func (b *SessionSpecApplyConfiguration) WithConfig(value *SessionInfoApplyConfiguration) *SessionSpecApplyConfiguration {
	b.Config = value
	return b
}
