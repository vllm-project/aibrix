/*
Copyright 2025 The Aibrix Team.

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

package types

import "fmt"

var (
	ErrInvalidArgs    = fmt.Errorf("invalid args")
	ErrNotImplemented = fmt.Errorf("not implemented")
)

// ProvisionerError represents a provisioning error.
type ProvisionerError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Retry   bool   `json:"retry,omitempty"` // Whether the operation can be retried
}

func (e *ProvisionerError) Error() string {
	return e.Message
}

// IsRetryable returns true if the error can be retried.
func (e *ProvisionerError) IsRetryable() bool {
	return e.Retry
}

// Common errors for provisioning operations.
var (
	// ErrUnsupportedProvisioner is returned when a provisioner type is not supported.
	ErrUnsupportedProvisioner = &ProvisionerError{Message: "unsupported provisioner type"}

	ErrProvisionNotFound = &ProvisionerError{Message: "provision not found"}

	// ErrQuotaExceeded is returned when quota limits are exceeded.
	ErrQuotaExceeded = &ProvisionerError{Message: "quota exceeded"}
)

type CatalogError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Retry   bool   `json:"retry,omitempty"` // Whether the operation can be retried
}

func (e *CatalogError) Error() string {
	return e.Message
}

// IsRetryable returns true if the error can be retried.
func (e *CatalogError) IsRetryable() bool {
	return e.Retry
}

// Common errors for catalog operations.
var (
	// ErrUnsupportedCatalog is returned when a catalog type is not supported.
	ErrUnsupportedCatalog = &CatalogError{Message: "unsupported catalog type"}
)

type ClientsetError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Retry   bool   `json:"retry,omitempty"` // Whether the operation can be retried
}

func (e *ClientsetError) Error() string {
	return e.Message
}

// IsRetryable returns true if the error can be retried.
func (e *ClientsetError) IsRetryable() bool {
	return e.Retry
}

var (
	// ErrCredentialIsNil is returned when credential is nil.
	ErrCredentialIsNil = &ClientsetError{Message: "credential is nil"}
	// ErrInvalidCredential is returned when credentials are invalid.
	ErrInvalidCredential = &ClientsetError{Message: "invalid credential"}
)
