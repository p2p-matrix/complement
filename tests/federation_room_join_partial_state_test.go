//go:build faster_joins
// +build faster_joins

// This file contains tests for joining rooms over federation, with the
// features introduced in msc2775.

package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/matrix-org/gomatrixserverlib"

	"github.com/matrix-org/complement/internal/b"
	"github.com/matrix-org/complement/internal/client"
	"github.com/matrix-org/complement/internal/docker"
	"github.com/matrix-org/complement/internal/federation"
	"github.com/matrix-org/complement/internal/match"
	"github.com/matrix-org/complement/internal/must"
)

func TestPartialStateJoin(t *testing.T) {
	// test that a regular /sync request made during a partial-state /send_join
	// request blocks until the state is correctly synced.
	t.Run("SyncBlocksDuringPartialStateJoin", func(t *testing.T) {
		deployment := Deploy(t, b.BlueprintAlice)
		defer deployment.Destroy(t)
		alice := deployment.Client(t, "hs1", "@alice:hs1")

		psjResult := beginPartialStateJoin(t, deployment, alice)
		defer psjResult.Destroy()

		// Alice has now joined the room, and the server is syncing the state in the background.

		// attempts to sync should now block. Fire off a goroutine to try it.
		syncResponseChan := make(chan gjson.Result)
		defer close(syncResponseChan)
		go func() {
			response, _ := alice.MustSync(t, client.SyncReq{})
			syncResponseChan <- response
		}()

		// wait for the state_ids request to arrive
		psjResult.AwaitStateIdsRequest(t)

		// the client-side requests should still be waiting
		select {
		case <-syncResponseChan:
			t.Fatalf("Sync completed before state resync complete")
		default:
		}

		// release the federation /state response
		psjResult.FinishStateRequest()

		// the /sync request should now complete, with the new room
		var syncRes gjson.Result
		select {
		case <-time.After(1 * time.Second):
			t.Fatalf("/sync request request did not complete")
		case syncRes = <-syncResponseChan:
		}

		roomRes := syncRes.Get("rooms.join." + client.GjsonEscape(psjResult.ServerRoom.RoomID))
		if !roomRes.Exists() {
			t.Fatalf("/sync completed without join to new room\n")
		}

		// check that the state includes both charlie and derek.
		matcher := match.JSONCheckOffAllowUnwanted("state.events",
			[]interface{}{
				"m.room.member|" + psjResult.Server.UserID("charlie"),
				"m.room.member|" + psjResult.Server.UserID("derek"),
			}, func(result gjson.Result) interface{} {
				return strings.Join([]string{result.Map()["type"].Str, result.Map()["state_key"].Str}, "|")
			}, nil,
		)
		if err := matcher([]byte(roomRes.Raw)); err != nil {
			t.Errorf("Did not find expected state events in /sync response: %s", err)

		}
	})

	// when Alice does a lazy-loading sync, she should see the room immediately
	t.Run("CanLazyLoadingSyncDuringPartialStateJoin", func(t *testing.T) {
		deployment := Deploy(t, b.BlueprintAlice)
		defer deployment.Destroy(t)
		alice := deployment.Client(t, "hs1", "@alice:hs1")

		psjResult := beginPartialStateJoin(t, deployment, alice)
		defer psjResult.Destroy()

		alice.MustSyncUntil(t,
			client.SyncReq{
				Filter: buildLazyLoadingSyncFilter(nil),
			},
			client.SyncJoinedTo(alice.UserID, psjResult.ServerRoom.RoomID),
		)
		t.Logf("Alice successfully synced")
	})

	// we should be able to send events in the room, during the resync
	t.Run("CanSendEventsDuringPartialStateJoin", func(t *testing.T) {
		t.Skip("Cannot yet send events during resync")
		deployment := Deploy(t, b.BlueprintAlice)
		defer deployment.Destroy(t)
		alice := deployment.Client(t, "hs1", "@alice:hs1")

		psjResult := beginPartialStateJoin(t, deployment, alice)
		defer psjResult.Destroy()

		alice.Client.Timeout = 2 * time.Second
		paths := []string{"_matrix", "client", "v3", "rooms", psjResult.ServerRoom.RoomID, "send", "m.room.message", "0"}
		res := alice.MustDoFunc(t, "PUT", paths, client.WithJSONBody(t, map[string]interface{}{
			"msgtype": "m.text",
			"body":    "Hello world!",
		}))
		body := gjson.ParseBytes(client.ParseJSON(t, res))
		eventID := body.Get("event_id").Str
		t.Logf("Alice sent event event ID %s", eventID)
	})

	// we should be able to receive events over federation during the resync
	t.Run("CanReceiveEventsDuringPartialStateJoin", func(t *testing.T) {
		deployment := Deploy(t, b.BlueprintAlice)
		defer deployment.Destroy(t)
		alice := deployment.Client(t, "hs1", "@alice:hs1")

		psjResult := beginPartialStateJoin(t, deployment, alice)
		defer psjResult.Destroy()

		// the HS will make an /event_auth request for the event
		federation.HandleEventAuthRequests()(psjResult.Server)

		// derek sends an event in the room
		event := psjResult.Server.MustCreateEvent(t, psjResult.ServerRoom, b.Event{
			Type:   "m.room.message",
			Sender: psjResult.Server.UserID("derek"),
			Content: map[string]interface{}{
				"msgtype": "m.text",
				"body":    "Message",
			},
		})
		psjResult.ServerRoom.AddEvent(event)
		psjResult.Server.MustSendTransaction(t, deployment, "hs1", []json.RawMessage{event.JSON()}, nil)
		t.Logf("Derek sent event event ID %s", event.EventID())

		/* TODO: check that a lazy-loading sync can see the event. Currently this doesn't work, because /sync blocks.
		 * https://github.com/matrix-org/synapse/issues/13146
		alice.MustSyncUntil(t,
			client.SyncReq{
				Filter: buildLazyLoadingSyncFilter(nil),
			},
			client.SyncTimelineHasEventID(psjResult.ServerRoom.RoomID, event.EventID()),
		)
		*/

		// still, Alice should be able to see the event with an /event request. We might have to try it a few times.
		start := time.Now()
		for {
			if time.Since(start) > time.Second {
				t.Fatalf("timeout waiting for received event to be visible")
			}
			res := alice.DoFunc(t, "GET", []string{"_matrix", "client", "r0", "rooms", psjResult.ServerRoom.RoomID, "event", event.EventID()})
			eventResBody := client.ParseJSON(t, res)
			if res.StatusCode == 200 {
				t.Logf("Successfully fetched received event %s", event.EventID())
				break
			}
			if res.StatusCode == 404 && gjson.GetBytes(eventResBody, "errcode").String() == "M_NOT_FOUND" {
				t.Logf("Fetching received event failed with M_NOT_FOUND; will retry")
				time.Sleep(100 * time.Millisecond)
				continue
			}
			t.Fatalf("GET /event failed with %d: %s", res.StatusCode, string(eventResBody))
		}

		// allow the partial join to complete
		psjResult.FinishStateRequest()
		alice.MustSyncUntil(t,
			client.SyncReq{},
			client.SyncJoinedTo(alice.UserID, psjResult.ServerRoom.RoomID),
		)

		// check the server's idea of the state at the event. We do this by making a `state_ids` request over federation
		stateReq := gomatrixserverlib.NewFederationRequest("GET", "hs1",
			fmt.Sprintf("/_matrix/federation/v1/state_ids/%s?event_id=%s",
				url.PathEscape(psjResult.ServerRoom.RoomID),
				url.QueryEscape(event.EventID()),
			),
		)
		var respStateIDs gomatrixserverlib.RespStateIDs
		if err := psjResult.Server.SendFederationRequest(deployment, stateReq, &respStateIDs); err != nil {
			t.Errorf("/state_ids request returned non-200: %s", err)
			return
		}
		var gotState, expectedState []interface{}
		for _, ev := range respStateIDs.StateEventIDs {
			gotState = append(gotState, ev)
		}
		for _, ev := range psjResult.ServerRoom.AllCurrentState() {
			expectedState = append(expectedState, ev.EventID())
		}
		must.CheckOffAll(t, gotState, expectedState)
	})

	// a request to (client-side) /members?at= should block until the (federation) /state request completes
	// TODO(faster_joins): also need to test /state, and /members without an `at`, which follow a different path
	t.Run("MembersRequestBlocksDuringPartialStateJoin", func(t *testing.T) {
		deployment := Deploy(t, b.BlueprintAlice)
		defer deployment.Destroy(t)
		alice := deployment.Client(t, "hs1", "@alice:hs1")

		psjResult := beginPartialStateJoin(t, deployment, alice)
		defer psjResult.Destroy()

		// we need a sync token to pass to the `at` param.
		syncToken := alice.MustSyncUntil(t,
			client.SyncReq{
				Filter: buildLazyLoadingSyncFilter(nil),
			},
			client.SyncJoinedTo(alice.UserID, psjResult.ServerRoom.RoomID),
		)
		t.Logf("Alice successfully synced")

		// Fire off a goroutine to send the request, and write the response back to a channel.
		clientMembersRequestResponseChan := make(chan *http.Response)
		defer close(clientMembersRequestResponseChan)
		go func() {
			queryParams := url.Values{}
			queryParams.Set("at", syncToken)
			clientMembersRequestResponseChan <- alice.MustDoFunc(
				t,
				"GET",
				[]string{"_matrix", "client", "v3", "rooms", psjResult.ServerRoom.RoomID, "members"},
				client.WithQueries(queryParams),
			)
		}()

		// release the federation /state response
		psjResult.FinishStateRequest()

		// the client-side /members request should now complete, with a response that includes charlie and derek.
		select {
		case <-time.After(1 * time.Second):
			t.Fatalf("client-side /members request did not complete")
		case res := <-clientMembersRequestResponseChan:
			must.MatchResponse(t, res, match.HTTPResponse{
				JSON: []match.JSON{
					match.JSONCheckOff("chunk",
						[]interface{}{
							"m.room.member|" + alice.UserID,
							"m.room.member|" + psjResult.Server.UserID("charlie"),
							"m.room.member|" + psjResult.Server.UserID("derek"),
						}, func(result gjson.Result) interface{} {
							return strings.Join([]string{result.Map()["type"].Str, result.Map()["state_key"].Str}, "|")
						}, nil),
				},
			})
		}
	})

	// test that a partial-state join continues syncing state after a restart
	// the same as SyncBlocksDuringPartialStateJoin, with a restart in the middle
	t.Run("PartialStateJoinContinuesAfterRestart", func(t *testing.T) {
		deployment := Deploy(t, b.BlueprintAlice)
		defer deployment.Destroy(t)
		alice := deployment.Client(t, "hs1", "@alice:hs1")

		psjResult := beginPartialStateJoin(t, deployment, alice)
		defer psjResult.Destroy()

		// Alice has now joined the room, and the server is syncing the state in the background.

		// wait for the state_ids request to arrive
		psjResult.AwaitStateIdsRequest(t)

		// restart the homeserver
		err := deployment.Restart(t)
		if err != nil {
			t.Errorf("Failed to restart homeserver: %s", err)
		}

		// attempts to sync should block. Fire off a goroutine to try it.
		syncResponseChan := make(chan gjson.Result)
		defer close(syncResponseChan)
		go func() {
			response, _ := alice.MustSync(t, client.SyncReq{})
			syncResponseChan <- response
		}()

		// we expect another state_ids request to arrive.
		// we'd do another AwaitStateIdsRequest, except it's single-use.

		// the client-side requests should still be waiting
		select {
		case <-syncResponseChan:
			t.Fatalf("Sync completed before state resync complete")
		default:
		}

		// release the federation /state response
		psjResult.FinishStateRequest()

		// the /sync request should now complete, with the new room
		var syncRes gjson.Result
		select {
		case <-time.After(1 * time.Second):
			t.Fatalf("/sync request request did not complete")
		case syncRes = <-syncResponseChan:
		}

		roomRes := syncRes.Get("rooms.join." + client.GjsonEscape(psjResult.ServerRoom.RoomID))
		if !roomRes.Exists() {
			t.Fatalf("/sync completed without join to new room\n")
		}
	})

	// test that a partial-state join can fall back to other homeservers when re-syncing
	// partial state.
	t.Run("PartialStateJoinSyncsUsingOtherHomeservers", func(t *testing.T) {
		// set up 3 homeservers: hs1, hs2 and complement
		deployment := Deploy(t, b.BlueprintFederationTwoLocalOneRemote)
		defer deployment.Destroy(t)
		alice := deployment.Client(t, "hs1", "@alice:hs1")
		charlie := deployment.Client(t, "hs2", "@charlie:hs2")

		// create a public room
		roomID := alice.CreateRoom(t, map[string]interface{}{
			"preset": "public_chat",
		})

		// create the complement homeserver
		server := federation.NewServer(t, deployment,
			federation.HandleKeyRequests(),
			federation.HandlePartialStateMakeSendJoinRequests(),
			federation.HandleEventRequests(),
			federation.HandleTransactionRequests(
				func(e *gomatrixserverlib.Event) {
					t.Fatalf("Received unexpected PDU: %s", string(e.JSON()))
				},
				// hs1 may send us presence when alice syncs
				nil,
			),
		)
		cancelListener := server.Listen()
		defer cancelListener()

		// join complement to the public room
		room := server.MustJoinRoom(t, deployment, "hs1", roomID, server.UserID("david"))

		// we expect a /state_ids request from hs2 after it joins the room
		// we will respond to the request with garbage
		fedStateIdsRequestReceivedWaiter := NewWaiter()
		fedStateIdsSendResponseWaiter := NewWaiter()
		server.Mux().Handle(
			fmt.Sprintf("/_matrix/federation/v1/state_ids/%s", roomID),
			http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				queryParams := req.URL.Query()
				t.Logf("Incoming state_ids request for event %s in room %s", queryParams["event_id"], roomID)
				fedStateIdsRequestReceivedWaiter.Finish()
				fedStateIdsSendResponseWaiter.Wait(t, 60*time.Second)
				t.Logf("Replying to /state_ids request with invalid response")

				w.WriteHeader(200)

				if _, err := w.Write([]byte("{}")); err != nil {
					t.Errorf("Error writing to request: %v", err)
				}
			}),
		).Methods("GET")

		// join charlie on hs2 to the room, via the complement homeserver
		charlie.JoinRoom(t, roomID, []string{server.ServerName()})

		// and let hs1 know that charlie has joined,
		// otherwise hs1 will refuse /state_ids requests
		member_event := room.CurrentState("m.room.member", charlie.UserID).JSON()
		server.MustSendTransaction(t, deployment, "hs1", []json.RawMessage{member_event}, nil)
		alice.MustSyncUntil(t, client.SyncReq{}, client.SyncJoinedTo(charlie.UserID, roomID))

		// wait until hs2 starts syncing state
		fedStateIdsRequestReceivedWaiter.Waitf(t, 5*time.Second, "Waiting for /state_ids request")

		syncResponseChan := make(chan gjson.Result)
		defer close(syncResponseChan)
		go func() {
			response, _ := charlie.MustSync(t, client.SyncReq{})
			syncResponseChan <- response
		}()

		// the client-side requests should still be waiting
		select {
		case <-syncResponseChan:
			t.Fatalf("hs2 sync completed before state resync complete")
		default:
		}

		// reply to hs2 with a bogus /state_ids response
		fedStateIdsSendResponseWaiter.Finish()

		// charlie's /sync request should now complete, with the new room
		var syncRes gjson.Result
		select {
		case <-time.After(1 * time.Second):
			t.Fatalf("hs2 /sync request request did not complete")
		case syncRes = <-syncResponseChan:
		}

		roomRes := syncRes.Get("rooms.join." + client.GjsonEscape(roomID))
		if !roomRes.Exists() {
			t.Fatalf("hs2 /sync completed without join to new room\n")
		}
	})

	// test a lazy-load-members sync while re-syncing partial state, followed by completion of state syncing,
	// followed by a gappy sync. the gappy sync should include the correct member state,
	// since it was not sent on the previous sync.
	t.Run("GappySyncAfterPartialStateSynced", func(t *testing.T) {
		deployment := Deploy(t, b.BlueprintAlice)
		defer deployment.Destroy(t)
		alice := deployment.Client(t, "hs1", "@alice:hs1")

		psjResult := beginPartialStateJoin(t, deployment, alice)
		defer psjResult.Destroy()

		// get a sync token before state syncing finishes.
		syncToken := alice.MustSyncUntil(t,
			client.SyncReq{
				Filter: buildLazyLoadingSyncFilter(nil),
			},
			client.SyncJoinedTo(alice.UserID, psjResult.ServerRoom.RoomID),
		)
		t.Logf("Alice successfully synced")

		// wait for partial state to finish syncing,
		// by waiting for the room to show up in a regular /sync.
		psjResult.AwaitStateIdsRequest(t)
		psjResult.FinishStateRequest()
		alice.MustSyncUntil(t,
			client.SyncReq{},
			client.SyncJoinedTo(alice.UserID, psjResult.ServerRoom.RoomID),
		)

		// make derek send two messages into the room.
		// we will do a gappy sync after, which will only pick up the last message.
		var lastEventID string
		for i := 0; i < 2; i++ {
			event := psjResult.Server.MustCreateEvent(t, psjResult.ServerRoom, b.Event{
				Type:   "m.room.message",
				Sender: psjResult.Server.UserID("derek"),
				Content: map[string]interface{}{
					"msgtype": "m.text",
					"body":    "Message " + strconv.Itoa(i),
				},
			})
			lastEventID = event.EventID()
			psjResult.ServerRoom.AddEvent(event)
			psjResult.Server.MustSendTransaction(t, deployment, "hs1", []json.RawMessage{event.JSON()}, nil)
		}

		// wait for the events to come down a regular /sync.
		alice.MustSyncUntil(t,
			client.SyncReq{},
			client.SyncTimelineHasEventID(psjResult.ServerRoom.RoomID, lastEventID),
		)

		// now do a gappy sync using the sync token from before.
		syncRes, _ := alice.MustSync(t,
			client.SyncReq{
				Since: syncToken,
				Filter: buildLazyLoadingSyncFilter(map[string]interface{}{
					"limit": 1,
				}),
			},
		)

		// check that the state includes derek.
		roomRes := syncRes.Get("rooms.join." + client.GjsonEscape(psjResult.ServerRoom.RoomID))
		if !roomRes.Exists() {
			t.Fatalf("/sync completed without join to new room\n")
		}
		t.Logf("gappy /sync response for %s: %s", psjResult.ServerRoom.RoomID, roomRes)

		timelineMatcher := match.JSONCheckOff("timeline.events",
			[]interface{}{lastEventID},
			func(result gjson.Result) interface{} {
				return result.Map()["event_id"].Str
			}, nil,
		)
		stateMatcher := match.JSONCheckOffAllowUnwanted("state.events",
			[]interface{}{
				"m.room.member|" + psjResult.Server.UserID("derek"),
			}, func(result gjson.Result) interface{} {
				return strings.Join([]string{result.Map()["type"].Str, result.Map()["state_key"].Str}, "|")
			}, nil,
		)
		if err := timelineMatcher([]byte(roomRes.Raw)); err != nil {
			t.Errorf("Unexpected timeline events found in gappy /sync response: %s", err)
		}
		if err := stateMatcher([]byte(roomRes.Raw)); err != nil {
			t.Errorf("Did not find derek's m.room.member event in gappy /sync response: %s", err)
		}
	})
}

