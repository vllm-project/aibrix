/*
Copyright 2024 The Aibrix Team.

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

package tokenizer

import "fmt"

// ErrInvalidConfig represents a configuration validation error
type ErrInvalidConfig struct {
	Message string
}

func (e ErrInvalidConfig) Error() string {
	return "invalid configuration: " + e.Message
}

// ErrTokenizationFailed represents a tokenization failure
type ErrTokenizationFailed struct {
	Message string
	Cause   error
}

func (e ErrTokenizationFailed) Error() string {
	msg := "tokenization failed: " + e.Message
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap returns the underlying error cause
func (e ErrTokenizationFailed) Unwrap() error {
	return e.Cause
}

// ErrDetokenizationFailed represents a detokenization failure
type ErrDetokenizationFailed struct {
	Message string
	Cause   error
}

func (e ErrDetokenizationFailed) Error() string {
	msg := "detokenization failed: " + e.Message
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap returns the underlying error cause
func (e ErrDetokenizationFailed) Unwrap() error {
	return e.Cause
}

// ErrUnsupportedOperation represents an error when an operation is not supported by the engine
type ErrUnsupportedOperation struct {
	Engine    string
	Operation string
}

func (e ErrUnsupportedOperation) Error() string {
	return fmt.Sprintf("operation %s is not supported by engine %s", e.Operation, e.Engine)
}

// ErrHTTPRequest represents an HTTP request error
type ErrHTTPRequest struct {
	StatusCode int
	Message    string
	Body       string
}

func (e ErrHTTPRequest) Error() string {
	msg := fmt.Sprintf("HTTP request failed with status %d", e.StatusCode)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}
