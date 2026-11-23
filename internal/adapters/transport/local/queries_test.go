package local

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

type queryService struct{ *fakeService }

func (queryService) ListModels(context.Context) ([]api.ModelInfo, error) {
	return []api.ModelInfo{{ID: "example/model", Provider: "example", Name: "Example"}}, nil
}
func (queryService) InspectWorkspace(context.Context, string) (api.WorkspaceChanges, error) {
	return api.WorkspaceChanges{}, nil
}
func (queryService) OpenTerminal(context.Context, string) error { return nil }

func TestModelsEndpointBeforeCreatingAConversation(t *testing.T) {
	service := queryService{fakeService: &fakeService{snapshotFunc: func(context.Context) (api.Snapshot, error) {
		t.Error("model catalog must not require a thread snapshot")
		return api.Snapshot{}, nil
	}}}
	server := startTestServer(t, service)
	client := startTestClient(t, server)
	models, err := client.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "example/model" {
		t.Fatalf("new-session model catalog: %+v %v", models, err)
	}
}

type catalogTransport func(*http.Request) (*http.Response, error)

func (f catalogTransport) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestOldCoreModel404MeansServiceMismatch(t *testing.T) {
	client := Dial("unused-test-socket")
	defer client.Close()
	client.httpClient = &http.Client{Transport: catalogTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != "GET" || request.URL.Path != "/v1/models" {
			t.Fatalf("model picker requested a thread-bound endpoint: %s %s", request.Method, request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"not_found","message":"unknown endpoint"}`))}, nil
	})}
	if _, err := client.ListModels(context.Background()); api.CodeOf(err) != api.VersionMismatch {
		t.Fatalf("old Core endpoint absence misreported as missing conversation: %v", err)
	}
}

func TestMissingModelServiceDoesNotReportMissingConversation(t *testing.T) {
	server := startTestServer(t, &fakeService{})
	client := startTestClient(t, server)
	if _, err := client.ListModels(context.Background()); api.CodeOf(err) != api.VersionMismatch {
		t.Fatalf("missing catalog misclassified as conversation error: %v", err)
	}
}
