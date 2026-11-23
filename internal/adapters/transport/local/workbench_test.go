package local

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

type workbenchFake struct {
	*fakeService
	work func(context.Context, api.WorkbenchRequest) (api.WorkbenchResponse, error)
}

func (f workbenchFake) Workbench(ctx context.Context, r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	return f.work(ctx, r)
}

func TestWorkbenchCancellationPropagatesAcrossSocket(t *testing.T) {
	entered, finished := make(chan struct{}), make(chan struct{})
	service := workbenchFake{fakeService: &fakeService{}, work: func(ctx context.Context, _ api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		close(entered)
		<-ctx.Done()
		close(finished)
		return api.WorkbenchResponse{}, ctx.Err()
	}}
	server := startTestServer(t, service)
	client := startTestClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "plugins.list"}); result <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("workbench call did not enter service")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("client cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not return")
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("service did not receive cancellation")
	}
}

func TestWorkbenchRejectsMalformedOrOversizedInput(t *testing.T) {
	called := false
	server := &Server{service: workbenchFake{fakeService: &fakeService{}, work: func(context.Context, api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		called = true
		return api.WorkbenchResponse{}, nil
	}}}
	for name, body := range map[string]string{"null": "null", "unknown-field": `{"action":"models","credential":"unexpected"}`, "multiple": `{"action":"models"} {}`, "too-large": `{"action":"models","name":"` + strings.Repeat("a", maxWorkbenchBytes) + `"}`} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/workbench", strings.NewReader(body))
			response := httptest.NewRecorder()
			server.handleWorkbench(response, request)
			if response.Code != http.StatusBadRequest || called {
				t.Fatalf("invalid request reached service: code=%d called=%v", response.Code, called)
			}
		})
	}
}
