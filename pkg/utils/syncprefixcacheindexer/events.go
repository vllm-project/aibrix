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

package syncprefixcacheindexer

// BlockStored represents an event when blocks are stored
type BlockStored struct {
	// BlockHashes contains the engine block hashes that were stored
	BlockHashes []int64

	// ParentBlockHash is the optional parent block hash
	// nil means this is the first block in the sequence
	ParentBlockHash *int64

	// Tokens contains the token data for each block
	// The length should match BlockHashes
	Tokens [][]byte

	// Context information
	ModelName string
	LoraID    int64 // -1 for no adapter

	// Source pod that stored these blocks
	SourcePod string
}

// BlockRemoved represents an event when blocks are removed
type BlockRemoved struct {
	// BlockHashes contains the engine block hashes that were removed
	BlockHashes []int64

	// Context information
	ModelName string
	LoraID    int64 // -1 for no adapter

	// Source pod that removed these blocks (optional)
	SourcePod string
}

// AllBlocksCleared represents an event when all blocks are cleared
// This is currently not implemented as per requirements
type AllBlocksCleared struct {
	// Placeholder for future implementation
}