// buildLazyLoadingSyncFilter constructs a json-marshalled filter suitable the 'Filter' field of a client.SyncReq
func buildLazyLoadingSyncFilter(timelineOptions map[string]interface{}) string {
	timelineFilter := map[string]interface{}{
		"lazy_load_members": true,
	}

	for k, v := range timelineOptions {
		timelineFilter[k] = v
	}

	j, _ := json.Marshal(map[string]interface{}{
		"room": map[string]interface{}{
			"timeline": timelineFilter,
			"state": map[string]interface{}{
				"lazy_load_members": true,
			},
		},
	})
	return string(j)
}

// partialStateJoinResult is the result of beginPartialStateJoin
type partialStateJoinResult struct {
	cancelListener                   func()
	Server                           *federation.Server
	ServerRoom                       *federation.ServerRoom
	fedStateIdsRequestReceivedWaiter *Waiter
	fedStateIdsSendResponseWaiter    *Waiter
}

// beginPartialStateJoin spins up a room on a complement server,
// then has a test user join it. It returns a partialStateJoinResult,
// which must be Destroy'd on completion.
//
// When this method completes, the /join request will have completed, but the
// state has not yet been re-synced. To allow the re-sync to proceed, call
// partialStateJoinResult.FinishStateRequest.
func beginPartialStateJoin(t *testing.T, deployment *docker.Deployment, joiningUser *client.CSAPI) partialStateJoinResult {
	result := partialStateJoinResult{}
	success := false
	defer func() {
		if !success {
			result.Destroy()
		}
	}()

	result.Server = federation.NewServer(t, deployment,
		federation.HandleKeyRequests(),
		federation.HandlePartialStateMakeSendJoinRequests(),
		federation.HandleEventRequests(),
		federation.HandleTransactionRequests(
			func(e *gomatrixserverlib.Event) {
				t.Fatalf("Received unexpected PDU: %s", string(e.JSON()))
			},
			// the homeserver under test may send us presence when the joining user syncs
			nil,
		),
	)
	result.cancelListener = result.Server.Listen()

	// some things for orchestration
	result.fedStateIdsRequestReceivedWaiter = NewWaiter()
	result.fedStateIdsSendResponseWaiter = NewWaiter()

	// create the room on the complement server, with charlie and derek as members
	roomVer := joiningUser.GetDefaultRoomVersion(t)
	result.ServerRoom = result.Server.MustMakeRoom(t, roomVer, federation.InitialRoomEvents(roomVer, result.Server.UserID("charlie")))
	result.ServerRoom.AddEvent(result.Server.MustCreateEvent(t, result.ServerRoom, b.Event{
		Type:     "m.room.member",
		StateKey: b.Ptr(result.Server.UserID("derek")),
		Sender:   result.Server.UserID("derek"),
		Content: map[string]interface{}{
			"membership": "join",
		},
	}))

	// register a handler for /state_ids requests for the most recent event,
	// which finishes fedStateIdsRequestReceivedWaiter, then
	// waits for fedStateIdsSendResponseWaiter and sends a reply
	lastEvent := result.ServerRoom.Timeline[len(result.ServerRoom.Timeline)-1]
	currentState := result.ServerRoom.AllCurrentState()
	handleStateIdsRequests(
		t, result.Server, result.ServerRoom,
		lastEvent.EventID(), currentState,
		result.fedStateIdsRequestReceivedWaiter, result.fedStateIdsSendResponseWaiter,
	)

	// a handler for /state requests, which sends a sensible response
	handleStateRequests(
		t, result.Server, result.ServerRoom,
		lastEvent.EventID(), currentState,
		nil, nil,
	)

	// have joiningUser join the room by room ID.
	joiningUser.JoinRoom(t, result.ServerRoom.RoomID, []string{result.Server.ServerName()})
	t.Logf("/join request completed")

	success = true
	return result
}

