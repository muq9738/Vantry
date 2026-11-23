package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/attachments"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// importAttachment stores one Core-owned copy and returns its identity, the way
// Core hands it to the provider: an ID, never a client path.
func importAttachment(t *testing.T, dataDir, name string, data []byte) api.Attachment {
	t.Helper()
	item, err := (attachments.Store{Directory: filepath.Join(dataDir, "attachments")}).Import(name, data)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func waitFileContains(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s", want, path)
}

func TestRPCInitialPromptCarriesImagesAndText(t *testing.T) {
	p, request, config := newRPCProvider(t, "normal")
	request.Attachments = []api.Attachment{
		importAttachment(t, p.options.DataDir, "note.txt", []byte("attachment body")),
		importAttachment(t, p.options.DataDir, "pixel.png", []byte("\x89PNG\r\n\x1a\nimage-bytes")),
	}
	var textSeen bool
	if err := p.RunEvents(context.Background(), request, func(event provider.Event) error {
		if event.Delta != "" {
			textSeen = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !textSeen {
		t.Fatal("run produced no output")
	}
	waitFileContains(t, config.Commands, `"type":"prompt"`)
	raw := readFile(t, config.Commands)
	if !strings.Contains(raw, "Attached file") || !strings.Contains(raw, "attachment body") {
		t.Fatalf("text attachment was not sent as UTF-8 prompt content: %s", raw)
	}
	if !strings.Contains(raw, `"images"`) || !strings.Contains(raw, `"mimeType":"image/png"`) {
		t.Fatalf("image attachment was not sent as ImageContent: %s", raw)
	}
	// The prompt is written exactly once, and never as a CLI positional argument.
	if n := strings.Count(raw, `"type":"prompt"`); n != 1 {
		t.Fatalf("prompt count=%d", n)
	}
	if args := readFile(t, config.Arguments); strings.Contains(args, "attachment body") || strings.Contains(args, "@") {
		t.Fatalf("attachment leaked into CLI arguments: %s", args)
	}
}

func TestRPCSteerCarriesAttachmentsOrRefusesBeforeStdin(t *testing.T) {
	for _, scenario := range []string{"delivered", "unsupported-file", "missing-file"} {
		t.Run(scenario, func(t *testing.T) {
			p, request, config := newRPCProvider(t, "control")
			control := provider.Control{ID: "input:steer", Kind: "steer", Message: "reconsider"}
			switch scenario {
			case "delivered":
				control.Attachments = []api.Attachment{
					importAttachment(t, p.options.DataDir, "extra.txt", []byte("steer body")),
					importAttachment(t, p.options.DataDir, "extra.png", []byte("\x89PNG\r\n\x1a\nmore")),
				}
			case "unsupported-file":
				// Binary that Pi cannot accept as text and that is not an image.
				control.Attachments = []api.Attachment{
					importAttachment(t, p.options.DataDir, "blob.bin", []byte{0x00, 0x01, 0x02}),
				}
			case "missing-file":
				control.Attachments = []api.Attachment{{ID: "att_missing", Name: "gone.txt", MIME: "text/plain", Size: 4}}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []provider.Event
			handedOff := false
			_ = p.RunEvents(ctx, request, func(event provider.Event) error {
				events = append(events, event)
				if event.Type == "text_delta" && !handedOff {
					handedOff = true
					return p.Control(context.Background(), request, control)
				}
				if event.Type == "control_result" {
					var result provider.ControlResult
					_ = json.Unmarshal(event.Data, &result)
					// A refused handoff never settles the fixture, so stop
					// observing once its durable outcome exists. An accepted one
					// settles on its own.
					if result.Status != "delivered" {
						cancel()
					}
				}
				return nil
			})
			results := controlResults(t, events)
			result, ok := results[control.ID]
			if !ok {
				t.Fatalf("no control result: %+v", results)
			}
			if scenario == "delivered" {
				if result.Status != "delivered" {
					t.Fatalf("steer result=%+v", result)
				}
				waitFileContains(t, config.Commands, `"type":"steer"`)
				raw := readFile(t, config.Commands)
				if !strings.Contains(raw, "steer body") || !strings.Contains(raw, `"images"`) {
					t.Fatalf("steer attachments lost: %s", raw)
				}
			} else {
				if result.Status != "failed" || result.Error == "" {
					t.Fatalf("unsupported steer=%+v", result)
				}
				raw := readFile(t, config.Commands)
				if strings.Contains(raw, `"type":"steer"`) {
					t.Fatalf("refused steer reached Pi stdin: %s", raw)
				}
			}
		})
	}
}

func TestRPCControlCommandShapes(t *testing.T) {
	dataDir := t.TempDir()
	job := jobRequest{DataDir: dataDir, Request: provider.Request{Message: "hello"}}
	command, err := promptCommand(job)
	if err != nil {
		t.Fatal(err)
	}
	if command["id"] != "vantry:prompt" || command["type"] != "prompt" || command["message"] != "hello" {
		t.Fatalf("prompt command=%v", command)
	}
	steer, err := controlCommand(job, "vantry:control:x", provider.Control{ID: "id", Kind: "steer", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(steer)
	if !strings.Contains(string(raw), `"type":"steer"`) || !strings.Contains(string(raw), `"message":"go"`) {
		t.Fatalf("steer command=%s", raw)
	}
	if response, err := controlCommand(job, "pi:dialog", provider.Control{ID: "id", Kind: "response"}); err != nil || response != nil {
		t.Fatalf("response must not build a steer payload: %v %v", response, err)
	}
}

func TestRPCPayloadExtractsPDFTextAndRejectsInvalidPDF(t *testing.T) {
	dataDir := t.TempDir()
	validPDF := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Page >>\nendobj\n2 0 obj\n<< /Length 49 >>\nstream\nBT /F1 12 Tf 72 720 Td (Quarterly Spec PDF) Tj ET\nendstream\nendobj\n%%EOF\n")
	pdfItem := importAttachment(t, dataDir, "spec.pdf", validPDF)
	job := jobRequest{DataDir: dataDir, Request: provider.Request{Message: "Summarize [📄 spec.pdf · 160 B]", Attachments: []api.Attachment{pdfItem}}}
	payload, err := buildRPCPayload(job, job.Request.Message, job.Request.Attachments)
	if err != nil {
		t.Fatalf("valid PDF attachment failed: %v", err)
	}
	if !strings.Contains(payload.Message, `Attached PDF "spec.pdf"`) || !strings.Contains(payload.Message, "1 pages") || !strings.Contains(payload.Message, "Quarterly Spec PDF") {
		t.Fatalf("PDF text was not extracted into payload: %s", payload.Message)
	}

	fakePDF := importAttachment(t, dataDir, "corrupt.pdf", []byte{0x00, 0x01, 0x02, 0x03})
	if _, err := buildRPCPayload(job, "check", []api.Attachment{fakePDF}); err == nil {
		t.Fatal("expected non-PDF binary disguised as .pdf to be rejected")
	}
}
