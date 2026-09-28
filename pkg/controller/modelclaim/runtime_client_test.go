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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// clientForServer returns the default runtime client plus the host/port of a
// test server, exercising the real HTTP path against a mock runtime.
func clientForServer(srv *httptest.Server) (RuntimeClient, string, int) {
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return NewRuntimeClient(), u.Hostname(), port
}

func TestHTTPRuntimeActivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/models/activate", r.URL.Path)
		var req ActivateRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "m1", req.ModelName)
		assert.Equal(t, "kvc_m1", req.IPCName)
		require.NotNil(t, req.ClaimRef)
		assert.Equal(t, "claim-uid", req.ClaimRef.UID)
		require.NotNil(t, req.EngineConfig)
		assert.Equal(t, "2048", req.EngineConfig.Args["--max-model-len"])
		_ = json.NewEncoder(w).Encode(ActivateResponse{
			Status: "success", ModelName: req.ModelName, Port: 9123, IPCName: req.IPCName,
		})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	resp, err := c.Activate(context.Background(), host, port, &ActivateRequest{
		ModelName: "m1",
		IPCName:   "kvc_m1",
		ClaimRef:  &ModelClaimRef{Namespace: "default", Name: "m1", UID: "claim-uid"},
		EngineConfig: &modelv1alpha1.ModelClaimEngineConfig{
			Args: map[string]string{"--max-model-len": "2048"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(9123), resp.Port)
	assert.Equal(t, "kvc_m1", resp.IPCName)
}

// hangingRuntime stands in for a runtime that takes each request and answers
// none of them, as a runtime whose snapshot handler is stuck would. Once
// answering is set, it answers again. With refusing set, it takes no
// connection. With stalling set, it sends its headers and no body.
//
// It is the transport of the client that calls it. So it counts a request the
// moment the client sends it, and no test waits for a request to reach a
// handler. A call to it ends when its caller gives up.
type hangingRuntime struct {
	host      string
	port      int
	requests  atomic.Int32
	answering atomic.Bool
	refusing  atomic.Bool
	stalling  atomic.Bool
	// deadline is the deadline of the last request, if it had one.
	deadline atomic.Pointer[time.Time]
}

func (r *hangingRuntime) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests.Add(1)
	r.deadline.Store(nil)
	if deadline, has := req.Context().Deadline(); has {
		r.deadline.Store(&deadline)
	}
	answer := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    req,
	}
	switch {
	case r.answering.Load():
		return answer, nil
	case r.refusing.Load():
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	case r.stalling.Load():
		answer.Body = stalledBody{req.Context()}
		return answer, nil
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// stalledBody is the body of an answer that never comes. A read of it ends
// when the caller of the request gives up.
type stalledBody struct {
	request context.Context
}

func (b stalledBody) Read([]byte) (int, error) {
	<-b.request.Done()
	return 0, b.request.Err()
}

func (stalledBody) Close() error { return nil }

// address is the runtime as the client remembers it.
func (r *hangingRuntime) address() string {
	return net.JoinHostPort(r.host, strconv.Itoa(r.port))
}

// asked checks how many requests the client has sent to the runtime.
func (r *hangingRuntime) asked(t *testing.T, requests int32) {
	t.Helper()
	assert.Equal(t, requests, r.requests.Load())
}

// runtimesByAddress sends each request of a client to the runtime it is for.
type runtimesByAddress map[string]*hangingRuntime

func (runtimes runtimesByAddress) RoundTrip(req *http.Request) (*http.Response, error) {
	runtime, found := runtimes[req.URL.Host]
	if !found {
		return nil, fmt.Errorf("no runtime at %s", req.URL.Host)
	}
	return runtime.RoundTrip(req)
}

// shortDeadline is the deadline of a call that is to time out in a test.
const shortDeadline = 20 * time.Millisecond

// hangingRuntimes returns count runtimes that hang, and a client that reads
// the clock given and sends its requests to them. A read of the client gives
// up after shortDeadline.
func hangingRuntimes(count int, now func() time.Time) (*httpRuntimeClient, []*hangingRuntime) {
	c := newHTTPRuntimeClient(shortDeadline, now)
	byAddress := runtimesByAddress{}
	runtimes := make([]*hangingRuntime, count)
	for i := range runtimes {
		runtimes[i] = &hangingRuntime{host: fmt.Sprintf("10.0.0.%d", i+1), port: DefaultRuntimePort}
		byAddress[runtimes[i].address()] = runtimes[i]
	}
	c.httpClient.Transport = byAddress
	return c, runtimes
}

// hangingServer is a runtime behind a real socket that answers no request
// until the test ends.
func hangingServer(t *testing.T) (host string, port int) {
	t.Helper()
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-released }))
	t.Cleanup(func() {
		close(released)
		srv.Close()
	})
	u, _ := url.Parse(srv.URL)
	port, _ = strconv.Atoi(u.Port())
	return u.Hostname(), port
}