// Destroy cleans up the resources associated with the join attempt. It must
// be called once the test is finished
func (psj *partialStateJoinResult) Destroy() {
	if psj.fedStateIdsSendResponseWaiter != nil {
		psj.fedStateIdsSendResponseWaiter.Finish()
	}

	if psj.fedStateIdsRequestReceivedWaiter != nil {
		psj.fedStateIdsRequestReceivedWaiter.Finish()
	}

	if psj.cancelListener != nil {
		psj.cancelListener()
	}
}

// wait for a /state_ids request for the test room to arrive
func (psj *partialStateJoinResult) AwaitStateIdsRequest(t *testing.T) {
	psj.fedStateIdsRequestReceivedWaiter.Waitf(t, 5*time.Second, "Waiting for /state_ids request")
}

// allow the /state_ids request to complete, thus allowing the state re-sync to complete
func (psj *partialStateJoinResult) FinishStateRequest() {
	psj.fedStateIdsSendResponseWaiter.Finish()
}

// handleStateIdsRequests registers a handler for /state_ids requests for 'eventID'
//
// the returned state is as passed in 'roomState'
//
// if requestReceivedWaiter is not nil, it will be Finish()ed when the request arrives.
// if sendResponseWaiter is not nil, we will Wait() for it to finish before sending the response.
func handleStateIdsRequests(
	t *testing.T, srv *federation.Server, serverRoom *federation.ServerRoom,
	eventID string, roomState []*gomatrixserverlib.Event,
	requestReceivedWaiter *Waiter, sendResponseWaiter *Waiter,
) {
	srv.Mux().NewRoute().Methods("GET").Path(
		fmt.Sprintf("/_matrix/federation/v1/state_ids/%s", serverRoom.RoomID),
	).Queries("event_id", eventID).Handler(
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			queryParams := req.URL.Query()
			t.Logf("Incoming state_ids request for event %s in room %s", queryParams["event_id"], serverRoom.RoomID)
			if requestReceivedWaiter != nil {
				requestReceivedWaiter.Finish()
			}
			if sendResponseWaiter != nil {
				sendResponseWaiter.Waitf(t, 60*time.Second, "Waiting for /state_ids request")
			}
			t.Logf("Replying to /state_ids request")

			res := gomatrixserverlib.RespStateIDs{
				AuthEventIDs:  eventIDsFromEvents(serverRoom.AuthChainForEvents(roomState)),
				StateEventIDs: eventIDsFromEvents(roomState),
			}
			w.WriteHeader(200)
			jsonb, _ := json.Marshal(res)

			if _, err := w.Write(jsonb); err != nil {
				t.Errorf("Error writing to request: %v", err)
			}
		}),
	)
	t.Logf("Registered state_ids handler for event %s", eventID)
}

