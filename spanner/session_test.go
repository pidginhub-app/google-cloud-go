/*
Copyright 2017 Google LLC

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

package spanner

import (
	"bytes"
	"container/heap"
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	vkit "cloud.google.com/go/spanner/apiv1"
	"cloud.google.com/go/spanner/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSessionPoolConfigValidation tests session pool config validation.
func TestSessionPoolConfigValidation(t *testing.T) {
	t.Parallel()
	server, client := newSpannerInMemTestServer(t)
	defer server.teardown(client)

	for _, test := range []struct {
		spc SessionPoolConfig
		err error
	}{
		{
			SessionPoolConfig{},
			errNoRPCGetter(),
		},
		{
			SessionPoolConfig{
				getRPCClient: func() (*vkit.Client, error) {
					return client.clients[0], nil
				},
				MinOpened: 10,
				MaxOpened: 5,
			},
			errMinOpenedGTMaxOpened(5, 10),
		},
	} {
		if _, err := newSessionPool("mockdb", test.spc, nil); !testEqual(err, test.err) {
			t.Fatalf("want %v, got %v", test.err, err)
		}
	}
}

// TestSessionCreation tests session creation during sessionPool.Take().
func TestSessionCreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServer(t)
	defer server.teardown(client)
	sp := client.idleSessions

	// Take three sessions from session pool, this should trigger session pool
	// to create three new sessions.
	shs := make([]*sessionHandle, 3)
	// gotDs holds the unique sessions taken from session pool.
	gotDs := map[string]bool{}
	for i := 0; i < len(shs); i++ {
		var err error
		shs[i], err = sp.take(ctx)
		if err != nil {
			t.Fatalf("failed to get session(%v): %v", i, err)
		}
		gotDs[shs[i].getID()] = true
	}
	if len(gotDs) != len(shs) {
		t.Fatalf("session pool created %v sessions, want %v", len(gotDs), len(shs))
	}
	if wantDs := server.testSpanner.DumpSessions(); !testEqual(gotDs, wantDs) {
		t.Fatalf("session pool creates sessions %v, want %v", gotDs, wantDs)
	}
	// Verify that created sessions are recorded correctly in session pool.
	sp.mu.Lock()
	if int(sp.numOpened) != len(shs) {
		t.Fatalf("session pool reports %v open sessions, want %v", sp.numOpened, len(shs))
	}
	if sp.createReqs != 0 {
		t.Fatalf("session pool reports %v session create requests, want 0", int(sp.createReqs))
	}
	sp.mu.Unlock()
	// Verify that created sessions are tracked correctly by healthcheck queue.
	hc := sp.hc
	hc.mu.Lock()
	if hc.queue.Len() != len(shs) {
		t.Fatalf("healthcheck queue length = %v, want %v", hc.queue.Len(), len(shs))
	}
	for _, s := range hc.queue.sessions {
		if !gotDs[s.getID()] {
			t.Fatalf("session %v is in healthcheck queue, but it is not created by session pool", s.getID())
		}
	}
	hc.mu.Unlock()
}

// TestTakeFromIdleList tests taking sessions from session pool's idle list.
func TestTakeFromIdleList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Make sure maintainer keeps the idle sessions.
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{MaxIdle: 10},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	// Take ten sessions from session pool and recycle them.
	shs := make([]*sessionHandle, 10)
	for i := 0; i < len(shs); i++ {
		var err error
		shs[i], err = sp.take(ctx)
		if err != nil {
			t.Fatalf("failed to get session(%v): %v", i, err)
		}
	}
	// Make sure it's sampled once before recycling, otherwise it will be
	// cleaned up.
	<-time.After(sp.SessionPoolConfig.healthCheckSampleInterval)
	for i := 0; i < len(shs); i++ {
		shs[i].recycle()
	}
	// Further session requests from session pool won't cause mockclient to
	// create more sessions.
	wantSessions := server.testSpanner.DumpSessions()
	// Take ten sessions from session pool again, this time all sessions should
	// come from idle list.
	gotSessions := map[string]bool{}
	for i := 0; i < len(shs); i++ {
		sh, err := sp.take(ctx)
		if err != nil {
			t.Fatalf("cannot take session from session pool: %v", err)
		}
		gotSessions[sh.getID()] = true
	}
	if len(gotSessions) != 10 {
		t.Fatalf("got %v unique sessions, want 10", len(gotSessions))
	}
	if !testEqual(gotSessions, wantSessions) {
		t.Fatalf("got sessions: %v, want %v", gotSessions, wantSessions)
	}
}

// TesttakeWriteSessionFromIdleList tests taking write sessions from session
// pool's idle list.
func TestTakeWriteSessionFromIdleList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Make sure maintainer keeps the idle sessions.
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{MaxIdle: 20},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	// Take ten sessions from session pool and recycle them.
	shs := make([]*sessionHandle, 10)
	for i := 0; i < len(shs); i++ {
		var err error
		shs[i], err = sp.takeWriteSession(ctx)
		if err != nil {
			t.Fatalf("failed to get session(%v): %v", i, err)
		}
	}
	// Make sure it's sampled once before recycling, otherwise it will be
	// cleaned up.
	<-time.After(sp.SessionPoolConfig.healthCheckSampleInterval)
	for i := 0; i < len(shs); i++ {
		shs[i].recycle()
	}
	// Further session requests from session pool won't cause mockclient to
	// create more sessions.
	wantSessions := server.testSpanner.DumpSessions()
	// Take ten sessions from session pool again, this time all sessions should
	// come from idle list.
	gotSessions := map[string]bool{}
	for i := 0; i < len(shs); i++ {
		sh, err := sp.takeWriteSession(ctx)
		if err != nil {
			t.Fatalf("cannot take session from session pool: %v", err)
		}
		gotSessions[sh.getID()] = true
	}
	if len(gotSessions) != 10 {
		t.Fatalf("got %v unique sessions, want 10", len(gotSessions))
	}
	if !testEqual(gotSessions, wantSessions) {
		t.Fatalf("got sessions: %v, want %v", gotSessions, wantSessions)
	}
}

// TestTakeFromIdleListChecked tests taking sessions from session pool's idle
// list, but with a extra ping check.
func TestTakeFromIdleListChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Make sure maintainer keeps the idle sessions.
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				MaxIdle:                   1,
				HealthCheckInterval:       50 * time.Millisecond,
				healthCheckSampleInterval: 10 * time.Millisecond,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	// Stop healthcheck workers to simulate slow pings.
	sp.hc.close()

	// Create a session and recycle it.
	sh, err := sp.take(ctx)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}

	// Make sure it's sampled once before recycling, otherwise it will be
	// cleaned up.
	<-time.After(sp.SessionPoolConfig.healthCheckSampleInterval)
	wantSid := sh.getID()
	sh.recycle()

	// TODO(deklerk): get rid of this
	<-time.After(time.Second)

	// Two back-to-back session requests, both of them should return the same
	// session created before and none of them should trigger a session ping.
	for i := 0; i < 2; i++ {
		// Take the session from the idle list and recycle it.
		sh, err = sp.take(ctx)
		if err != nil {
			t.Fatalf("%v - failed to get session: %v", i, err)
		}
		if gotSid := sh.getID(); gotSid != wantSid {
			t.Fatalf("%v - got session id: %v, want %v", i, gotSid, wantSid)
		}

		// The two back-to-back session requests shouldn't trigger any session
		// pings because sessionPool.Take
		// reschedules the next healthcheck.
		if got, want := server.testSpanner.DumpPings(), ([]string{wantSid}); !testEqual(got, want) {
			t.Fatalf("%v - got ping session requests: %v, want %v", i, got, want)
		}
		sh.recycle()
	}

	// Inject session error to server stub, and take the session from the
	// session pool, the old session should be destroyed and the session pool
	// will create a new session.
	server.testSpanner.PutExecutionTime(testutil.MethodGetSession,
		testutil.SimulatedExecutionTime{
			Errors: []error{status.Errorf(codes.NotFound, "Session not found")},
		})

	// Delay to trigger sessionPool.Take to ping the session.
	// TODO(deklerk): get rid of this
	<-time.After(time.Second)

	// take will take the idle session. Then it will send a GetSession request
	// to check if it's healthy. It'll discover that it's not healthy
	// (NotFound), drop it, and create a new session.
	sh, err = sp.take(ctx)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	ds := server.testSpanner.DumpSessions()
	if len(ds) != 1 {
		t.Fatalf("dumped sessions from mockclient: %v, want %v", ds, sh.getID())
	}
	if sh.getID() == wantSid {
		t.Fatalf("sessionPool.Take still returns the same session %v, want it to create a new one", wantSid)
	}
}

// TestTakeFromIdleWriteListChecked tests taking sessions from session pool's
// idle list, but with a extra ping check.
func TestTakeFromIdleWriteListChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Make sure maintainer keeps the idle sessions.
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				MaxIdle:                   1,
				HealthCheckInterval:       50 * time.Millisecond,
				healthCheckSampleInterval: 10 * time.Millisecond,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	// Stop healthcheck workers to simulate slow pings.
	sp.hc.close()

	// Create a session and recycle it.
	sh, err := sp.takeWriteSession(ctx)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	wantSid := sh.getID()

	// Make sure it's sampled once before recycling, otherwise it will be
	// cleaned up.
	<-time.After(sp.SessionPoolConfig.healthCheckSampleInterval)
	sh.recycle()

	// TODO(deklerk): get rid of this
	<-time.After(time.Second)

	// Two back-to-back session requests, both of them should return the same
	// session created before and none of them should trigger a session ping.
	for i := 0; i < 2; i++ {
		// Take the session from the idle list and recycle it.
		sh, err = sp.takeWriteSession(ctx)
		if err != nil {
			t.Fatalf("%v - failed to get session: %v", i, err)
		}
		if gotSid := sh.getID(); gotSid != wantSid {
			t.Fatalf("%v - got session id: %v, want %v", i, gotSid, wantSid)
		}
		// The two back-to-back session requests shouldn't trigger any session
		// pings because sessionPool.Take reschedules the next healthcheck.
		if got, want := server.testSpanner.DumpPings(), ([]string{wantSid}); !testEqual(got, want) {
			t.Fatalf("%v - got ping session requests: %v, want %v", i, got, want)
		}
		sh.recycle()
	}

	// Inject session error to mockclient, and take the session from the
	// session pool, the old session should be destroyed and the session pool
	// will create a new session.
	server.testSpanner.PutExecutionTime(testutil.MethodGetSession,
		testutil.SimulatedExecutionTime{
			Errors: []error{status.Errorf(codes.NotFound, "Session not found")},
		})

	// Delay to trigger sessionPool.Take to ping the session.
	// TOOD(deklerk) get rid of this
	<-time.After(time.Second)

	sh, err = sp.takeWriteSession(ctx)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	ds := server.testSpanner.DumpSessions()
	if len(ds) != 1 {
		t.Fatalf("dumped sessions from mockclient: %v, want %v", ds, sh.getID())
	}
	if sh.getID() == wantSid {
		t.Fatalf("sessionPool.Take still returns the same session %v, want it to create a new one", wantSid)
	}
}

// TestMaxOpenedSessions tests max open sessions constraint.
func TestMaxOpenedSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				MaxOpened: 1,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	sh1, err := sp.take(ctx)
	if err != nil {
		t.Fatalf("cannot take session from session pool: %v", err)
	}

	// Session request will timeout due to the max open sessions constraint.
	ctx2, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, gotErr := sp.take(ctx2)
	if wantErr := errGetSessionTimeout(); !testEqual(gotErr, wantErr) {
		t.Fatalf("the second session retrival returns error %v, want %v", gotErr, wantErr)
	}

	go func() {
		// TODO(deklerk): remove this
		<-time.After(time.Second)
		// Destroy the first session to allow the next session request to
		// proceed.
		sh1.destroy()
	}()

	// Now session request can be processed because the first session will be
	// destroyed.
	sh2, err := sp.take(ctx)
	if err != nil {
		t.Fatalf("after the first session is destroyed, session retrival still returns error %v, want nil", err)
	}
	if !sh2.session.isValid() || sh2.getID() == "" {
		t.Fatalf("got invalid session: %v", sh2.session)
	}
}

// TestMinOpenedSessions tests min open session constraint.
func TestMinOpenedSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				MinOpened: 1,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	// Take ten sessions from session pool and recycle them.
	var ss []*session
	var shs []*sessionHandle
	for i := 0; i < 10; i++ {
		sh, err := sp.take(ctx)
		if err != nil {
			t.Fatalf("failed to get session(%v): %v", i, err)
		}
		ss = append(ss, sh.session)
		shs = append(shs, sh)
		sh.recycle()
	}
	for _, sh := range shs {
		sh.recycle()
	}

	// Simulate session expiration.
	for _, s := range ss {
		s.destroy(true)
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()
	// There should be still one session left in idle list due to the min open
	// sessions constraint.
	if sp.idleList.Len() != 1 {
		t.Fatalf("got %v sessions in idle list, want 1 %d", sp.idleList.Len(), sp.numOpened)
	}
}

// TestMaxBurst tests max burst constraint.
func TestMaxBurst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				MaxBurst: 1,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	// Will cause session creation RPC to be retried forever.
	server.testSpanner.PutExecutionTime(testutil.MethodCreateSession,
		testutil.SimulatedExecutionTime{
			Errors:    []error{status.Errorf(codes.Unavailable, "try later")},
			KeepError: true,
		})

	// This session request will never finish until the injected error is
	// cleared.
	go sp.take(ctx)

	// Poll for the execution of the first session request.
	for {
		sp.mu.Lock()
		cr := sp.createReqs
		sp.mu.Unlock()
		if cr == 0 {
			<-time.After(time.Second)
			continue
		}
		// The first session request is being executed.
		break
	}

	ctx2, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, gotErr := sp.take(ctx2)

	// Since MaxBurst == 1, the second session request should block.
	if wantErr := errGetSessionTimeout(); !testEqual(gotErr, wantErr) {
		t.Fatalf("session retrival returns error %v, want %v", gotErr, wantErr)
	}

	// Let the first session request succeed.
	server.testSpanner.Freeze()
	server.testSpanner.PutExecutionTime(testutil.MethodCreateSession, testutil.SimulatedExecutionTime{})
	//close(allowRequests)
	server.testSpanner.Unfreeze()

	// Now new session request can proceed because the first session request will eventually succeed.
	sh, err := sp.take(ctx)
	if err != nil {
		t.Fatalf("session retrival returns error %v, want nil", err)
	}
	if !sh.session.isValid() || sh.getID() == "" {
		t.Fatalf("got invalid session: %v", sh.session)
	}
}

// TestSessionRecycle tests recycling sessions.
func TestSessionRecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				MinOpened: 1,
				MaxIdle:   5,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	// Test session is correctly recycled and reused.
	for i := 0; i < 20; i++ {
		s, err := sp.take(ctx)
		if err != nil {
			t.Fatalf("cannot get the session %v: %v", i, err)
		}
		s.recycle()
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()
	// Ideally it should only be 1, because the session should be recycled and
	// re-used each time. However, sometimes the pool maintainer might increase
	// the pool size by 1 right around the time we take (which also increases
	// the pool size by 1), so this assertion is OK with either 1 or 2. We
	// expect never to see more than 2, though, even when MaxIdle is quite high:
	// each session should be recycled and re-used.
	if sp.numOpened != 1 && sp.numOpened != 2 {
		t.Fatalf("Expect session pool size 1 or 2, got %d", sp.numOpened)
	}
}

// TODO(deklerk): Investigate why s.destroy(true) is flakey.
// TestSessionDestroy tests destroying sessions.
func TestSessionDestroy(t *testing.T) {
	t.Skip("s.destroy(true) is flakey")
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				MinOpened: 1,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	<-time.After(10 * time.Millisecond) // maintainer will create one session, we wait for it create session to avoid flakiness in test
	sh, err := sp.take(ctx)
	if err != nil {
		t.Fatalf("cannot get session from session pool: %v", err)
	}
	s := sh.session
	sh.recycle()
	if d := s.destroy(true); d || !s.isValid() {
		// Session should be remaining because of min open sessions constraint.
		t.Fatalf("session %s invalid, want it to stay alive. (destroy in expiration mode, success: %v)", s.id, d)
	}
	if d := s.destroy(false); !d || s.isValid() {
		// Session should be destroyed.
		t.Fatalf("failed to destroy session %s. (destroy in default mode, success: %v)", s.id, d)
	}
}

// TestHcHeap tests heap operation on top of hcHeap.
func TestHcHeap(t *testing.T) {
	in := []*session{
		{nextCheck: time.Unix(10, 0)},
		{nextCheck: time.Unix(0, 5)},
		{nextCheck: time.Unix(1, 8)},
		{nextCheck: time.Unix(11, 7)},
		{nextCheck: time.Unix(6, 3)},
	}
	want := []*session{
		{nextCheck: time.Unix(1, 8), hcIndex: 0},
		{nextCheck: time.Unix(6, 3), hcIndex: 1},
		{nextCheck: time.Unix(8, 2), hcIndex: 2},
		{nextCheck: time.Unix(10, 0), hcIndex: 3},
		{nextCheck: time.Unix(11, 7), hcIndex: 4},
	}
	hh := hcHeap{}
	for _, s := range in {
		heap.Push(&hh, s)
	}
	// Change top of the heap and do a adjustment.
	hh.sessions[0].nextCheck = time.Unix(8, 2)
	heap.Fix(&hh, 0)
	for idx := 0; hh.Len() > 0; idx++ {
		got := heap.Pop(&hh).(*session)
		want[idx].hcIndex = -1
		if !testEqual(got, want[idx]) {
			t.Fatalf("%v: heap.Pop returns %v, want %v", idx, got, want[idx])
		}
	}
}

// TestHealthCheckScheduler tests if healthcheck workers can schedule and
// perform healthchecks properly.
func TestHealthCheckScheduler(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				HealthCheckInterval:       50 * time.Millisecond,
				healthCheckSampleInterval: 10 * time.Millisecond,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	// Create 50 sessions.
	ss := []string{}
	for i := 0; i < 50; i++ {
		sh, err := sp.take(ctx)
		if err != nil {
			t.Fatalf("cannot get session from session pool: %v", err)
		}
		ss = append(ss, sh.getID())
	}

	// Wait for 10-30 pings per session.
	waitFor(t, func() error {
		dp := server.testSpanner.DumpPings()
		gotPings := map[string]int64{}
		for _, p := range dp {
			gotPings[p]++
		}
		for _, s := range ss {
			want := int64(20)
			if got := gotPings[s]; got < want/2 || got > want+want/2 {
				// This is an unnacceptable amount of pings.
				return fmt.Errorf("got %v healthchecks on session %v, want it between (%v, %v)", got, s, want/2, want+want/2)
			}
		}
		return nil
	})
}

// Tests that a fractions of sessions are prepared for write by health checker.
func TestWriteSessionsPrepared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				WriteSessions: 0.5,
				MaxIdle:       20,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	shs := make([]*sessionHandle, 10)
	var err error
	for i := 0; i < 10; i++ {
		shs[i], err = sp.take(ctx)
		if err != nil {
			t.Fatalf("cannot get session from session pool: %v", err)
		}
	}
	// Now there are 10 sessions in the pool. Release them.
	for _, sh := range shs {
		sh.recycle()
	}

	// Sleep for 1s, allowing healthcheck workers to invoke begin transaction.
	// TODO(deklerk): get rid of this
	<-time.After(time.Second)
	wshs := make([]*sessionHandle, 5)
	for i := 0; i < 5; i++ {
		wshs[i], err = sp.takeWriteSession(ctx)
		if err != nil {
			t.Fatalf("cannot get session from session pool: %v", err)
		}
		if wshs[i].getTransactionID() == nil {
			t.Fatalf("got nil transaction id from session pool")
		}
	}
	for _, sh := range wshs {
		sh.recycle()
	}

	// TODO(deklerk): get rid of this
	<-time.After(time.Second)

	// Now force creation of 10 more sessions.
	shs = make([]*sessionHandle, 20)
	for i := 0; i < 20; i++ {
		shs[i], err = sp.take(ctx)
		if err != nil {
			t.Fatalf("cannot get session from session pool: %v", err)
		}
	}

	// Now there are 20 sessions in the pool. Release them.
	for _, sh := range shs {
		sh.recycle()
	}

	// TODO(deklerk): get rid of this
	<-time.After(time.Second)

	if sp.idleWriteList.Len() != 10 {
		t.Fatalf("Expect 10 write prepared session, got: %d", sp.idleWriteList.Len())
	}
}

// TestTakeFromWriteQueue tests that sessionPool.take() returns write prepared
// sessions as well.
func TestTakeFromWriteQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				MaxOpened:     1,
				WriteSessions: 1.0,
				MaxIdle:       1,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	sh, err := sp.take(ctx)
	if err != nil {
		t.Fatalf("cannot get session from session pool: %v", err)
	}
	sh.recycle()

	// TODO(deklerk): get rid of this
	<-time.After(time.Second)

	// The session should now be in write queue but take should also return it.
	if sp.idleWriteList.Len() == 0 {
		t.Fatalf("write queue unexpectedly empty")
	}
	if sp.idleList.Len() != 0 {
		t.Fatalf("read queue not empty")
	}
	sh, err = sp.take(ctx)
	if err != nil {
		t.Fatalf("cannot get session from session pool: %v", err)
	}
	sh.recycle()
}

// TestSessionHealthCheck tests healthchecking cases.
func TestSessionHealthCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				HealthCheckInterval:       50 * time.Millisecond,
				healthCheckSampleInterval: 10 * time.Millisecond,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	// Test pinging sessions.
	sh, err := sp.take(ctx)
	if err != nil {
		t.Fatalf("cannot get session from session pool: %v", err)
	}

	// Wait for healthchecker to send pings to session.
	waitFor(t, func() error {
		pings := server.testSpanner.DumpPings()
		if len(pings) == 0 || pings[0] != sh.getID() {
			return fmt.Errorf("healthchecker didn't send any ping to session %v", sh.getID())
		}
		return nil
	})
	// Test broken session detection.
	sh, err = sp.take(ctx)
	if err != nil {
		t.Fatalf("cannot get session from session pool: %v", err)
	}

	server.testSpanner.Freeze()
	server.testSpanner.PutExecutionTime(testutil.MethodGetSession,
		testutil.SimulatedExecutionTime{
			Errors:    []error{status.Errorf(codes.NotFound, "Session not found")},
			KeepError: true,
		})
	server.testSpanner.Unfreeze()
	//atomic.SwapInt64(&requestShouldErr, 1)

	// Wait for healthcheck workers to find the broken session and tear it down.
	// TODO(deklerk): get rid of this
	<-time.After(1 * time.Second)

	s := sh.session
	if sh.session.isValid() {
		t.Fatalf("session(%v) is still alive, want it to be dropped by healthcheck workers", s)
	}

	server.testSpanner.Freeze()
	server.testSpanner.PutExecutionTime(testutil.MethodGetSession, testutil.SimulatedExecutionTime{})
	server.testSpanner.Unfreeze()

	// Test garbage collection.
	sh, err = sp.take(ctx)
	if err != nil {
		t.Fatalf("cannot get session from session pool: %v", err)
	}
	sp.close()
	if sh.session.isValid() {
		t.Fatalf("session(%v) is still alive, want it to be garbage collected", s)
	}
}

// TestStressSessionPool does stress test on session pool by the following concurrent operations:
//	1) Test worker gets a session from the pool.
//	2) Test worker turns a session back into the pool.
//	3) Test worker destroys a session got from the pool.
//	4) Healthcheck destroys a broken session (because a worker has already destroyed it).
//	5) Test worker closes the session pool.
//
// During the test, the session pool maintainer maintains the number of sessions,
// and it is expected that all sessions that are taken from session pool remains valid.
// When all test workers and healthcheck workers exit, mockclient, session pool
// and healthchecker should be in consistent state.
func TestStressSessionPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Use concurrent workers to test different session pool built from different configurations.
	for ti, cfg := range []SessionPoolConfig{
		{},
		{MinOpened: 10, MaxOpened: 100},
		{MaxBurst: 50},
		{MinOpened: 10, MaxOpened: 200, MaxBurst: 5},
		{MinOpened: 10, MaxOpened: 200, MaxBurst: 5, WriteSessions: 0.2},
	} {
		var wg sync.WaitGroup
		// Create a more aggressive session healthchecker to increase test concurrency.
		cfg.HealthCheckInterval = 50 * time.Millisecond
		cfg.healthCheckSampleInterval = 10 * time.Millisecond
		cfg.HealthCheckWorkers = 50

		server, client := newSpannerInMemTestServerWithConfig(t,
			ClientConfig{
				SessionPoolConfig: cfg,
			})
		sp := client.idleSessions

		for i := 0; i < 100; i++ {
			wg.Add(1)
			// Schedule a test worker.
			go func(idx int, pool *sessionPool, client *Client) {
				defer wg.Done()
				// Test worker iterates 1K times and tries different
				// session / session pool operations.
				for j := 0; j < 1000; j++ {
					if idx%10 == 0 && j >= 900 {
						// Close the pool in selected set of workers during the
						// middle of the test.
						pool.close()
					}
					// Take a write sessions ~ 20% of the times.
					takeWrite := rand.Intn(5) == 4
					var (
						sh     *sessionHandle
						gotErr error
					)
					if takeWrite {
						sh, gotErr = pool.takeWriteSession(ctx)
					} else {
						sh, gotErr = pool.take(ctx)
					}
					if gotErr != nil {
						if pool.isValid() {
							t.Errorf("%v.%v: pool.take returns error when pool is still valid: %v", ti, idx, gotErr)
						}
						if wantErr := errInvalidSessionPool(); !testEqual(gotErr, wantErr) {
							t.Errorf("%v.%v: got error when pool is closed: %v, want %v", ti, idx, gotErr, wantErr)
						}
						continue
					}
					// Verify if session is valid when session pool is valid.
					// Note that if session pool is invalid after sh is taken,
					// then sh might be invalidated by healthcheck workers.
					if (sh.getID() == "" || sh.session == nil || !sh.session.isValid()) && pool.isValid() {
						t.Errorf("%v.%v.%v: pool.take returns invalid session %v", ti, idx, takeWrite, sh.session)
					}
					if takeWrite && sh.getTransactionID() == nil {
						t.Errorf("%v.%v: pool.takeWriteSession returns session %v without transaction", ti, idx, sh.session)
					}
					if rand.Intn(100) < idx {
						// Random sleep before destroying/recycling the session,
						// to give healthcheck worker a chance to step in.
						<-time.After(time.Duration(rand.Int63n(int64(cfg.HealthCheckInterval))))
					}
					if rand.Intn(100) < idx {
						// destroy the session.
						sh.destroy()
						continue
					}
					// recycle the session.
					sh.recycle()
				}
			}(i, sp, client)
		}
		wg.Wait()
		sp.hc.close()
		// Here the states of healthchecker, session pool and mockclient are
		// stable.
		idleSessions := map[string]bool{}
		hcSessions := map[string]bool{}
		mockSessions := server.testSpanner.DumpSessions()
		// Dump session pool's idle list.
		for sl := sp.idleList.Front(); sl != nil; sl = sl.Next() {
			s := sl.Value.(*session)
			if idleSessions[s.getID()] {
				t.Fatalf("%v: found duplicated session in idle list: %v", ti, s.getID())
			}
			idleSessions[s.getID()] = true
		}
		for sl := sp.idleWriteList.Front(); sl != nil; sl = sl.Next() {
			s := sl.Value.(*session)
			if idleSessions[s.getID()] {
				t.Fatalf("%v: found duplicated session in idle write list: %v", ti, s.getID())
			}
			idleSessions[s.getID()] = true
		}
		sp.mu.Lock()
		if int(sp.numOpened) != len(idleSessions) {
			t.Fatalf("%v: number of opened sessions (%v) != number of idle sessions (%v)", ti, sp.numOpened, len(idleSessions))
		}
		if sp.createReqs != 0 {
			t.Fatalf("%v: number of pending session creations = %v, want 0", ti, sp.createReqs)
		}
		// Dump healthcheck queue.
		for _, s := range sp.hc.queue.sessions {
			if hcSessions[s.getID()] {
				t.Fatalf("%v: found duplicated session in healthcheck queue: %v", ti, s.getID())
			}
			hcSessions[s.getID()] = true
		}
		sp.mu.Unlock()

		// Verify that idleSessions == hcSessions == mockSessions.
		if !testEqual(idleSessions, hcSessions) {
			t.Fatalf("%v: sessions in idle list (%v) != sessions in healthcheck queue (%v)", ti, idleSessions, hcSessions)
		}
		// The server may contain more sessions than the health check queue.
		// This can be caused by a timeout client side during a CreateSession
		// request. The request may still be received and executed by the
		// server, but the session pool will not register the session.
		for id, b := range hcSessions {
			if b && !mockSessions[id] {
				t.Fatalf("%v: session in healthcheck queue (%v) was not found on server", ti, id)
			}
		}
		sp.close()
		mockSessions = server.testSpanner.DumpSessions()
		for id, b := range hcSessions {
			if b && mockSessions[id] {
				t.Fatalf("Found session from pool still live on server: %v", id)
			}
		}
		server.teardown(client)
	}
}

// TODO(deklerk): Investigate why this test is flakey, even with waitFor. Example
// flakey failure: session_test.go:946: after 15s waiting, got Scale down.
// Expect 5 open, got 6
//
// TestMaintainer checks the session pool maintainer maintains the number of
// sessions in the following cases:
//
// 1. On initialization of session pool, replenish session pool to meet
//    MinOpened or MaxIdle.
// 2. On increased session usage, provision extra MaxIdle sessions.
// 3. After the surge passes, scale down the session pool accordingly.
func TestMaintainer(t *testing.T) {
	t.Skip("asserting session state seems flakey")
	t.Parallel()
	ctx := context.Background()

	minOpened := uint64(5)
	maxIdle := uint64(4)
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: SessionPoolConfig{
				MinOpened: minOpened,
				MaxIdle:   maxIdle,
			},
		})
	defer server.teardown(client)
	sp := client.idleSessions

	sampleInterval := sp.SessionPoolConfig.healthCheckSampleInterval

	waitFor(t, func() error {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		if sp.numOpened != 5 {
			return fmt.Errorf("Replenish. Expect %d open, got %d", sp.MinOpened, sp.numOpened)
		}
		return nil
	})

	// To save test time, we are not creating many sessions, because the time
	// to create sessions will have impact on the decision on sessionsToKeep.
	// We also parallelize the take and recycle process.
	shs := make([]*sessionHandle, 10)
	for i := 0; i < len(shs); i++ {
		var err error
		shs[i], err = sp.take(ctx)
		if err != nil {
			t.Fatalf("cannot get session from session pool: %v", err)
		}
	}
	sp.mu.Lock()
	if sp.numOpened != 10 {
		t.Fatalf("Scale out from normal use. Expect %d open, got %d", 10, sp.numOpened)
	}
	sp.mu.Unlock()

	<-time.After(sampleInterval)
	for _, sh := range shs[:7] {
		sh.recycle()
	}

	waitFor(t, func() error {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		if sp.numOpened != 7 {
			return fmt.Errorf("Keep extra MaxIdle sessions. Expect %d open, got %d", 7, sp.numOpened)
		}
		return nil
	})

	for _, sh := range shs[7:] {
		sh.recycle()
	}
	waitFor(t, func() error {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		if sp.numOpened != minOpened {
			return fmt.Errorf("Scale down. Expect %d open, got %d", minOpened, sp.numOpened)
		}
		return nil
	})
}

// Tests that maintainer creates up to MinOpened connections.
//
// Historical context: This test also checks that a low
// healthCheckSampleInterval does not prevent it from opening connections.
// The low healthCheckSampleInterval will however sometimes cause session
// creations to time out. That should not be considered a problem, but it
// could cause the test case to fail if it happens too often.
// See: https://github.com/googleapis/google-cloud-go/issues/1259
func TestMaintainer_CreatesSessions(t *testing.T) {
	t.Parallel()
	spc := SessionPoolConfig{
		MinOpened:                 10,
		MaxIdle:                   10,
		healthCheckSampleInterval: 20 * time.Millisecond,
	}
	server, client := newSpannerInMemTestServerWithConfig(t,
		ClientConfig{
			SessionPoolConfig: spc,
		})
	defer server.teardown(client)
	sp := client.idleSessions

	timeoutAmt := 4 * time.Second
	timeout := time.After(timeoutAmt)
	var numOpened uint64
loop:
	for {
		select {
		case <-timeout:
			t.Fatalf("timed out after %v, got %d session(s), want %d", timeoutAmt, numOpened, spc.MinOpened)
		default:
			sp.mu.Lock()
			numOpened = sp.numOpened
			sp.mu.Unlock()
			if numOpened == 10 {
				break loop
			}
		}
	}
}

func (s1 *session) Equal(s2 *session) bool {
	return s1.client == s2.client &&
		s1.id == s2.id &&
		s1.pool == s2.pool &&
		s1.createTime == s2.createTime &&
		s1.valid == s2.valid &&
		s1.hcIndex == s2.hcIndex &&
		s1.idleList == s2.idleList &&
		s1.nextCheck.Equal(s2.nextCheck) &&
		s1.checkingHealth == s2.checkingHealth &&
		testEqual(s1.md, s2.md) &&
		bytes.Equal(s1.tx, s2.tx)
}

func waitFor(t *testing.T, assert func() error) {
	t.Helper()
	timeout := 15 * time.Second
	ta := time.After(timeout)

	for {
		select {
		case <-ta:
			if err := assert(); err != nil {
				t.Fatalf("after %v waiting, got %v", timeout, err)
			}
			return
		default:
		}

		if err := assert(); err != nil {
			// Fail. Let's pause and retry.
			time.Sleep(10 * time.Millisecond)
			continue
		}

		return
	}
}
