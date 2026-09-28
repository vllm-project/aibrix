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
	"sort"
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

func TestPlanKVLimitsKeepsASleepingEngineAtWhatItHolds(t *testing.T) {
	asleep := plannedEngine("asleep", 200, 100, 0)
	asleep.asleep = true
	engines := []engineOnPod{plannedEngine("awake", 200, 100, 0), asleep}

	limits, err := planKVLimits(1000, engines)

	require.NoError(t, err)
	require.Len(t, limits, 2)
	// The 400 left once both footprints and both floors are paid for goes to
	// the engine that is awake.
	assert.Equal(t, "asleep", limits[0].claimName)
	assert.Equal(t, int64(100), limits[0].kvLimitBytes)
	assert.Equal(t, int64(500), limits[1].kvLimitBytes)
	assert.Equal(t, int64(1000), spentBytes(engines, limits))
}

func TestPlanKVLimitsGivesNoRoundingRemainderToASleepingEngine(t *testing.T) {
	asleep := plannedEngine("a", 200, 100, 0)
	asleep.asleep = true
	engines := []engineOnPod{asleep, plannedEngine("b", 100, 100, 0), plannedEngine("c", 100, 100, 0)}

	// 1001 less 400 of footprints and 300 of floors leaves 301, which does not
	// split evenly between the two engines awake.
	limits, err := planKVLimits(1001, engines)

	require.NoError(t, err)
	assert.Equal(t, int64(100), limits[0].kvLimitBytes)
	assert.Equal(t, int64(251), limits[1].kvLimitBytes)
	assert.Equal(t, int64(250), limits[2].kvLimitBytes)
	assert.Equal(t, int64(1001), spentBytes(engines, limits))
}