// Over a real socket: a read gives up at its deadline, and what the HTTP
// client returns then is taken for a timeout.
func TestHTTPRuntimeSnapshotGivesUpOnARuntimeThatDoesNotAnswer(t *testing.T) {
	host, port := hangingServer(t)
	c := newHTTPRuntimeClient(100*time.Millisecond, time.Now)

	start := time.Now()
	_, err := c.Snapshot(context.Background(), host, port)

	require.Error(t, err)
	require.NotErrorIs(t, err, errRuntimeSilent)
	assert.Less(t, time.Since(start), 5*time.Second)
	_, err = c.Snapshot(context.Background(), host, port)
	assert.ErrorIs(t, err, errRuntimeSilent)
}

// Over a real socket: a call that changes state gives up at the timeout of
// the HTTP client, which is taken for a timeout as well.
func TestHTTPRuntimeActivateGivesUpOnARuntimeThatDoesNotAnswer(t *testing.T) {
	host, port := hangingServer(t)
	c := newHTTPRuntimeClient(5*time.Second, time.Now)
	c.httpClient.Timeout = 100 * time.Millisecond

	_, err := c.Activate(context.Background(), host, port, &ActivateRequest{ModelName: "m1"})

	require.Error(t, err)
	require.NotErrorIs(t, err, errRuntimeSilent)
	_, err = c.Snapshot(context.Background(), host, port)
	assert.ErrorIs(t, err, errRuntimeSilent)
}

// The docs and the comments quote these figures, and the client that runs in
// production is built from them.
func TestTheRuntimeClientIsBuiltWithItsDeadlines(t *testing.T) {
	assert.Equal(t, 10*time.Second, runtimeSnapshotTimeout)
	assert.Equal(t, 60*time.Second, defaultRuntimeHTTPTimeout)
	assert.Equal(t, 10*time.Second, shortestRuntimeSilence)
	assert.Equal(t, time.Minute, runtimeSilenceWindow)

	c := NewRuntimeClient().(*httpRuntimeClient)
	assert.Equal(t, runtimeSnapshotTimeout, c.snapshotTimeout)
	assert.Equal(t, defaultRuntimeHTTPTimeout, c.httpClient.Timeout)
	assert.Equal(t, shortestRuntimeSilence, c.silence.shortest)
	assert.Equal(t, runtimeSilenceWindow, c.silence.longest)
}

func TestHTTPRuntimeLeavesARuntimeThatDidNotAnswerAloneForAWhile(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	now := start
	c, runtimes := hangingRuntimes(1, func() time.Time { return now })
	runtime := runtimes[0]
	ctx := context.Background()

	_, err := c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)
	require.NotErrorIs(t, err, errRuntimeSilent)
	runtime.asked(t, 1)

	// Until the round is over, every call to it fails at once, to its very
	// end.
	for _, into := range []time.Duration{0, shortestRuntimeSilence / 2, shortestRuntimeSilence - time.Nanosecond} {
		now = start.Add(into)
		_, err = c.Snapshot(ctx, runtime.host, runtime.port)
		assert.ErrorIs(t, err, errRuntimeSilent, into)
		_, err = c.Activate(ctx, runtime.host, runtime.port, &ActivateRequest{ModelName: "m1"})
		assert.ErrorIs(t, err, errRuntimeSilent, into)
	}
	runtime.asked(t, 1)

	// After that, it is called again, and once it answers, it is called as usual.
	runtime.answering.Store(true)
	now = start.Add(shortestRuntimeSilence)
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	require.NoError(t, err)
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	require.NoError(t, err)
	runtime.asked(t, 3)
}

// A runtime that was slow once is read again a round later. One that stays
// silent is left alone twice as long after each timeout, up to a minute.
func TestHTTPRuntimeLeavesARuntimeAloneLongerAfterEachTimeout(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, runtimes := hangingRuntimes(1, func() time.Time { return now })
	runtime := runtimes[0]
	ctx := context.Background()

	asked := int32(0)
	for _, alone := range []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute,
	} {
		_, err := c.Snapshot(ctx, runtime.host, runtime.port)
		require.Error(t, err)
		require.NotErrorIs(t, err, errRuntimeSilent)
		asked++
		runtime.asked(t, asked)

		now = now.Add(alone - time.Nanosecond)
		_, err = c.Snapshot(ctx, runtime.host, runtime.port)
		require.ErrorIs(t, err, errRuntimeSilent, "still left alone after %s", alone-time.Nanosecond)
		now = now.Add(time.Nanosecond)
	}

	// An answer ends it, and the next timeout counts as the first.
	runtime.answering.Store(true)
	_, err := c.Snapshot(ctx, runtime.host, runtime.port)
	require.NoError(t, err)
	runtime.answering.Store(false)
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)
	now = now.Add(shortestRuntimeSilence)
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	assert.NotErrorIs(t, err, errRuntimeSilent)
}

// Only a snapshot read has the short deadline. Starting an engine can take the
// runtime longer than a read may, and is given the time.
func TestHTTPRuntimeGivesACallThatChangesStateItsOwnTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(ActivateResponse{Status: "success", ModelName: "m1", Port: 9001})
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	c := newHTTPRuntimeClient(100*time.Millisecond, time.Now)

	response, err := c.Activate(context.Background(), u.Hostname(), port, &ActivateRequest{ModelName: "m1"})

	require.NoError(t, err)
	assert.Equal(t, int32(9001), response.Port)
}

