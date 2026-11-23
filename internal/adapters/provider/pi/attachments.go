package pi

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/muq9738/Vantry/internal/adapters/attachments"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

type rpcImage struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MIMEType string `json:"mimeType"`
}

// rpcPayload is the prompt/steer body written to Pi. RPC forbids CLI @file
// arguments, so only Core-owned attachments are resolved: supported images
// travel as ImageContent and textual files become part of the message. A
// binary attachment that Pi cannot take is an explicit error, never dropped.
type rpcPayload struct {
	Message string     `json:"message"`
	Images  []rpcImage `json:"images,omitempty"`
}

func (p rpcPayload) apply(command map[string]any) {
	command["message"] = p.Message
	if len(p.Images) > 0 {
		command["images"] = p.Images
	}
}

func buildRPCPayload(job jobRequest, text string, items []api.Attachment) (rpcPayload, error) {
	payload := rpcPayload{Message: text}
	store := attachments.Store{Directory: filepath.Join(job.DataDir, "attachments")}
	for _, item := range items {
		path, err := store.Resolve(item)
		if err != nil {
			return payload, fmt.Errorf("attachment: %w", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return payload, err
		}
		mediaType := strings.ToLower(strings.TrimSpace(item.MIME))
		if parsed, _, parseErr := mime.ParseMediaType(mediaType); parseErr == nil && parsed != "" {
			mediaType = strings.ToLower(parsed)
		}
		if mediaType == "application/x-pdf" || (strings.EqualFold(filepath.Ext(item.Name), ".pdf") && bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-"))) {
			mediaType = "application/pdf"
		}
		switch mediaType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			payload.Images = append(payload.Images, rpcImage{
				Type: "image", MIMEType: mediaType, Data: base64.StdEncoding.EncodeToString(data),
			})
		case "application/pdf":
			extracted, pages, err := extractPDFText(data)
			if err != nil {
				return payload, fmt.Errorf("attachment %q is not a valid PDF: %w", item.Name, err)
			}
			header := fmt.Sprintf("Attached PDF %q (%s", item.Name, formatPDFSize(item.Size))
			if pages > 0 {
				header += fmt.Sprintf(", %d pages", pages)
			}
			header += fmt.Sprintf(", path: %s):", path)
			if strings.TrimSpace(extracted) != "" {
				payload.Message += fmt.Sprintf("\n\n%s\n%s", header, extracted)
			} else {
				payload.Message += fmt.Sprintf("\n\n%s\n[PDF contains no embedded text layer; inspect %s directly if needed.]", header, path)
			}
		default:
			if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
				return payload, fmt.Errorf("attachment %q is not a supported image, PDF, or UTF-8 text file", item.Name)
			}
			payload.Message += fmt.Sprintf("\n\nAttached file %q:\n%s", item.Name, data)
		}
	}
	return payload, nil
}

// promptCommand builds the one initial prompt command. It is never written twice.
func promptCommand(job jobRequest) (map[string]any, error) {
	payload, err := buildRPCPayload(job, job.Request.Message, job.Request.Attachments)
	if err != nil {
		return nil, err
	}
	command := map[string]any{"id": "vantry:prompt", "type": "prompt"}
	payload.apply(command)
	return command, nil
}

// controlCommand builds one steer or dialog response command. A steer keeps the
// Core-owned attachment identities; a dialog response carries no attachments.
func controlCommand(job jobRequest, rpcID string, control provider.Control) (map[string]any, error) {
	if control.Kind != "steer" {
		return nil, nil
	}
	payload, err := buildRPCPayload(job, control.Message, control.Attachments)
	if err != nil {
		return nil, err
	}
	command := map[string]any{"id": rpcID, "type": "steer"}
	payload.apply(command)
	return command, nil
}
