package server

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testHealthyBackend(t *testing.T) string {
	t.Helper()

	_, target := testBackend(t, "ok", http.StatusOK)
	return target
}

func testDeploy(t *testing.T, router *Router, targets ...string) {
	t.Helper()

	require.NoError(t, router.DeployService("service1", targets, defaultEmptyReaders, defaultServiceOptions, defaultTargetOptions, defaultDeploymentOptions))
}

func testForgotten(targets ...string) []string {
	forgotten := []string{}
	for _, target := range targets {
		forgotten = append(forgotten, "service1/"+target)
	}
	return forgotten
}

func TestRouter_ForgetTarget_WhenADeployDropsAnAddress(t *testing.T) {
	fake := installFakeTracker(t)
	router := testRouter(t)
	a, b, c := testHealthyBackend(t), testHealthyBackend(t), testHealthyBackend(t)

	testDeploy(t, router, a, b)
	require.Empty(t, fake.forgotten())

	testDeploy(t, router, b, c)

	assert.Equal(t, testForgotten(a), fake.forgotten(), "b lives on, and keeps its history")
}

func TestRouter_ForgetTarget_WhenAServiceIsRemoved(t *testing.T) {
	fake := installFakeTracker(t)
	router := testRouter(t)
	a, b := testHealthyBackend(t), testHealthyBackend(t)

	testDeploy(t, router, a, b)
	require.NoError(t, router.RemoveService("service1"))

	assert.ElementsMatch(t, testForgotten(a, b), fake.forgotten())
}

func TestRouter_ForgetTarget_IncludesRolloutTargetsWhenAServiceIsRemoved(t *testing.T) {
	fake := installFakeTracker(t)
	router := testRouter(t)
	a, b := testHealthyBackend(t), testHealthyBackend(t)

	testDeploy(t, router, a)
	require.NoError(t, router.SetRolloutTargets("service1", []string{b}, defaultEmptyReaders, defaultDeploymentOptions))
	require.NoError(t, router.RemoveService("service1"))

	assert.ElementsMatch(t, testForgotten(a, b), fake.forgotten())
}

func TestRouter_ForgetTarget_NotOnAFailedDeploy(t *testing.T) {
	fake := installFakeTracker(t)
	router := testRouter(t)
	a := testHealthyBackend(t)
	_, unhealthy := testBackend(t, "down", http.StatusInternalServerError)

	testDeploy(t, router, a)

	options := defaultDeploymentOptions
	options.DeployTimeout = 50 * time.Millisecond
	err := router.DeployService("service1", []string{unhealthy}, defaultEmptyReaders, defaultServiceOptions, defaultTargetOptions, options)

	require.ErrorIs(t, err, ErrorTargetFailedToBecomeHealthy)
	assert.Empty(t, fake.forgotten())
}

func TestRouter_ForgetTarget_WhenARolloutDropsAnAddress(t *testing.T) {
	fake := installFakeTracker(t)
	router := testRouter(t)
	a, b, c := testHealthyBackend(t), testHealthyBackend(t), testHealthyBackend(t)

	testDeploy(t, router, a)
	require.NoError(t, router.SetRolloutTargets("service1", []string{b}, defaultEmptyReaders, defaultDeploymentOptions))
	require.NoError(t, router.SetRolloutTargets("service1", []string{c}, defaultEmptyReaders, defaultDeploymentOptions))

	assert.Equal(t, testForgotten(b), fake.forgotten())
}

// Promoting a rollout is a deploy of the rollout's address. The series it built
// up serving the other slot are still the live ones.
func TestRouter_ForgetTarget_KeepsAnAddressServingInTheOtherSlot(t *testing.T) {
	fake := installFakeTracker(t)
	router := testRouter(t)
	a, b := testHealthyBackend(t), testHealthyBackend(t)

	testDeploy(t, router, a)
	require.NoError(t, router.SetRolloutTargets("service1", []string{a}, defaultEmptyReaders, defaultDeploymentOptions))

	testDeploy(t, router, b)

	assert.Empty(t, fake.forgotten(), "a is still the rollout target")
}

// The deploy's drain is what lets the old target's requests finish, and they are
// still worth counting while it lasts: an address is forgotten once it is empty.
func TestRouter_ForgetTarget_WaitsForTheDrain(t *testing.T) {
	fake := installFakeTracker(t)
	router := testRouter(t)

	started := make(chan struct{})
	release := make(chan struct{})
	_, a := testBackendWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == DefaultHealthCheckPath {
			return
		}

		close(started)
		<-release
	})
	b := testHealthyBackend(t)

	testDeploy(t, router, a)

	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := testLoggedRequest("service1")
		router.ServeHTTP(httptest.NewRecorder(), req)
	}()

	select {
	case <-started:
	case <-time.After(testPoolArrivalTimeout):
		close(release)
		t.Fatal("the request never reached the target")
	}

	deployed := make(chan struct{})
	go func() {
		defer close(deployed)
		testDeploy(t, router, b)
	}()

	require.Eventually(t, func() bool {
		return router.serviceForName("service1").ActiveLoadBalancer().Targets().Names()[0] == b
	}, time.Second, time.Millisecond)
	assert.Empty(t, fake.forgotten(), "a is still draining")

	close(release)
	<-done
	<-deployed

	assert.Equal(t, testForgotten(a), fake.forgotten())
}

