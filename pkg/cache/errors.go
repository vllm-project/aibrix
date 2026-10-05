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
package cache

import "errors"

var (
	ErrorTypeMetricNotFound = &CacheError{error: errors.New("metric not found")}
	ErrorMissingProfile     = &CacheError{error: errors.New("missing profile")}
)

// Error support error type detection and structured error info.
type Error interface {
	// ErrorType returns the type of the error.
	ErrorType() error
}

func IsError(err error, errCategory error) bool {
	switch e := err.(type) {
	case Error:
		return e.ErrorType() == errCategory
	default:
		return err == errCategory
	}
}

type CacheError struct {
	error
}

func (e *CacheError) ErrorType() error {
	return e
}

type MetricNotFoundError struct {
	*CacheError
	PodName    string
	MetricName string
}

type MissingProfileError struct {
	*CacheError
	ProfileKey string
}
