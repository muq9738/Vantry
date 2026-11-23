package t3compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestOutgoingT3CommandsUseTheRPCPayloadContract(t *testing.T) {
	server, _, _ := testServer(t)
	ctx := context.Background()
	shell, err := server.shell(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config, err := server.config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	commands := make(chan Object, 16)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/auth/websocket-ticket/rpc":
			_ = json.NewEncoder(w).Encode(Object{"ticket": "test-ticket"})
		case "/api/orchestration/shell":
			_ = json.NewEncoder(w).Encode(shell)
		case "/api/orchestration/threads/seed":
			_ = json.NewEncoder(w).Encode(Object{"snapshotSequence": 1, "projection": Object{"runtimeRequests": []any{Object{"id": "request", "kind": "permission"}}}})
		case "/ws":
			conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, req, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			var request Object
			if conn.ReadJSON(&request) != nil {
				return
			}
			value := any(Object{"sequence": 1})
			if request["tag"] == "server.getConfig" {
				value = config
			} else {
				commands <- object(request["payload"])
			}
			_ = conn.WriteJSON([]any{Object{"_tag": "Exit", "requestId": request["id"], "exit": Object{"_tag": "Success", "value": value}}})
		default:
			http.NotFound(w, req)
		}
	}))
	defer remote.Close()
	adapter := &RemoteAdapter{Remote: Remote{URL: remote.URL, Token: "test-token", EnvironmentID: "test"}}
	pinned := true
	requests := []api.Command{
		{Kind: api.CreateThread, CommandID: "create", ThreadID: "new", Title: "New", ProviderID: "pi", Model: "test-model"},
		{Kind: api.SendMessage, CommandID: "send", ThreadID: "seed", Message: "Message"},
		{Kind: api.QueueMessage, CommandID: "queue", ThreadID: "seed", Message: "Later"},
		{Kind: api.SteerRun, CommandID: "steer", ThreadID: "seed", RunID: "run", Message: "Change"},
		{Kind: api.StopRun, CommandID: "stop", ThreadID: "seed", RunID: "run"},
		{Kind: api.SetThreadModel, CommandID: "model", ThreadID: "seed", ProviderID: "pi", Model: "test-model"},
		{Kind: api.RespondRequest, CommandID: "approval", ThreadID: "seed", RequestID: "request", Confirmed: true},
		{Kind: api.UpdateThread, CommandID: "pin", ThreadID: "seed", Pinned: &pinned},
		{Kind: api.UpdateThread, CommandID: "rename", ThreadID: "seed", Title: "Renamed"},
		{Kind: api.DeleteThread, CommandID: "delete", ThreadID: "seed"},
	}
	wires := []Object{}
	for _, request := range requests {
		if _, err = adapter.dispatch(ctx, request); err != nil {
			t.Fatal(request.Kind, err)
		}
		wire := <-commands
		if wire["type"] == nil || wire["commandId"] != request.CommandID || wire["command"] != nil {
			t.Fatalf("RPC payload is not a direct command: %+v", wire)
		}
		wires = append(wires, wire)
	}
	if path := os.Getenv("VANTRY_T3_COMMAND_FIXTURES"); path != "" {
		data, _ := json.Marshal(wires)
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