// Old and new target share one gauge when a redeploy keeps the address. The old
// one's last request reports zero as it ends, which must not stand as the count
// for an address the new target is busy on.
func TestRouter_RedeployToTheSameAddressLeavesTheLiveInFlightCount(t *testing.T) {
	fake := installFakeTracker(t)
	router := testRouter(t)

	startedOld, startedNew := make(chan struct{}), make(chan struct{})
	releaseOld, releaseNew := make(chan struct{}), make(chan struct{})

	_, a := testBackendWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			close(startedOld)
			<-releaseOld
		case "/new":
			close(startedNew)
			<-releaseNew
		}
	})

	send := func(path string) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			req, _ := testLoggedRequest("service1")
			req.URL.Path = path
			router.ServeHTTP(httptest.NewRecorder(), req)
		}()
		return done
	}

	testDeploy(t, router, a)
	original := router.serviceForName("service1").ActiveLoadBalancer()

	oldDone := send("/old")
	<-startedOld

	deployed := make(chan struct{})
	go func() {
		defer close(deployed)
		testDeploy(t, router, a)
	}()

	require.Eventually(t, func() bool {
		return router.serviceForName("service1").ActiveLoadBalancer() != original
	}, time.Second, time.Millisecond)

	newDone := send("/new")
	<-startedNew

	close(releaseOld)
	<-oldDone
	<-deployed

	count, ok := fake.targetInFlightCount("service1", a)
	assert.True(t, ok)
	assert.Equal(t, 1, count, "the new target is still holding its request")
	assert.Empty(t, fake.forgotten())

	close(releaseNew)
	<-newDone

	count, _ = fake.targetInFlightCount("service1", a)
	assert.Equal(t, 0, count)
}

// A drain gives up at its timeout, so a request can end after the deploy is over.
// By then its target has no say in a count that belongs to its successor.
func TestRouter_ARequestOutlivingTheDrainLeavesTheLiveInFlightCount(t *testing.T) {
	fake := installFakeTracker(t)
	router := testRouter(t)
	a := testHealthyBackend(t)

	testDeploy(t, router, a)
	replaced := router.serviceForName("service1").ActiveLoadBalancer().Targets()[0]

	straggler, _ := testLoggedRequest("service1")
	straggler, err := replaced.StartRequest(straggler)
	require.NoError(t, err)

	options := defaultDeploymentOptions
	options.DrainTimeout = time.Millisecond
	require.NoError(t, router.DeployService("service1", []string{a}, defaultEmptyReaders, defaultServiceOptions, defaultTargetOptions, options))

	current := router.serviceForName("service1").ActiveLoadBalancer().Targets()[0]
	require.NotSame(t, replaced, current)

	held, _ := testLoggedRequest("service1")
	held, err = current.StartRequest(held)
	require.NoError(t, err)
	defer current.endInflightRequest(held)

	replaced.endInflightRequest(straggler)

	count, ok := fake.targetInFlightCount("service1", a)
	assert.True(t, ok)
	assert.Equal(t, 1, count)
}

func TestRouter_ListActiveServices_TargetDetails(t *testing.T) {
	router := testRouter(t)
	reader := testHealthyBackend(t)
	_, writer := testBackendWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server-Timing", "queue;dur=12.5")
	})

	require.NoError(t, router.DeployService("service1", []string{writer}, []string{reader}, defaultServiceOptions, defaultTargetOptions, defaultDeploymentOptions))

	req := httptest.NewRequest(http.MethodPost, "http://example.com/", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)

	details := router.ListActiveServices()["service1"].TargetDetails
	require.Len(t, details, 2)

	assert.Equal(t, writer, details[0].Address)
	assert.Equal(t, "active", details[0].Slot)
	assert.Equal(t, "healthy", details[0].State)
	assert.Equal(t, 0, details[0].InFlight)
	require.NotNil(t, details[0].Queue)
	assert.Equal(t, 60, details[0].Queue.WindowSeconds)
	assert.Equal(t, 1, details[0].Queue.Requests)
	assert.Equal(t, 1, details[0].Queue.App)

	assert.Equal(t, reader, details[1].Address)
	assert.Equal(t, "active", details[1].Slot)
	assert.Equal(t, 0, details[1].Queue.Requests)
}

