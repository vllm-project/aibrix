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

package hash

import (
	"fmt"
	"hash"

	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/rand"
)

// DeepHashObject writes specified object to hash using the spew library
// which follows pointers and prints actual values of the nested objects
// ensuring the hash does not change when a pointer changes.
func DeepHashObject(hasher hash.Hash, objectToWrite interface{}) {
	hasher.Reset()
	fmt.Fprintf(hasher, "%v", dump.ForHash(objectToWrite))
}

// ShortSafeEncodeString returns a shortened (6-char) DNS-safe hash to mitigate 63-char name limits.
// It ensures a consistent 6-character output by padding if needed, maintaining DNS compliance.
func ShortSafeEncodeString(hashValue uint32) string {
	hash := rand.SafeEncodeString(fmt.Sprint(hashValue))
	if len(hash) >= 6 {
		return hash[:6]
	}
	// Pad with leading zeros if shorter than 6 chars
	return fmt.Sprintf("%06s", hash)
}