// Whatever call times out, the runtime did not answer in time.
func TestHTTPRuntimeLeavesARuntimeAloneAfterAnyCallTimesOut(t *testing.T) {
	c, runtimes := hangingRuntimes(1, time.Now)
	runtime := runtimes[0]
	c.snapshotTimeout = 5 * time.Second
	c.httpClient.Timeout = shortDeadline
	ctx := context.Background()

	_, err := c.Activate(ctx, runtime.host, runtime.port, &ActivateRequest{ModelName: "m1"})
	require.Error(t, err)
	require.NotErrorIs(t, err, errRuntimeSilent)
	runtime.asked(t, 1)

	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	assert.ErrorIs(t, err, errRuntimeSilent)
	runtime.asked(t, 1)
}

type timedOut struct{}

func (timedOut) Error() string   { return "timed out" }
func (timedOut) Timeout() bool   { return true }
func (timedOut) Temporary() bool { return true }

func TestRuntimeSilenceForgetsARuntimeNotHeardOfForAMinute(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	silence := newRuntimeSilence(func() time.Time { return now })

	silence.observe("10.0.0.1:8080", timedOut{})
	silence.observe("10.0.0.1:8080", timedOut{})
	require.Equal(t, 2, silence.runtimes["10.0.0.1:8080"].timeouts)

	// Its pod is gone, so nothing calls it again. It is dropped once another
	// runtime times out a minute after it was last left alone.
	now = now.Add(2*shortestRuntimeSilence + runtimeSilenceWindow - time.Nanosecond)
	silence.observe("10.0.0.2:8080", timedOut{})
	assert.Contains(t, silence.runtimes, "10.0.0.1:8080")
	now = now.Add(time.Nanosecond)
	silence.observe("10.0.0.3:8080", timedOut{})
	assert.NotContains(t, silence.runtimes, "10.0.0.1:8080")
	assert.Contains(t, silence.runtimes, "10.0.0.2:8080")

	// A runtime that comes back that late starts over as well.
	now = now.Add(time.Hour)
	silence.observe("10.0.0.2:8080", timedOut{})
	assert.Equal(t, 1, silence.runtimes["10.0.0.2:8080"].timeouts)
	assert.Equal(t, now.Add(shortestRuntimeSilence), silence.runtimes["10.0.0.2:8080"].until)
}

// A stop is sent to a runtime that is left alone. If it times out as well,
// the runtime is left alone for longer, from then.
func TestHTTPRuntimeCountsAStopThatTimesOutOnARuntimeLeftAlone(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, runtimes := hangingRuntimes(1, func() time.Time { return now })
	runtime := runtimes[0]
	c.httpClient.Timeout = shortDeadline
	ctx := context.Background()
	_, err := c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)

	now = now.Add(shortestRuntimeSilence / 2)
	err = c.Deactivate(ctx, runtime.host, runtime.port, &DeactivateRequest{ModelName: "m1"})

	require.Error(t, err)
	require.NotErrorIs(t, err, errRuntimeSilent)
	runtime.asked(t, 2)
	assert.Equal(t, silentRuntime{timeouts: 2, until: now.Add(2 * shortestRuntimeSilence)},
		c.silence.runtimes[runtime.address()])
}

func TestHTTPRuntimeStillStopsAnEngineOnARuntimeThatDidNotAnswer(t *testing.T) {
	// A claim is deleted or scaled down only once, so stopping its engine is
	// tried even on a runtime that did not answer a moment ago.
	c, runtimes := hangingRuntimes(1, time.Now)
	runtime := runtimes[0]
	ctx := context.Background()
	_, err := c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)
	runtime.asked(t, 1)

	runtime.answering.Store(true)
	require.NoError(t, c.Deactivate(ctx, runtime.host, runtime.port, &DeactivateRequest{ModelName: "m1"}))
	runtime.asked(t, 2)

	// It answered, so it is called as usual again.
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	require.NoError(t, err)
	runtime.asked(t, 3)
}

func TestHTTPRuntimeCallsAgainARuntimeThatFailedFast(t *testing.T) {
	// A runtime that answers with an error, or refuses the connection, costs
	// nothing to call again, so it is not left alone.
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := listener.Addr().(*net.TCPAddr)
	require.NoError(t, listener.Close())
	c := newHTTPRuntimeClient(5*time.Second, time.Now)
	ctx := context.Background()

	for range 2 {
		_, err := c.Snapshot(ctx, u.Hostname(), port)
		require.Error(t, err)
		assert.NotErrorIs(t, err, errRuntimeSilent)
		_, err = c.Snapshot(ctx, refused.IP.String(), refused.Port)
		require.Error(t, err)
		assert.NotErrorIs(t, err, errRuntimeSilent)
	}
	assert.Equal(t, int32(2), requests.Load())
}