func TestRouter_ListActiveServices_TargetDetailsCoverTheRollout(t *testing.T) {
	router := testRouter(t)
	a, b := testHealthyBackend(t), testHealthyBackend(t)

	testDeploy(t, router, a)
	require.NoError(t, router.SetRolloutTargets("service1", []string{b}, defaultEmptyReaders, defaultDeploymentOptions))

	details := router.ListActiveServices()["service1"].TargetDetails
	require.Len(t, details, 2)

	assert.Equal(t, a, details[0].Address)
	assert.Equal(t, "active", details[0].Slot)

	assert.Equal(t, b, details[1].Address)
	assert.Equal(t, "rollout", details[1].Slot)
	assert.Equal(t, "healthy", details[1].State, "state stays the plain target state, so a reader can compare it")
}

func TestRouter_ListActiveServices_TargetDetailsInJSON(t *testing.T) {
	router := testRouter(t)
	_, target := testBackendWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server-Timing", "queue;dur=7")
	})

	testDeploy(t, router, target)
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/", nil))

	encoded, err := json.Marshal(ListResponse{Targets: router.ListActiveServices()})
	require.NoError(t, err)

	var decoded struct {
		Services map[string]struct {
			TargetDetails []struct {
				Address  string `json:"address"`
				Slot     string `json:"slot"`
				State    string `json:"state"`
				InFlight *int   `json:"in_flight"`
				Queue    struct {
					WindowSeconds int     `json:"window_seconds"`
					Requests      int     `json:"requests"`
					SumMs         float64 `json:"sum_ms"`
					MaxMs         float64 `json:"max_ms"`
					P95Ms         float64 `json:"p95_ms"`
					App           int     `json:"app"`
					Proxy         int     `json:"proxy"`
					Buckets       []struct {
						LeMs  float64 `json:"le_ms"`
						Count int     `json:"count"`
					} `json:"buckets"`
				} `json:"queue"`
			} `json:"target_details"`
		} `json:"services"`
	}
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	details := decoded.Services["service1"].TargetDetails
	require.Len(t, details, 1)
	assert.Equal(t, target, details[0].Address)
	assert.Equal(t, "active", details[0].Slot)
	assert.Equal(t, "healthy", details[0].State)
	require.NotNil(t, details[0].InFlight)
	assert.Equal(t, 60, details[0].Queue.WindowSeconds)
	assert.Equal(t, 1, details[0].Queue.Requests)
	assert.InDelta(t, 7, details[0].Queue.SumMs, 0.001)
	assert.InDelta(t, 7, details[0].Queue.MaxMs, 0.001)
	assert.Positive(t, details[0].Queue.P95Ms)
	assert.Equal(t, 1, details[0].Queue.App)
	assert.Equal(t, 0, details[0].Queue.Proxy)
	assert.Equal(t, float64(5000), details[0].Queue.Buckets[11].LeMs)
	assert.Equal(t, 1, details[0].Queue.Buckets[11].Count)
}

// legacyServiceDescription is ServiceDescription as a proxy before target_details
// knew it. The CLI and the server run different versions during a proxy
// replacement, in both directions.
type legacyServiceDescription struct {
	Host           string
	Path           string
	TLS            bool
	Target         string
	State          string
	Hosts          []string
	PathPrefixes   []string
	Targets        []string
	ReaderTargets  []string
	RolloutTargets []string
}

func TestServiceDescription_GobRoundTrip(t *testing.T) {
	window := newQueueWindow()
	window.Observe(testQueueNow, 12*time.Millisecond, queueSourceApp)
	queue := window.Snapshot(testQueueNow)

	sent := ServiceDescription{
		Host:   "example.com",
		Target: "web-1:3000",
		State:  "running",
		TargetDetails: []TargetDescription{
			{Address: "web-1:3000", Slot: "active", State: "healthy", InFlight: 2, Queue: &queue},
			{Address: "web-2:3000", Slot: "rollout", State: "adding", Queue: &QueueDescription{WindowSeconds: queueWindowSeconds}},
		},
	}

	var buffer bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buffer).Encode(sent))

	var received ServiceDescription
	require.NoError(t, gob.NewDecoder(&buffer).Decode(&received))

	assert.Equal(t, sent, received)
}

func TestServiceDescription_GobAcceptsAnOldServer(t *testing.T) {
	var buffer bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buffer).Encode(legacyServiceDescription{Host: "example.com", Target: "web-1:3000"}))

	var received ServiceDescription
	require.NoError(t, gob.NewDecoder(&buffer).Decode(&received))

	assert.Equal(t, "web-1:3000", received.Target)
	assert.Empty(t, received.TargetDetails)
}

func TestServiceDescription_GobAcceptsAnOldClient(t *testing.T) {
	sent := ServiceDescription{
		Target:        "web-1:3000",
		TargetDetails: []TargetDescription{{Address: "web-1:3000", Queue: &QueueDescription{WindowSeconds: queueWindowSeconds}}},
	}

	var buffer bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buffer).Encode(sent))

	var received legacyServiceDescription
	require.NoError(t, gob.NewDecoder(&buffer).Decode(&received))

	assert.Equal(t, "web-1:3000", received.Target)
}
