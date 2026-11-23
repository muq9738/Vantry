package desktoprelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/muq9738/Vantry/internal/connectivity"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
	"github.com/pion/webrtc/v4"
)

func TestDesktopApprovalSignalingAndTURNOverWSS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	key := connectivity.Token()
	server, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	pairings, err := connectivity.NewRelay(connectivity.RelayConfig{RegistrationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer pairings.Close()
	mux := http.NewServeMux()
	mux.Handle("/desktop/", server)
	mux.Handle("/desktop/pairings", http.StripPrefix("/desktop", pairings))
	mux.Handle("/desktop/pairings/", http.StripPrefix("/desktop", pairings))
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()
	host, err := NewClient(httpServer.URL, key, false)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	video, _ := rtc.Quality("balanced")
	hostInfo, err := host.Create(ctx, video)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Delete(context.Background(), hostInfo.SessionID, hostInfo.HostToken)
	code := host.Pairing().Code
	if !strings.HasPrefix(code, "DESK-") {
		t.Fatal("desktop code missing")
	}
	if _, err := Redeem(ctx, httpServer.URL, strings.Replace(code, "DESK-", "SESSION-", 1)); err == nil {
		t.Fatal("session code accepted for desktop")
	}
	ticket, err := Redeem(ctx, "", host.Ticket())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Redeem(ctx, httpServer.URL, code); err == nil {
		t.Fatal("desktop code reused")
	}
	viewer, err := NewClient(ticket.Relay, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	viewInfo := make(chan rtc.SessionInfo, 1)
	viewError := make(chan error, 1)
	go func() {
		info, err := viewer.Get(ctx, ticket.SessionID, ticket.Token)
		if err != nil {
			viewError <- err
		} else {
			viewInfo <- info
		}
	}()
	for deadline := time.Now().Add(3 * time.Second); ; {
		pending, err := host.Pending(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if pending != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("viewer request not visible")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-viewInfo:
		t.Fatal("viewer bypassed local host approval")
	default:
	}
	if err = host.Approve(ctx, true); err != nil {
		t.Fatal(err)
	}
	var viewerInfo rtc.SessionInfo
	select {
	case viewerInfo = <-viewInfo:
	case err = <-viewError:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("approval did not unblock viewer")
	}
	if hostInfo.Transport != "wss-tcp" || viewerInfo.Transport != "wss-tcp" {
		t.Fatal("transport mislabeled")
	}
	hostSignal, err := host.Signal(ctx, hostInfo.SessionID, "host", hostInfo.HostToken)
	if err != nil {
		t.Fatal(err)
	}
	defer hostSignal.Close()
	viewerSignal, err := viewer.Signal(ctx, ticket.SessionID, "viewer", ticket.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer viewerSignal.Close()
	// The native RTC client trickles "ice" messages after SDP. Verify both
	// directions and end-of-candidates instead of relying only on bundled SDP.
	for _, direction := range []struct {
		name     string
		from, to *rtc.SignalConn
	}{{"host to viewer", hostSignal, viewerSignal}, {"viewer to host", viewerSignal, hostSignal}} {
		candidate := &webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 1 127.0.0.1 50000 typ relay"}
		if err := direction.from.Send(ctx, rtc.SignalMessage{Type: "ice", Candidate: candidate}); err != nil {
			t.Fatal(err)
		}
		if err := direction.from.Send(ctx, rtc.SignalMessage{Type: "ice"}); err != nil {
			t.Fatal(err)
		}
		_ = direction.to.Conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			message, err := direction.to.Read()
			if err != nil {
				t.Fatalf("%s: native ICE candidate not forwarded: %v", direction.name, err)
			}
			if message.Type != "ice" {
				continue
			}
			if message.Candidate == nil || message.Candidate.Candidate != candidate.Candidate {
				t.Fatalf("%s: ICE candidate was altered", direction.name)
			}
			break
		}
		end, err := direction.to.Read()
		if err != nil || end.Type != "ice" || end.Candidate != nil {
			t.Fatalf("%s: ICE completion not forwarded: %v", direction.name, err)
		}
		_ = direction.to.Conn.SetReadDeadline(time.Time{})
	}
	if err = hostSignal.Send(ctx, rtc.SignalMessage{Type: "offer", SDP: "test signaling"}); err != nil {
		t.Fatal(err)
	}
	for {
		message, err := viewerSignal.Read()
		if err != nil {
			t.Fatal(err)
		}
		if message.Type == "offer" {
			if message.SDP != "test signaling" {
				t.Fatal(message)
			}
			break
		}
	}
	// Two real relay-only ICE agents must exchange encrypted data through the
	// in-process WSS/TCP gateways and the embedded TURN allocations.
	a, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: hostInfo.ICEServers, ICETransportPolicy: webrtc.ICETransportPolicyRelay})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: viewerInfo.ICEServers, ICETransportPolicy: webrtc.ICETransportPolicyRelay})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	received := make(chan string, 1)
	b.OnDataChannel(func(channel *webrtc.DataChannel) {
		channel.OnMessage(func(message webrtc.DataChannelMessage) { received <- string(message.Data) })
	})
	channel, err := a.CreateDataChannel("native-desktop", nil)
	if err != nil {
		t.Fatal(err)
	}
	channel.OnOpen(func() { _ = channel.SendText("vantry-native-turn") })
	offer, err := a.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherA := webrtc.GatheringCompletePromise(a)
	if err = a.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gatherA:
	case <-ctx.Done():
		t.Fatal("host relay ICE gathering timed out")
	}
	if !strings.Contains(a.LocalDescription().SDP, "typ relay") {
		t.Fatal("host has no TURN candidate")
	}
	if err = b.SetRemoteDescription(*a.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := b.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherB := webrtc.GatheringCompletePromise(b)
	if err = b.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gatherB:
	case <-ctx.Done():
		t.Fatal("viewer relay ICE gathering timed out")
	}
	if !strings.Contains(b.LocalDescription().SDP, "typ relay") {
		t.Fatal("viewer has no TURN candidate")
	}
	if err = a.SetRemoteDescription(*b.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-received:
		if message != "vantry-native-turn" {
			t.Fatal(message)
		}
	case <-ctx.Done():
		t.Fatal("TURN/WSS did not carry the real encrypted connection")
	}
	if err = host.Delete(ctx, hostInfo.SessionID, hostInfo.HostToken); err != nil {
		t.Fatal(err)
	}
	if err = connectivity.Request(ctx, nil, "GET", httpServer.URL, "/desktop/v1/sessions/"+ticket.SessionID, ticket.Token, nil, &rtc.SessionInfo{}); err == nil {
		t.Fatal("revoked desktop remained accessible")
	}
}