func TestHTTPRuntimeLeavesOnlyTheRuntimeThatDidNotAnswerAlone(t *testing.T) {
	// Each runtime is remembered by its own address, so one that does not
	// answer costs the others nothing.
	now := time.Unix(1_700_000_000, 0)
	c, runtimes := hangingRuntimes(3, func() time.Time { return now })
	silent, answering, late := runtimes[0], runtimes[1], runtimes[2]
	answering.answering.Store(true)
	ctx := context.Background()

	_, err := c.Snapshot(ctx, silent.host, silent.port)
	require.Error(t, err)
	now = now.Add(shortestRuntimeSilence)
	_, err = c.Snapshot(ctx, silent.host, silent.port)
	require.Error(t, err)
	_, err = c.Snapshot(ctx, silent.host, silent.port)
	require.ErrorIs(t, err, errRuntimeSilent)

	_, err = c.Snapshot(ctx, answering.host, answering.port)
	require.NoError(t, err)
	silent.asked(t, 2)
	answering.asked(t, 1)

	// Its timeouts are its own as well. Another runtime that times out for
	// the first time is left alone for a round, and not for two.
	_, err = c.Snapshot(ctx, late.host, late.port)
	require.Error(t, err)
	assert.Equal(t, silentRuntime{timeouts: 2, until: now.Add(2 * shortestRuntimeSilence)},
		c.silence.runtimes[silent.address()])
	assert.Equal(t, silentRuntime{timeouts: 1, until: now.Add(shortestRuntimeSilence)},
		c.silence.runtimes[late.address()])
}

// A call given up by its caller, as when the controller shuts down, says
// nothing about the runtime. It does not leave the runtime alone, and it does
// not end the time a runtime is left alone either.
func TestHTTPRuntimeLearnsNothingFromACallThatWasCanceled(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, runtimes := hangingRuntimes(1, func() time.Time { return now })
	runtime := runtimes[0]
	c.snapshotTimeout = 5 * time.Second
	canceled := func(call func(ctx context.Context) error) {
		t.Helper()
		sent := runtime.requests.Load()
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for runtime.requests.Load() == sent {
				time.Sleep(time.Millisecond)
			}
			cancel()
		}()
		require.ErrorIs(t, call(ctx), context.Canceled)
	}
	read := func(ctx context.Context) error {
		_, err := c.Snapshot(ctx, runtime.host, runtime.port)
		return err
	}

	canceled(read)
	assert.NotContains(t, c.silence.runtimes, runtime.address())

	c.snapshotTimeout = shortDeadline
	require.Error(t, read(context.Background()))
	leftAlone := c.silence.runtimes[runtime.address()]
	require.Equal(t, silentRuntime{timeouts: 1, until: now.Add(shortestRuntimeSilence)}, leftAlone)

	// A stop is sent to a runtime that is left alone, and is given up.
	canceled(func(ctx context.Context) error {
		return c.Deactivate(ctx, runtime.host, runtime.port, &DeactivateRequest{ModelName: "m1"})
	})
	assert.Equal(t, leftAlone, c.silence.runtimes[runtime.address()])
	assert.ErrorIs(t, read(context.Background()), errRuntimeSilent)
}

// Over a real socket: what the HTTP client returns for a call that its caller
// canceled is taken for a canceled call.
func TestHTTPRuntimeLearnsNothingFromACallCanceledOverASocket(t *testing.T) {
	arrived := make(chan struct{}, 1)
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		arrived <- struct{}{}
		<-released
	}))
	t.Cleanup(func() {
		close(released)
		srv.Close()
	})
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	c := newHTTPRuntimeClient(5*time.Second, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-arrived
		cancel()
	}()

	_, err := c.Snapshot(ctx, u.Hostname(), port)

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, c.silence.runtimes)
}

// A failure that is no timeout ends the time a runtime is left alone, as an
// answer does. The next timeout counts as the first.
func TestHTTPRuntimeStartsOverAfterACallThatFailedAtOnce(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, runtimes := hangingRuntimes(1, func() time.Time { return now })
	runtime := runtimes[0]
	ctx := context.Background()
	for range 2 {
		_, err := c.Snapshot(ctx, runtime.host, runtime.port)
		require.Error(t, err)
		now = now.Add(time.Minute)
	}
	require.Equal(t, 2, c.silence.runtimes[runtime.address()].timeouts)

	runtime.refusing.Store(true)
	_, err := c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)
	require.NotErrorIs(t, err, errRuntimeSilent)
	assert.NotContains(t, c.silence.runtimes, runtime.address())

	runtime.refusing.Store(false)
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)
	assert.Equal(t, silentRuntime{timeouts: 1, until: now.Add(shortestRuntimeSilence)},
		c.silence.runtimes[runtime.address()])
	runtime.asked(t, 4)
}