// makeStateHandler returns a handler for /state requests for 'eventID'
//
// the returned state is as passed in 'roomState'
//
// if requestReceivedWaiter is not nil, it will be Finish()ed when the request arrives.
// if sendResponseWaiter is not nil, we will Wait() for it to finish before sending the response.
func handleStateRequests(
	t *testing.T, srv *federation.Server, serverRoom *federation.ServerRoom,
	eventID string, roomState []*gomatrixserverlib.Event,
	requestReceivedWaiter *Waiter, sendResponseWaiter *Waiter,
) {
	srv.Mux().NewRoute().Methods("GET").Path(
		fmt.Sprintf("/_matrix/federation/v1/state/%s", serverRoom.RoomID),
	).Queries("event_id", eventID).Handler(
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			queryParams := req.URL.Query()
			t.Logf("Incoming state request for event %s in room %s", queryParams["event_id"], serverRoom.RoomID)
			if requestReceivedWaiter != nil {
				requestReceivedWaiter.Finish()
			}
			if sendResponseWaiter != nil {
				sendResponseWaiter.Waitf(t, 60*time.Second, "Waiting for /state request")
			}
			res := gomatrixserverlib.RespState{
				AuthEvents:  gomatrixserverlib.NewEventJSONsFromEvents(serverRoom.AuthChainForEvents(roomState)),
				StateEvents: gomatrixserverlib.NewEventJSONsFromEvents(roomState),
			}
			w.WriteHeader(200)
			jsonb, _ := json.Marshal(res)

			if _, err := w.Write(jsonb); err != nil {
				t.Errorf("Error writing to request: %v", err)
			}
		}),
	)
}

func eventIDsFromEvents(he []*gomatrixserverlib.Event) []string {
	eventIDs := make([]string, len(he))
	for i := range he {
		eventIDs[i] = he[i].EventID()
	}
	return eventIDs
}
