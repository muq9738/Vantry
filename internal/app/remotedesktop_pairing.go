package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/muq9738/Vantry/internal/desktoprelay"
)

type desktopPeer struct {
	Input  string              `json:"input"`
	Ticket desktoprelay.Ticket `json:"ticket"`
}

// Remember the device credential, rather than redeeming a consumed code on every
// reconnect. The sharing host still confirms each new viewing/control request.
func (b *RemoteDesktopBridge) desktopPair(ctx context.Context, relay, input string) (desktoprelay.Ticket, error) {
	b.mu.Lock()
	if b.desktopPeers == nil {
		b.desktopPeers = []desktopPeer{}
		if b.dataDir != "" {
			data, err := os.ReadFile(filepath.Join(b.dataDir, "remote-desktop", "paired.json"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				b.mu.Unlock()
				return desktoprelay.Ticket{}, err
			}
			if len(data) > 0 && json.Unmarshal(data, &b.desktopPeers) != nil {
				b.mu.Unlock()
				return desktoprelay.Ticket{}, errors.New("已配对桌面设备记录无效，请检查本机数据目录")
			}
		}
	}
	for _, peer := range b.desktopPeers {
		if peer.Input == input && (relay == "" || peer.Ticket.Relay == relay) {
			ticket := peer.Ticket
			b.mu.Unlock()
			return ticket, nil
		}
	}
	b.mu.Unlock()
	ticket, err := desktoprelay.Redeem(ctx, relay, input)
	if err != nil {
		return ticket, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, peer := range b.desktopPeers {
		if peer.Ticket.Relay == ticket.Relay && peer.Ticket.SessionID == ticket.SessionID {
			b.desktopPeers = append(b.desktopPeers[:i:i], b.desktopPeers[i+1:]...)
			break
		}
	}
	b.desktopPeers = append(b.desktopPeers, desktopPeer{Input: input, Ticket: ticket})
	if len(b.desktopPeers) > 32 {
		b.desktopPeers = b.desktopPeers[len(b.desktopPeers)-32:]
	}
	if b.dataDir != "" {
		directory := filepath.Join(b.dataDir, "remote-desktop")
		if err = os.MkdirAll(directory, 0700); err != nil {
			return ticket, err
		}
		data, _ := json.Marshal(b.desktopPeers)
		file, err := os.CreateTemp(directory, ".paired-*")
		if err != nil {
			return ticket, err
		}
		defer os.Remove(file.Name())
		if err = file.Chmod(0600); err == nil {
			_, err = file.Write(data)
		}
		closeErr := file.Close()
		if err != nil {
			return ticket, err
		}
		if closeErr != nil {
			return ticket, closeErr
		}
		if err = os.Rename(file.Name(), filepath.Join(directory, "paired.json")); err != nil {
			return ticket, err
		}
	}
	return ticket, nil
}
