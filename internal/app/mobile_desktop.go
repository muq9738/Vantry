package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"

	"github.com/muq9738/Vantry/internal/desktoprelay"
	"github.com/muq9738/Vantry/internal/t3compat"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

// The existing paired-device gateway owns both signaling routes. The phone
// receives neither the local viewer token nor the relay's registration key.
func (b *RemoteDesktopBridge) mobileDesktopTarget(ctx context.Context, selector string) (t3compat.DesktopTarget, error) {
	if err := ctx.Err(); err != nil {
		return t3compat.DesktopTarget{}, err
	}
	b.hostMu.Lock()
	root := b.host
	b.hostMu.Unlock()
	if root == nil || desktopHostError(root) != nil {
		return t3compat.DesktopTarget{}, t3compat.ErrDesktopNotSharing
	}
	host := root
	relayAvailable := false
	if automatic, ok := root.(*automaticDesktopHost); ok {
		relayAvailable = true
		host = automatic.direct
		if selector == "relay" {
			var err error
			host, err = automatic.relay(ctx)
			if err != nil {
				return t3compat.DesktopTarget{}, err
			}
		} else if fallback := automatic.currentRelay(); fallback != nil && selector == fallback.SessionID() {
			host = fallback
		}
	}
	if selector != "direct" && selector != "relay" && selector != host.SessionID() {
		return t3compat.DesktopTarget{}, errors.New("desktop session has ended")
	}
	current := func(ctx context.Context) error {
		b.hostMu.Lock()
		matches := b.host == root
		b.hostMu.Unlock()
		if !matches {
			return errors.New("desktop sharing has ended")
		}
		return ctx.Err()
	}
	if ticket, err := rtc.ParseLANTicket(host.Ticket()); err == nil {
		if selector == "relay" {
			return t3compat.DesktopTarget{}, errors.New("no desktop relay configured")
		}
		certificate, err := x509.ParseCertificate(ticket.Certificate)
		if err != nil {
			return t3compat.DesktopTarget{}, err
		}
		roots := x509.NewCertPool()
		roots.AddCert(certificate)
		// This is a local private listener with the certificate supplied by its
		// own host instance. Keep full chain, validity and hostname verification.
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
		return t3compat.DesktopTarget{
			Relay: "https://" + ticket.Address, SessionID: ticket.SessionID, Token: ticket.Token,
			Direct: true, RelayAvailable: relayAvailable, Transport: transport,
			Authorize: func(ctx context.Context, _ string) error { return current(ctx) },
		}, nil
	}
	owned, ok := host.(*ownedHost)
	if !ok {
		return t3compat.DesktopTarget{}, errors.New("mobile desktop transport is unavailable")
	}
	relay, ok := owned.relay.(*desktoprelay.HTTPRelay)
	if !ok {
		return t3compat.DesktopTarget{}, errors.New("mobile desktop transport is unavailable")
	}
	ticket, err := relay.ViewerAccess()
	if err != nil {
		return t3compat.DesktopTarget{}, err
	}
	return t3compat.DesktopTarget{
		Relay: ticket.Relay, SessionID: ticket.SessionID, Token: ticket.Token,
		Authorize: func(ctx context.Context, label string) error {
			if err := current(ctx); err != nil {
				return err
			}
			return relay.AuthorizePairedViewer(ctx, label)
		},
	}, nil
}