// A read is sent with the deadline the client was built with, counted from
// the moment of the call.
func TestHTTPRuntimeSendsAReadWithItsDeadline(t *testing.T) {
	const readDeadline = 7 * time.Second
	c, runtimes := hangingRuntimes(1, time.Now)
	runtime := runtimes[0]
	runtime.answering.Store(true)
	c.snapshotTimeout = readDeadline

	before := time.Now()
	_, err := c.Snapshot(context.Background(), runtime.host, runtime.port)
	after := time.Now()

	require.NoError(t, err)
	deadline := runtime.deadline.Load()
	require.NotNil(t, deadline)
	assert.False(t, deadline.Before(before.Add(readDeadline)), "the deadline is %s after the call", deadline.Sub(before))
	assert.False(t, deadline.After(after.Add(readDeadline)), "the deadline is %s after the call", deadline.Sub(after))
}

// The client that runs in production reads the time of day. With a clock that
// stood still, a runtime left alone once would never be called again.
func TestTheRuntimeClientReadsTheClock(t *testing.T) {
	c := NewRuntimeClient().(*httpRuntimeClient)

	before := time.Now()
	read := c.silence.now()
	after := time.Now()

	assert.False(t, read.Before(before))
	assert.False(t, read.After(after))
}

// stallingServer is a runtime behind a real socket that sends the headers of
// its answer and the start of a body, and then nothing until the test ends.
func stallingServer(t *testing.T) (host string, port int) {
	t.Helper()
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":`))
		w.(http.Flusher).Flush()
		<-released
	}))
	t.Cleanup(func() {
		close(released)
		srv.Close()
	})
	u, _ := url.Parse(srv.URL)
	port, _ = strconv.Atoi(u.Port())
	return u.Hostname(), port
}

// timedOutError reports whether a call failed because its time was up.
func timedOutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Over a real socket: an answer counts once all of it has arrived. A runtime
// that sends its headers and then stalls did not answer in time.
func TestHTTPRuntimeLeavesARuntimeThatStallsAfterItsHeadersAlone(t *testing.T) {
	read := func(c *httpRuntimeClient, host string, port int) error {
		_, err := c.Snapshot(context.Background(), host, port)
		return err
	}
	start := func(c *httpRuntimeClient, host string, port int) error {
		_, err := c.Activate(context.Background(), host, port, &ActivateRequest{ModelName: "m1"})
		return err
	}
	stop := func(c *httpRuntimeClient, host string, port int) error {
		return c.Deactivate(context.Background(), host, port, &DeactivateRequest{ModelName: "m1"})
	}
	for name, tc := range map[string]struct {
		readDeadline, clientTimeout time.Duration
		call                        func(c *httpRuntimeClient, host string, port int) error
	}{
		"a read, at its deadline":                       {100 * time.Millisecond, 5 * time.Second, read},
		"a start, at the timeout of the client":         {5 * time.Second, 100 * time.Millisecond, start},
		"a stop, which needs no body from its answer":   {5 * time.Second, 100 * time.Millisecond, stop},
		"a read, at the timeout of the client, if ever": {5 * time.Second, 100 * time.Millisecond, read},
	} {
		t.Run(name, func(t *testing.T) {
			host, port := stallingServer(t)
			c := newHTTPRuntimeClient(tc.readDeadline, time.Now)
			c.httpClient.Timeout = tc.clientTimeout

			err := tc.call(c, host, port)

			require.Error(t, err)
			assert.True(t, timedOutError(err), "%v", err)
			assert.Equal(t, 1, c.silence.runtimes[net.JoinHostPort(host, strconv.Itoa(port))].timeouts)
			assert.ErrorIs(t, read(c, host, port), errRuntimeSilent)
		})
	}
}

// The status of an answer decides, whatever its body says.
func TestHTTPRuntimeTakesAnErrorStatusForAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(ActivateResponse{Status: "success", ModelName: "m1", Port: 9001})
	}))
	defer srv.Close()
	c, host, port := clientForServer(srv)

	_, err := c.Activate(context.Background(), host, port, &ActivateRequest{ModelName: "m1"})
	require.ErrorContains(t, err, "returned 503")
	_, err = c.Snapshot(context.Background(), host, port)
	require.ErrorContains(t, err, "returned 503")
}

// A stall after the headers counts like any other timeout: each one in a row
// doubles the time alone. The headers are no answer that ends it.
func TestHTTPRuntimeCountsTheStallsOfARuntimeAfterItsHeaders(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, runtimes := hangingRuntimes(1, func() time.Time { return now })
	runtime := runtimes[0]
	runtime.stalling.Store(true)
	ctx := context.Background()

	_, err := c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)
	assert.True(t, timedOutError(err), "%v", err)
	now = now.Add(shortestRuntimeSilence)
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)

	assert.Equal(t, silentRuntime{timeouts: 2, until: now.Add(2 * shortestRuntimeSilence)},
		c.silence.runtimes[runtime.address()])
	runtime.asked(t, 2)

	// A call that is canceled while its body is read changes nothing.
	now = now.Add(2 * shortestRuntimeSilence)
	canceled, cancel := context.WithCancel(ctx)
	c.snapshotTimeout = 5 * time.Second
	go func() {
		for runtime.requests.Load() < 3 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err = c.Snapshot(canceled, runtime.host, runtime.port)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, c.silence.runtimes[runtime.address()].timeouts)
}