func TestViewerNeedsApprovalBeforeSignalingAndRevocationClosesMedia(t *testing.T) {
	key := connectivity.Token()
	relay, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	server := httptest.NewServer(relay)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var info rtc.SessionInfo
	if err = connectivity.Request(ctx, nil, "POST", server.URL, "/desktop/v1/sessions", key, map[string]any{"video": rtc.VideoConfig{Width: 1280, Height: 720, FPS: 30}}, &info); err != nil {
		t.Fatal(err)
	}
	header := http.Header{"Authorization": {"Bearer " + info.ViewerToken}}
	endpoint := strings.Replace(server.URL, "http", "ws", 1) + "/desktop/v1/sessions/" + info.SessionID
	conn, response, err := websocket.DefaultDialer.Dial(endpoint+"/signal?role=viewer", header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("unapproved viewer opened signaling")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("unapproved viewer not rejected", response, err)
	}
	_ = response.Body.Close()
	relay.mu.Lock()
	ghost := relay.rooms[info.SessionID].peers["viewer"] != nil
	relay.mu.Unlock()
	if ghost {
		t.Fatal("rejected viewer occupied the signaling slot")
	}
	if err = connectivity.Request(ctx, nil, "POST", server.URL, "/desktop/v1/sessions/"+info.SessionID+"/request", info.ViewerToken, map[string]string{"label": "Viewer"}, nil); err != nil {
		t.Fatal(err)
	}
	if err = connectivity.Request(ctx, nil, "POST", server.URL, "/desktop/v1/sessions/"+info.SessionID+"/approve", info.HostToken, map[string]bool{"allow": true}, nil); err != nil {
		t.Fatal(err)
	}
	conn, response, err = websocket.DefaultDialer.Dial(endpoint+"/turn", header)
	if response != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = connectivity.Request(ctx, nil, "POST", server.URL, "/desktop/v1/sessions/"+info.SessionID+"/approve", info.HostToken, map[string]bool{"allow": false}, nil); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = conn.ReadMessage(); err == nil {
		t.Fatal("revoked media tunnel remained open")
	}
	if err, ok := err.(interface{ Timeout() bool }); ok && err.Timeout() {
		t.Fatal("revoked media tunnel did not close promptly")
	}

	relay.mu.Lock()
	relay.activeTunnels = maxDesktopTunnels
	relay.mu.Unlock()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/desktop/v1/sessions/"+info.SessionID+"/turn", nil)
	req.Header.Set("Authorization", "Bearer "+info.HostToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when activeTunnels at cap, got %d", resp.StatusCode)
	}
	relay.mu.Lock()
	relay.activeTunnels = 0
	relay.mu.Unlock()
}
