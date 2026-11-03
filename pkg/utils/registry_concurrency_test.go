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

package utils

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

type registryTestArray struct {
	values []string
}

func TestCustomizedRegistryStorePublishesOneSnapshot(t *testing.T) {
	reg := NewRegistryWithArrayProvider[string, *registryTestArray](func(values []string) *registryTestArray {
		return &registryTestArray{values: values}
	})
	reg.Store("first", "first")
	reg.Array()

	const (
		iterations = 20000
		readers    = 4
	)
	var (
		phase    atomic.Uint64
		mismatch atomic.Bool
		wg       sync.WaitGroup
	)
	stop := make(chan struct{})

	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}

				generation := phase.Load()
				if generation%2 == 0 {
					runtime.Gosched()
					continue
				}

				innerLen := reg.Len()
				outerLen := len(reg.Array().values)
				if phase.Load() == generation && outerLen < innerLen {
					mismatch.Store(true)
					return
				}
			}
		}()
	}

	for range iterations {
		reg.Delete("second")
		reg.Array()

		phase.Add(1)
		reg.Store("second", "second")
		phase.Add(1)
		if mismatch.Load() {
			break
		}
	}

	close(stop)
	wg.Wait()
	if mismatch.Load() {
		t.Fatal("Len observed the stored value before the customized Array snapshot was invalidated")
	}
}

func TestCustomizedRegistryDeletePublishesOneSnapshot(t *testing.T) {
	reg := NewRegistryWithArrayProvider[string, *registryTestArray](func(values []string) *registryTestArray {
		return &registryTestArray{values: values}
	})
	reg.Store("first", "first")
	reg.Store("second", "second")
	reg.Array()

	const (
		iterations = 20000
		readers    = 4
	)
	var (
		phase    atomic.Uint64
		mismatch atomic.Bool
		wg       sync.WaitGroup
	)
	stop := make(chan struct{})

	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}

				generation := phase.Load()
				if generation%2 == 0 {
					runtime.Gosched()
					continue
				}

				innerLen := reg.Len()
				outerLen := len(reg.Array().values)
				if phase.Load() == generation && outerLen > innerLen {
					mismatch.Store(true)
					return
				}
			}
		}()
	}

	for range iterations {
		phase.Add(1)
		reg.Delete("second")
		phase.Add(1)
		if mismatch.Load() {
			break
		}

		reg.Store("second", "second")
		reg.Array()
	}

	close(stop)
	wg.Wait()
	if mismatch.Load() {
		t.Fatal("Len observed the deleted value disappear before the customized Array snapshot was invalidated")
	}
}