func TestRuntimeURLTakesAnIPv6PodAddress(t *testing.T) {
	// An IPv6 address has to be bracketed, or the port cannot be told from it.
	for podIP, host := range map[string]string{
		"10.0.0.7": "10.0.0.7:8080",
		"fd00::7":  "[fd00::7]:8080",
	} {
		req, err := http.NewRequest(http.MethodGet, runtimeURL(podIP, 8080, snapshotPath), nil)
		require.NoError(t, err, podIP)
		assert.Equal(t, host, req.URL.Host)
		assert.Equal(t, snapshotPath, req.URL.Path)
	}
}

func TestHTTPRuntimeSnapshot(t *testing.T) {
	observedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	requestSuccessTotal := int64(12)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/snapshot", r.URL.Path)
		_ = json.NewEncoder(w).Encode(RuntimeSnapshot{
			ObservedAt: observedAt,
			Accelerators: []RuntimeAcceleratorSnapshot{{
				ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 700,
			}},
			Models: []RuntimeSnapshotModel{{
				ModelName: "m1", ArtifactURL: "hf://Org/M1", Port: 9001,
				IPCName: "kvc_m1", Phase: "active", Alive: true, Ready: true,
				RestartCount: 2, LastError: "previous launch failed", LastTransition: &observedAt,
				KVUsedBytes: 10, KVCapacityBytes: 100,
				HBMPeakBytes:           456,
				RequestMetricsObserved: true, RequestsRunning: 2, RequestsWaiting: 1,
				RequestSuccessTotal: &requestSuccessTotal,
				ClaimRef:            &ModelClaimRef{Namespace: "default", Name: "m1", UID: "claim-uid"},
			}},
			CachedArtifacts: []string{"hf://Org/M1"},
		})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	snapshot, err := c.Snapshot(context.Background(), host, port)

	require.NoError(t, err)
	require.Len(t, snapshot.Accelerators, 1)
	assert.Equal(t, int64(700), snapshot.Accelerators[0].HBMFreeBytes)
	require.Len(t, snapshot.Models, 1)
	assert.Equal(t, "claim-uid", snapshot.Models[0].ClaimRef.UID)
	assert.True(t, snapshot.Models[0].Alive)
	assert.Equal(t, 2, snapshot.Models[0].RestartCount)
	assert.Equal(t, "previous launch failed", snapshot.Models[0].LastError)
	require.NotNil(t, snapshot.Models[0].LastTransition)
	assert.Equal(t, observedAt, *snapshot.Models[0].LastTransition)
	assert.Equal(t, int64(456), snapshot.Models[0].HBMPeakBytes)
	assert.True(t, snapshot.Models[0].RequestMetricsObserved)
	assert.Equal(t, int64(2), snapshot.Models[0].RequestsRunning)
	assert.Equal(t, int64(1), snapshot.Models[0].RequestsWaiting)
	require.NotNil(t, snapshot.Models[0].RequestSuccessTotal)
	assert.Equal(t, int64(12), *snapshot.Models[0].RequestSuccessTotal)
	assert.Equal(t, []string{"hf://Org/M1"}, snapshot.CachedArtifacts)
}

