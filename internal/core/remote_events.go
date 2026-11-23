package core

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/muq9738/Vantry/pkg/provider"
)

// Remote entities are projected under the local proxy attempt. Their original
// IDs are retained only as routing references; a peer can never overwrite a
// local tool/dialog that happens to have the same ID.
func (c *Core) applyRemoteEvent(t *txn, eff effect, event provider.Event) error {
	switch event.Type {
	case "remote.tool.updated":
		var data toolEventData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		tool := data.Tool
		id := eff.attemptID + ":remote:" + tool.ID
		_, err := loadTool(t, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return c.saveTool(t, toolRecord{ID: id, ThreadID: eff.threadID, RunID: eff.runID, AttemptID: eff.attemptID,
			Name: tool.Name, Arguments: tool.Arguments, Output: tool.Output, Status: tool.Status, Offset: tool.OutputOffset}, err == nil)
	case "remote.request.updated":
		var data requestEventData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		req := data.Request
		id := eff.attemptID + ":remote:" + req.ID
		record, err := loadRequest(t, id)
		if errors.Is(err, sql.ErrNoRows) {
			return c.insertRequest(t, requestRecord{ID: id, ProviderRequestID: req.ID, ThreadID: eff.threadID, RunID: eff.runID,
				AttemptID: eff.attemptID, Method: req.Method, Title: req.Title, Message: req.Message, Options: req.Options,
				Prefill: req.Prefill, Status: req.Status, Error: req.Error})
		}
		if err != nil {
			return err
		}
		record.Status, record.Error = req.Status, req.Error
		return c.updateRequest(t, record)
	case "remote.input.updated":
		var data inputEventData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		input := data.Input
		// A remote steer uses the local InputID as an explicit business key.
		// Only a matching current proxy attempt may acknowledge it.
		const prefix = "remote-control:"
		if len(input.CommandID) <= len(prefix) || input.CommandID[:len(prefix)] != prefix {
			return nil
		}
		id := input.CommandID[len(prefix):]
		local, err := loadInput(t, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if local.ThreadID != eff.threadID || local.RunID != eff.runID || local.AttemptID != eff.attemptID {
			return nil
		}
		if local.Status != inputPending && local.Status != inputUnknown {
			return nil
		}
		switch input.Status {
		case "delivered", "failed", "unknown":
			local.Status, local.Error = input.Status, input.Error
			return c.updateInput(t, local)
		}
	}
	return nil
}