func TestPlanKVLimitsLeavesTheSpareWhenEveryEngineIsAsleep(t *testing.T) {
	first := plannedEngine("first", 200, 100, 0)
	first.asleep = true
	second := plannedEngine("second", 200, 100, 150)
	second.asleep = true

	limits, err := planKVLimits(1000, []engineOnPod{first, second})

	require.NoError(t, err)
	require.Len(t, limits, 2)
	assert.Equal(t, int64(100), limits[0].kvLimitBytes)
	assert.Equal(t, int64(150), limits[1].kvLimitBytes, "an engine never goes below what it holds")
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

func TestShortOfKV(t *testing.T) {
	held := func(usedBytes, limitBytes int64) engineOnPod {
		return engineOnPod{kvUsedBytes: usedBytes, kvCapacityBytes: limitBytes}
	}
	with := func(engine engineOnPod, change func(*engineOnPod)) engineOnPod {
		change(&engine)
		return engine
	}
	waiting := func(e *engineOnPod) { e.requestsWaiting = 1 }
	unread := func(e *engineOnPod) { e.demandUnknown = true }
	asleep := func(e *engineOnPod) { e.asleep = true }

	for name, c := range map[string]struct {
		engine engineOnPod
		short  bool
	}{
		"most of its limit unmapped":         {held(49, 100), false},
		"half of its limit mapped":           {held(50, 100), true},
		"a byte under half of an odd limit":  {held(50, 101), false},
		"half of an odd limit, rounded up":   {held(51, 101), true},
		"at its limit":                       {held(100, 100), true},
		"requests waiting":                   {with(held(1, 100), waiting), true},
		"its load could not be read":         {with(held(1, 100), unread), true},
		"asleep at its limit":                {with(held(100, 100), asleep), false},
		"asleep, with a request waiting":     {with(with(held(1, 100), asleep), waiting), false},
		"no segment yet":                     {held(0, kvLimitUnknown), false},
		"no segment, with requests waiting":  {with(held(0, kvLimitUnknown), waiting), false},
		"a limit of zero, its load not read": {with(held(0, 0), unread), false},
	} {
		assert.Equal(t, c.short, shortOfKV(c.engine), name)
	}
}

func TestNeedsDividing(t *testing.T) {
	const threshold = 10
	// engine is held to limit, records the same, and has mapped a tenth of it.
	engine := func(name string, limit int64) engineOnPod {
		return engineOnPod{claimName: name, kvUsedBytes: limit / 10, kvCapacityBytes: limit, kvRecordedBytes: limit}
	}
	serving := func(e engineOnPod) engineOnPod {
		e.inFlightRequests = 1
		return e
	}
	short := func(e engineOnPod) engineOnPod {
		e.kvUsedBytes = e.kvCapacityBytes
		return serving(e)
	}
	plan := func(engines []engineOnPod, planned ...int64) []plannedKVLimit {
		limits := make([]plannedKVLimit, len(engines))
		for i, e := range engines {
			limits[i] = plannedKVLimit{
				claimName: e.claimName, kvLimitBytes: planned[i],
				kvCapacityBytes: e.kvCapacityBytes, kvRecordedBytes: e.kvRecordedBytes,
			}
		}
		return limits
	}
	belowItsRecord := engine("a", 100)
	belowItsRecord.kvRecordedBytes = 150
	aboveItsRecord := engine("a", 100)
	aboveItsRecord.kvRecordedBytes = 50
	unrecorded := engine("a", 100)
	unrecorded.kvRecordedBytes = 0
	noSegment := engine("a", kvLimitUnknown)
	noSegment.kvRecordedBytes = 100
	noSegment.kvUsedBytes = 0
	asleep := engine("b", 40)
	asleep.asleep = true
	asleepWithARequest := serving(asleep)
	unread := engine("b", 100)
	unread.demandUnknown = true
	unread.kvCapacityBytes = kvLimitUnknown

	for name, c := range map[string]struct {
		engines []engineOnPod
		planned []int64
		needed  bool
	}{
		"nobody short, and the plan moves with the load": {
			[]engineOnPod{serving(engine("a", 100)), serving(engine("b", 100))}, []int64{150, 50}, false},
		"more for an engine that is short": {
			[]engineOnPod{short(engine("a", 100)), serving(engine("b", 100))}, []int64{110, 90}, true},
		"a little more for an engine that is short": {
			[]engineOnPod{short(engine("a", 100)), serving(engine("b", 100))}, []int64{109, 91}, false},
		"no more for an engine that is short": {
			[]engineOnPod{short(engine("a", 100)), serving(engine("b", 100))}, []int64{100, 100}, false},
		"less for an engine that is short": {
			[]engineOnPod{short(engine("a", 100)), serving(engine("b", 100))}, []int64{50, 150}, false},
		"more for the engine beside the one that is short": {
			[]engineOnPod{short(engine("a", 100)), serving(engine("b", 100))}, []int64{90, 110}, false},
		"held below its record": {
			[]engineOnPod{serving(belowItsRecord), serving(engine("b", 100))}, []int64{100, 100}, true},
		"held above its record": {
			[]engineOnPod{serving(aboveItsRecord), serving(engine("b", 100))}, []int64{100, 100}, true},
		// An instance that records nothing records zero. Its engine has a
		// segment, so it is held to a limit that nothing records.
		"a limit in force and no record": {
			[]engineOnPod{serving(unrecorded), serving(engine("b", 100))}, []int64{100, 100}, true},
		"no segment to be held in": {
			[]engineOnPod{serving(noSegment), serving(engine("b", 100))}, []int64{100, 100}, false},
		"at rest, an engine under half of its share": {
			[]engineOnPod{engine("a", 151), engine("b", 49)}, []int64{100, 100}, true},
		"at rest, an engine at half of its share": {
			[]engineOnPod{engine("a", 150), engine("b", 50)}, []int64{100, 100}, false},
		"at rest, an engine at half of an odd share, rounded up": {
			[]engineOnPod{engine("a", 150), engine("b", 50)}, []int64{101, 99}, false},
		"at rest, an engine a byte under half of an odd share": {
			[]engineOnPod{engine("a", 150), engine("b", 50)}, []int64{99, 101}, true},
		"an engine under half of its share, and a request in flight": {
			[]engineOnPod{serving(engine("a", 151)), engine("b", 49)}, []int64{100, 100}, false},
		"an engine under half of its share, and a load that could not be read": {
			[]engineOnPod{unread, engine("a", 49)}, []int64{100, 100}, false},
		"at rest, an engine with no segment to be held in": {
			[]engineOnPod{engine("b", 100), noSegment}, []int64{100, 100}, false},
		"at rest beside an engine that is asleep": {
			[]engineOnPod{engine("a", 49), asleepWithARequest}, []int64{160, 40}, true},
		"every engine asleep": {
			[]engineOnPod{asleep}, []int64{40}, false},
		"no engine": {nil, nil, false},
	} {
		// The plan lists the engines by claim name, whatever order they are
		// given in.
		limits := plan(c.engines, c.planned...)
		sort.Slice(limits, func(i, j int) bool { return limits[i].claimName < limits[j].claimName })
		assert.Equal(t, c.needed, needsDividing(c.engines, limits, threshold), name)
	}

	// With no threshold, more still means more.
	atItsShare := []engineOnPod{short(engine("a", 100)), serving(engine("b", 100))}
	assert.False(t, needsDividing(atItsShare, plan(atItsShare, 100, 100), 0))
	assert.True(t, needsDividing(atItsShare, plan(atItsShare, 101, 99), 0))
}
