package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

const eventThreadUpdated = "thread.updated"

// dispatchSetThreadModel changes only the configuration inherited by future
// runs and delegates. An existing run, including its attempts and effects,
// keeps the configuration it captured when it was created.
func (c *Core) dispatchSetThreadModel(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ThreadID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "threadId is required for %s", api.SetThreadModel)
	}

	var receipt api.Receipt
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			receipt = prior
			return nil
		}

		thread, err := loadThread(t, cmd.ThreadID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "thread %q not found", cmd.ThreadID)
		}
		if err != nil {
			return wrapInternal("read thread", err)
		}
		model := firstNonEmpty(cmd.Model, c.defaultModel)
		providerID, err := c.switchedProvider(thread, model, cmd.ProviderID)
		if err != nil {
			return err
		}
		if _, err := t.exec(`UPDATE threads SET model = ?, provider_id = ? WHERE id = ?`, model, providerID, thread.ID); err != nil {
			return wrapInternal("update thread model", err)
		}
		thread.Model, thread.ProviderID = model, providerID
		ev, err := t.event(eventThreadUpdated, thread.ID, "", "", threadEventData{Thread: thread.toAPI()})
		if err != nil {
			return err
		}
		receipt = api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  thread.ID,
			Sequence:  ev.Sequence,
		}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	return receipt, nil
}

// switchedProvider is the provider a thread uses after choosing model. A
// thread created with the offline demo provider in a Core whose executor is Pi
// joins Pi when the user picks a real model; a demo-only Core still refuses.
func (c *Core) switchedProvider(thread threadRecord, model, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if !provider.KnownID(requested) {
			return "", errorf(api.InvalidCommand, "unsupported provider %q", requested)
		}
		if requested == "controlled" && thread.ProviderID != "controlled" && c.defaultProviderID != "controlled" {
			return "", errorf(api.InvalidCommand, "controlled threads do not support model switching")
		}
		return requested, nil
	}
	current := firstNonEmpty(thread.ProviderID, c.defaultProviderID)
	if !provider.KnownID(current) && c.defaultProviderID != "controlled" {
		return firstNonEmpty(c.defaultProviderID, "pi"), nil
	}
	if current != "controlled" || model == thread.Model {
		return thread.ProviderID, nil
	}
	if c.defaultProviderID == "" || c.defaultProviderID == "controlled" {
		return "", errorf(api.InvalidCommand, "controlled threads do not support model switching")
	}
	return c.defaultProviderID, nil
}
