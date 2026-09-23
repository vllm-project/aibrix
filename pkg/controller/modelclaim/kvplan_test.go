/*
Copyright 2026 The Aibrix Team.

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

package modelclaim

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plannedEngine is one engine to divide a card among.
func plannedEngine(name string, footprint, floor, used int64) engineOnPod {
	return engineOnPod{
		claimName:       name,
		modelName:       name,
		perGPUBytes:     perGPUBytes{maximumFootprintBytes: footprint, kvFloorBytes: floor},
		kvUsedBytes:     used,
		kvCapacityBytes: kvLimitUnknown,
	}
}

// spentBytes is what a plan hands out in total, footprints included. It must
// come to exactly what the card can hold.
func spentBytes(engines []engineOnPod, limits []plannedKVLimit) int64 {
	spent := int64(0)
	for _, engine := range engines {
		spent += engine.maximumFootprintBytes
	}
	for _, limit := range limits {
		spent += limit.kvLimitBytes
	}
	return spent
}

func TestPlanKVLimitsSpendsTheWholeCard(t *testing.T) {
	engines := []engineOnPod{
		plannedEngine("grown", 200, 100, 250),
		plannedEngine("newcomer", 200, 100, 0),
	}

	limits, err := planKVLimits(1000, engines)

	require.NoError(t, err)
	require.Len(t, limits, 2)
	assert.Equal(t, int64(375), limits[0].kvLimitBytes)
	assert.Equal(t, "grown", limits[0].claimName)
	assert.Equal(t, int64(225), limits[1].kvLimitBytes)
	assert.Equal(t, int64(1000), spentBytes(engines, limits))
}

func TestPlanKVLimitsGivesTheBusierEngineTheLargerShare(t *testing.T) {
	busy := plannedEngine("busy", 200, 100, 100)
	busy.inFlightRequests = 3
	engines := []engineOnPod{busy, plannedEngine("quiet", 200, 100, 100)}

	limits, err := planKVLimits(1000, engines)

	require.NoError(t, err)
	require.Len(t, limits, 2)
	assert.Equal(t, "busy", limits[0].claimName)
	assert.Greater(t, limits[0].kvLimitBytes, limits[1].kvLimitBytes)
	assert.Equal(t, int64(1000), spentBytes(engines, limits))
}

func TestPlanKVLimitsNeverGoesBelowWhatAnEngineHolds(t *testing.T) {
	// The card is exactly full, so there is nothing to share out.
	engines := []engineOnPod{
		plannedEngine("grown", 200, 100, 600),
		plannedEngine("small", 100, 100, 0),
	}

	limits, err := planKVLimits(1000, engines)

	require.NoError(t, err)
	require.Len(t, limits, 2)
	assert.Equal(t, int64(600), limits[0].kvLimitBytes)
	assert.Equal(t, int64(100), limits[1].kvLimitBytes)
}

func TestPlanKVLimitsRefusesACardTheEnginesHaveOutgrown(t *testing.T) {
	engines := []engineOnPod{plannedEngine("grown", 200, 100, 900)}

	limits, err := planKVLimits(1000, engines)

	require.Error(t, err)
	assert.Nil(t, limits)
	assert.Contains(t, err.Error(), "more than it has")
}

func TestPlanKVLimitsRefusesAnEngineThatDeclaresNoCost(t *testing.T) {
	engines := []engineOnPod{plannedEngine("legacy", 0, 0, 0)}

	_, err := planKVLimits(1000, engines)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "legacy declares no per-GPU cost")
}

func TestPlanKVLimitsHandsOutTheRoundingRemainder(t *testing.T) {
	// 1000 less two footprints of 200 and two floors of 100 leaves 400 to
	// share three ways, which does not divide.
	engines := []engineOnPod{
		plannedEngine("a", 200, 100, 0),
		plannedEngine("b", 200, 100, 0),
	}
	engines[0].inFlightRequests = 1

	limits, err := planKVLimits(1000, engines)

	require.NoError(t, err)
	assert.Equal(t, int64(1000), spentBytes(engines, limits))
	assert.Equal(t, int64(367), limits[0].kvLimitBytes)
	assert.Equal(t, int64(233), limits[1].kvLimitBytes)
}

func TestPlanKVLimitsIsTheSameWhateverOrderTheEnginesArriveIn(t *testing.T) {
	forwards := []engineOnPod{
		plannedEngine("a", 200, 100, 0),
		plannedEngine("b", 200, 100, 150),
	}
	backwards := []engineOnPod{forwards[1], forwards[0]}

	first, err := planKVLimits(1000, forwards)
	require.NoError(t, err)
	second, err := planKVLimits(1000, backwards)
	require.NoError(t, err)

	assert.Equal(t, first, second)
}

func TestMinimumKVLimitChangeBytesRisesWithTheCard(t *testing.T) {
	// Small cards use the page-bundle floor.
	assert.Equal(t, int64(512)<<20, minimumKVLimitChangeBytes(16<<30))
	// A large card asks for more before it is worth dividing again.
	assert.Equal(t, int64(80<<30)/100, minimumKVLimitChangeBytes(80<<30))
}

func TestWorthWritingIgnoresADriftSmallerThanTheThreshold(t *testing.T) {
	limits := []plannedKVLimit{
		{claimName: "a", kvLimitBytes: 10 << 30, kvCapacityBytes: 10<<30 - 1<<20},
		{claimName: "b", kvLimitBytes: 20 << 30, kvCapacityBytes: 20<<30 + 1<<20},
	}

	assert.False(t, worthWriting(limits, 512<<20))
	assert.True(t, worthWriting(limits, 1<<20))
}

func TestWorthWritingIgnoresAnEngineWithNoSegmentToWriteInto(t *testing.T) {
	limits := []plannedKVLimit{
		{claimName: "booting", kvLimitBytes: 40 << 30, kvCapacityBytes: kvLimitUnknown},
	}

	assert.False(t, worthWriting(limits, 512<<20))
}

func TestWriteOrderShrinksBeforeItGrows(t *testing.T) {
	limits := []plannedKVLimit{
		{claimName: "grows", kvLimitBytes: 40, kvCapacityBytes: 10},
		{claimName: "unchanged", kvLimitBytes: 20, kvCapacityBytes: 20},
		{claimName: "shrinks", kvLimitBytes: 5, kvCapacityBytes: 30},
		{claimName: "booting", kvLimitBytes: 15, kvCapacityBytes: kvLimitUnknown},
	}

	written := writeOrder(limits)

	require.Len(t, written, 2)
	assert.Equal(t, "shrinks", written[0].claimName)
	assert.Equal(t, "grows", written[1].claimName)
}

func TestShrinksAndGrowsSplitsTheWritesIntoTwoSteps(t *testing.T) {
	limits := []plannedKVLimit{
		{claimName: "grows", kvLimitBytes: 40, kvCapacityBytes: 10},
		{claimName: "unchanged", kvLimitBytes: 20, kvCapacityBytes: 20},
		{claimName: "shrinks", kvLimitBytes: 5, kvCapacityBytes: 30},
		{claimName: "booting", kvLimitBytes: 15, kvCapacityBytes: kvLimitUnknown},
	}

	shrinks, grows := shrinksAndGrows(limits)

	require.Len(t, shrinks, 1)
	assert.Equal(t, "shrinks", shrinks[0].claimName)
	require.Len(t, grows, 1)
	assert.Equal(t, "grows", grows[0].claimName)
}