// TestHTTPRuntimeSnapshotReadsEveryFieldTheRuntimeSends decodes a snapshot as
// the runtime writes it, with a value in every field, so that a name changed
// in the client fails a test.
func TestHTTPRuntimeSnapshotReadsEveryFieldTheRuntimeSends(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"observed_at": "2026-09-21T10:00:00.123456Z",
			"accelerators": [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700, "hbm_usable_bytes": 950}],
			"models": [{"model_name": "m1", "artifact_url": "hf://Org/M1",
				"claim_ref": {"namespace": "default", "name": "claim-1", "uid": "uid-1"}, "port": 9001,
				"ipc_name": "kvc_m1", "phase": "active", "alive": true, "ready": true, "restart_count": 2,
				"last_error": "engine exited", "last_transition": "2026-09-21T09:59:58.000001Z",
				"kv_used_bytes": 300, "kv_capacity_bytes": 600, "hbm_peak_bytes": 456,
				"request_metrics_observed": true, "requests_running": 3, "requests_waiting": 1,
				"request_success_total": 12}],
			"cached_artifacts": ["hf://Org/M1"]}`))
	}))
	defer srv.Close()
	c, host, port := clientForServer(srv)

	snapshot, err := c.Snapshot(context.Background(), host, port)

	require.NoError(t, err)
	lastTransition := time.Date(2026, time.September, 21, 9, 59, 58, 1000, time.UTC)
	requestSuccessTotal := int64(12)
	assert.Equal(t, &RuntimeSnapshot{
		ObservedAt: time.Date(2026, time.September, 21, 10, 0, 0, 123456000, time.UTC),
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 700, HBMUsableBytes: 950},
		},
		Models: []RuntimeSnapshotModel{{
			ModelName: "m1", ArtifactURL: "hf://Org/M1",
			ClaimRef: &ModelClaimRef{Namespace: "default", Name: "claim-1", UID: "uid-1"},
			Port:     9001, IPCName: "kvc_m1", Phase: "active", Alive: true, Ready: true, RestartCount: 2,
			LastError: "engine exited", LastTransition: &lastTransition,
			KVUsedBytes: 300, KVCapacityBytes: 600, HBMPeakBytes: 456,
			RequestMetricsObserved: true, RequestsRunning: 3, RequestsWaiting: 1,
			RequestSuccessTotal: &requestSuccessTotal,
		}},
		CachedArtifacts: []string{"hf://Org/M1"},
	}, snapshot)
}

// TestHTTPRuntimeSnapshotReadsWhatTheRuntimeSends decodes what the runtime
// sends for an engine that is still booting, and for a card it could not
// measure.
func TestHTTPRuntimeSnapshotReadsWhatTheRuntimeSends(t *testing.T) {
	for name, tc := range map[string]struct {
		payload        string
		hbmUsableBytes int64
		sized          bool
		engines        int
	}{
		"a card the runtime measured": {`{
			"observed_at": "2026-09-21T10:00:00.123456Z",
			"accelerators": [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700, "hbm_usable_bytes": 950}],
			"models": [{"model_name": "m1", "artifact_url": "hf://Org/M1", "claim_ref": null, "port": 9001,
				"ipc_name": "kvc_m1", "phase": "booting", "alive": true, "ready": false, "restart_count": 0,
				"last_error": null, "last_transition": "2026-09-21T09:59:58.000001Z",
				"kv_used_bytes": -1, "kv_capacity_bytes": -1, "hbm_peak_bytes": 0,
				"request_metrics_observed": false, "requests_running": 0, "requests_waiting": 0,
				"request_success_total": null}],
			"cached_artifacts": []}`, 950, true, 1},
		"a card it could not measure": {`{
			"observed_at": "2026-09-21T10:00:00Z",
			"accelerators": [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700, "hbm_usable_bytes": -1}],
			"models": [], "cached_artifacts": []}`, -1, false, 0},
		"a runtime from before the field": {`{
			"observed_at": "2026-09-21T10:00:00Z",
			"accelerators": [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700}],
			"models": [], "cached_artifacts": []}`, 0, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.payload))
			}))
			defer srv.Close()
			c, host, port := clientForServer(srv)

			snapshot, err := c.Snapshot(context.Background(), host, port)

			require.NoError(t, err)
			require.Len(t, snapshot.Accelerators, 1)
			assert.Equal(t, tc.hbmUsableBytes, snapshot.Accelerators[0].HBMUsableBytes)
			hbmUsableBytes, sized := snapshot.hbmUsableBytes()
			assert.Equal(t, tc.sized, sized)
			if sized {
				assert.Equal(t, tc.hbmUsableBytes, hbmUsableBytes)
			}
			require.Len(t, snapshot.Models, tc.engines)
			for _, model := range snapshot.Models {
				assert.Equal(t, int64(-1), model.KVUsedBytes)
				assert.Equal(t, int64(-1), model.KVCapacityBytes)
				assert.Nil(t, model.ClaimRef)
				assert.Empty(t, model.LastError)
				require.NotNil(t, model.LastTransition)
			}
		})
	}
}

// A start is taken back only when the engine is known not to have started.
// This goes through the real client, so it holds what the client makes of
// each answer, and of each call that got none.
func TestHTTPRuntimeTellsAStartThatWasRefusedFromOneThatMayHaveBeenDone(t *testing.T) {
	answering := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	hijacked := func(then func(conn net.Conn)) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			then(conn)
		}
	}
	ownError := `{"status": "error", "model_name": "m1", "port": 0, "ipc_name": "", "message": "boom"}`

	for name, tc := range map[string]struct {
		runtime http.HandlerFunc
		notDone bool
		// patience is how long the client waits for an answer.
		patience time.Duration
	}{
		// What the runtime itself sends when it rejects a request, and when
		// starting the engine failed.
		"the runtime's own 400":                {answering(http.StatusBadRequest, ownError), true, time.Minute},
		"the runtime's own 500":                {answering(http.StatusInternalServerError, ownError), true, time.Minute},
		"a success status with an error in it": {answering(http.StatusOK, ownError), true, time.Minute},
		// A request that did not pass the API's validation never reached the
		// runtime.
		"a 422 from the API": {answering(http.StatusUnprocessableEntity, `{"detail": []}`), true, time.Minute},
		// A status from 400 to 499 blames the request, whatever its body says.
		"a 400 that is not the runtime's": {answering(http.StatusBadRequest, "Invalid HTTP request received."), true, time.Minute},
		"a 499":                           {answering(499, "client closed request"), true, time.Minute},
		"a 399":                           {answering(399, "nothing the runtime sends"), false, time.Minute},
		// Something between the controller and the runtime gave up waiting.
		// The runtime may still be starting the engine.
		"a 504 that is not the runtime's": {answering(http.StatusGatewayTimeout, "upstream request timeout"), false, time.Minute},
		"a 502 that is not the runtime's": {answering(http.StatusBadGateway, "<html>bad gateway</html>"), false, time.Minute},
		"a 502 with a report of its own":  {answering(http.StatusBadGateway, `{"status": "unavailable"}`), false, time.Minute},
		// What the framework sends for an exception outside the handler, when
		// the engine may already run.
		"a 500 that is not the runtime's": {answering(http.StatusInternalServerError, "Internal Server Error"), false, time.Minute},
		// A status that neither succeeds nor blames the request says nothing
		// of what the runtime did.
		"a 202 nobody asked for":            {answering(http.StatusAccepted, `{"status": "accepted"}`), false, time.Minute},
		"closed after the request was read": {hijacked(func(conn net.Conn) { _ = conn.Close() }), false, time.Minute},
		"reset after the request was read": {hijacked(func(conn net.Conn) {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
		}), false, time.Minute},
		"no answer in time": {hijacked(func(conn net.Conn) {
			time.Sleep(time.Second)
			_ = conn.Close()
		}), false, 300 * time.Millisecond},
		"a success status that cannot be read": {answering(http.StatusOK, "<html>ok</html>"), false, time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.runtime)
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			port, _ := strconv.Atoi(u.Port())
			c := NewRuntimeClient().(*httpRuntimeClient)
			// Only the runtime that gives no answer is waited for. An answer
			// that comes late on a busy machine is still an answer.
			c.httpClient.Timeout = tc.patience

			_, err := c.Activate(context.Background(), u.Hostname(), port, &ActivateRequest{ModelName: "m1"})

			require.Error(t, err)
			assert.Equal(t, tc.notDone, callNotDone(err), "%v", err)
		})
	}

	// Nobody listens, so the call was never sent.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := listener.Addr().(*net.TCPAddr)
	require.NoError(t, listener.Close())
	_, err = NewRuntimeClient().Activate(context.Background(), refused.IP.String(), refused.Port,
		&ActivateRequest{ModelName: "m1"})
	require.Error(t, err)
	assert.True(t, callNotDone(err), "%v", err)
}

func TestHTTPRuntimeSetKVLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/models/kv-limit", r.URL.Path)
		var req SetKVLimitRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "m1", req.ModelName)
		assert.Equal(t, int64(4096), req.LimitBytes)
		assert.Equal(t, "limit-1", req.OperationID)
		_ = json.NewEncoder(w).Encode(RuntimeOperationResponse{
			Status: "success", ModelName: "m1", OperationID: "limit-1", Applied: true, Phase: "active",
		})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	response, err := c.SetKVLimit(context.Background(), host, port, &SetKVLimitRequest{
		ModelName: "m1", LimitBytes: 4096, OperationID: "limit-1",
	})

	require.NoError(t, err)
	assert.True(t, response.Applied)
	assert.Equal(t, "active", response.Phase)
}

func TestHTTPRuntimeSleepAndWake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/runtime/models/sleep":
			var req SleepRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "m1", req.ModelName)
			assert.Equal(t, 2, req.Level)
			assert.Equal(t, "sleep-1", req.OperationID)
			_ = json.NewEncoder(w).Encode(RuntimeOperationResponse{
				Status: "success", ModelName: "m1", OperationID: "sleep-1", Applied: true, Phase: "sleeping",
			})
		case "/v1/runtime/models/wake":
			var req WakeRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "m1", req.ModelName)
			assert.Equal(t, "wake-1", req.OperationID)
			_ = json.NewEncoder(w).Encode(RuntimeOperationResponse{
				Status: "success", ModelName: "m1", OperationID: "wake-1", Applied: true, Phase: "active",
			})
		default:
			t.Fatalf("unexpected runtime control path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	slept, err := c.Sleep(context.Background(), host, port, &SleepRequest{
		ModelName: "m1", Level: 2, OperationID: "sleep-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "sleeping", slept.Phase)
	woken, err := c.Wake(context.Background(), host, port, &WakeRequest{
		ModelName: "m1", OperationID: "wake-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "active", woken.Phase)
}

func TestHTTPRuntimeActivate_StatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ActivateResponse{Status: "error", Message: "out of HBM"})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	_, err := c.Activate(context.Background(), host, port, &ActivateRequest{ModelName: "m1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of HBM")
}

func TestHTTPRuntimeActivate_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	_, err := c.Activate(context.Background(), host, port, &ActivateRequest{ModelName: "m1"})
	require.Error(t, err)
}

func TestHTTPRuntimeDeactivate(t *testing.T) {
	var gotMode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/models/deactivate", r.URL.Path)
		var req DeactivateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotMode = string(req.Mode)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	err := c.Deactivate(context.Background(), host, port, &DeactivateRequest{ModelName: "m1", Mode: DeactivateStop})
	require.NoError(t, err)
	assert.Equal(t, "stop", gotMode)
}

func TestHTTPRuntimeListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/models", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string][]ModelInfo{
			"models": {{ModelName: "m1", Port: 9001, Phase: "active", KVUsedBytes: 10, KVTotalBytes: 100}},
		})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	models, err := c.ListModels(context.Background(), host, port)
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "m1", models[0].ModelName)
	assert.Equal(t, int32(9001), models[0].Port)
	assert.Equal(t, int64(10), models[0].KVUsedBytes)
	assert.Equal(t, int64(100), models[0].KVTotalBytes)
}
