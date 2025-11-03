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

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricHistory(t *testing.T) {
	t.Run("add metric points", func(t *testing.T) {
		mh := NewMetricHistory(time.Second * 30)

		mh.Add(20.0, time.Now())
		assert.Len(t, mh.history, 1)
		assert.Equal(t, 20.0, mh.history[0].Value)

		mh.Add(30.0, time.Now().Add(time.Second*10))
		assert.Len(t, mh.history, 2)
		assert.Equal(t, 20.0, mh.history[0].Value)
		assert.Equal(t, 30.0, mh.history[1].Value)

		mh.Add(40.0, time.Now().Add(time.Second*20))
		assert.Len(t, mh.history, 3)
		assert.Equal(t, 20.0, mh.history[0].Value)
		assert.Equal(t, 30.0, mh.history[1].Value)
		assert.Equal(t, 40.0, mh.history[2].Value)

		mh.Add(50.0, time.Now().Add(time.Second*30))
		assert.Len(t, mh.history, 3)
		assert.Equal(t, 30.0, mh.history[0].Value)
		assert.Equal(t, 40.0, mh.history[1].Value)
		assert.Equal(t, 50.0, mh.history[2].Value)
	})

	t.Run("get stats", func(t *testing.T) {
		mh := NewMetricHistory(time.Second * 30)

		start := time.Now()
		mh.Add(20.0, start)
		require.Len(t, mh.history, 1)
		mh.Add(30.0, start.Add(time.Second*10))
		require.Len(t, mh.history, 2)
		mh.Add(40.0, start.Add(time.Second*20))
		require.Len(t, mh.history, 3)

		stats := mh.GetStats(start.Add(time.Second * 20))

		assert.NotNil(t, stats)
		assert.Equal(t, 3, stats.DataPoints)
		assert.Equal(t, 66.67, math.Round(stats.Variance*100)/100)
		assert.Equal(t, 8.16, math.Round(stats.StdDev*100)/100)
		assert.Equal(t, 20.0, stats.Min)
		assert.Equal(t, 40.0, stats.Max)
		assert.Equal(t, start, stats.WindowStart)
		assert.Equal(t, start.Add(time.Second*20), stats.WindowEnd)
		assert.Equal(t, start.Add(time.Second*20), stats.LastUpdate)
	})
}

func TestTimeWindow(t *testing.T) {
	t.Run("record metrics tracks one value for given granuality and truncate value beyond window duration", func(t *testing.T) {
		tw := NewTimeWindow(time.Minute*2, time.Minute)

		start := time.Now().Truncate(time.Minute) // initialize start with current time having seconds=0

		tw.Record(start, 10.0)
		assert.Equal(t, []float64{10}, tw.values)
		tw.Record(start.Add(time.Second*30), 20.0)
		assert.Equal(t, []float64{20}, tw.values) // kept only value (i.e. 20) for given minute granuaity

		tw.Record(start.Add(time.Second*60), 30.0)
		assert.Equal(t, []float64{20, 30}, tw.values)
		tw.Record(start.Add(time.Second*90), 40.0)
		assert.Equal(t, []float64{20, 40}, tw.values)

		tw.Record(start.Add(time.Second*120), 50.0)
		assert.Equal(t, []float64{20, 40, 50}, tw.values)
		tw.Record(start.Add(time.Second*150), 60.0)
		assert.Equal(t, []float64{20, 40, 60}, tw.values)

		tw.Record(start.Add(time.Second*180), 70.0)
		assert.Equal(t, []float64{40, 60, 70}, tw.values) // truncated old values (i.e. 20) beyond window duration
		tw.Record(start.Add(time.Second*210), 80.0)
		assert.Equal(t, []float64{40, 60, 80}, tw.values)
	})

	t.Run("size avg min max", func(t *testing.T) {
		tw := NewTimeWindow(time.Minute*2, time.Minute)

		start := time.Now().Truncate(time.Minute) // initialize start with current time having seconds=0

		tw.Record(start, 20.0)
		tw.Record(start.Add(time.Minute*1), 30.0)
		tw.Record(start.Add(time.Minute*2), 40.0)

		assert.Equal(t, 3, tw.Size())
		assert.Equal(t, []float64{20, 30, 40}, tw.Values())
		actualAvg, _ := tw.Avg()
		assert.Equal(t, 30.0, actualAvg)
		actualMin, _ := tw.Min()
		assert.Equal(t, 20.0, actualMin)
		actualMax, _ := tw.Max()
		assert.Equal(t, 40.0, actualMax)
	})
}
