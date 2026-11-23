package remoteaccess

import (
	"context"
	"errors"
	"net"
	"slices"
	"sort"
	"strconv"
	"time"
)

func directAddresses(address net.Addr, ifaceNames ...string) []string {
	tcp := address.(*net.TCPAddr)
	if tcp.IP != nil && tcp.IP.IsLoopback() {
		return []string{net.JoinHostPort(tcp.IP.String(), strconv.Itoa(tcp.Port))}
	}
	filterIface := ""
	if len(ifaceNames) > 0 {
		filterIface = ifaceNames[0]
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var result []string
	seen := map[string]bool{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if filterIface != "" && iface.Name != filterIface {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil || !validAdvertisedIP(ip) {
				continue
			}
			formatted := net.JoinHostPort(ip.String(), strconv.Itoa(tcp.Port))
			if !seen[formatted] {
				seen[formatted] = true
				result = append(result, formatted)
			}
		}
	}
	sort.Strings(result)
	return result
}

func (m *Manager) Discover(ctx context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.discovery != nil || m.opts.DisableDiscovery {
		return m.statusLocked(), nil
	}
	if err := ctx.Err(); err != nil {
		return m.statusLocked(), err
	}
	service, err := startMDNS(nil)
	if err != nil {
		m.discoveryError = "无法启动设备发现，请检查本地网络权限。"
		return m.statusLocked(), err
	}
	m.discovery = service
	go func() {
		seen := map[string]time.Time{}
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case device, ok := <-service.events:
				if !ok {
					return
				}
				m.applyNearbyDevice(device, seen)
			case <-ticker.C:
				m.mu.Lock()
				for id, last := range seen {
					if time.Since(last) > 65*time.Second {
						delete(m.nearby, id)
						delete(seen, id)
					}
				}
				m.mu.Unlock()
			case <-m.root.Done():
				service.close()
				return
			}
		}
	}()
	m.discoveryError = ""
	return m.statusLocked(), nil
}

func (m *Manager) applyNearbyDevice(device NearbyDevice, seen map[string]time.Time) {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	if device.Removed {
		delete(m.nearby, device.ID)
		if seen != nil {
			delete(seen, device.ID)
		}
		return
	}
	if seen == nil || len(m.nearby) < 64 || seen[device.ID] != (time.Time{}) {
		m.nearby[device.ID] = device
		if seen != nil {
			seen[device.ID] = time.Now()
		}
	}
	if device.ID == "" || len(device.Addresses) == 0 {
		return
	}
	changed := false
	for i, peer := range m.cfg.Peers {
		if peer.Pair.DeviceID == device.ID && !slices.Equal(peer.Pair.Direct, device.Addresses) {
			m.cfg.Peers[i].Pair.Direct = append([]string(nil), device.Addresses...)
			changed = true
		}
	}
	if changed {
		_ = saveConfig(m.opts.DataDir, m.cfg)
	}
}

func (m *Manager) Check(ctx context.Context, id, mode string) (Status, error) {
	m.mu.Lock()
	found := false
	for i, peer := range m.cfg.Peers {
		if peer.ID != id {
			continue
		}
		found = true
		if device, ok := m.nearby[peer.Pair.DeviceID]; ok {
			m.cfg.Peers[i].Pair.Direct = append([]string(nil), device.Addresses...)
			_ = saveConfig(m.opts.DataDir, m.cfg)
		}
		break
	}
	m.mu.Unlock()
	if !found {
		return m.Status(), errors.New("找不到已配对设备")
	}
	started := time.Now()
	client, err := ConnectWithMode(ctx, m.opts, id, mode)
	handshakeMs := int(time.Since(started).Milliseconds())
	reachable := err == nil
	checked := PeerInfo{Reachable: &reachable}
	if err != nil {
		var connErr *ConnectError
		if errors.As(err, &connErr) {
			checked.ErrorCode = connErr.Code
			checked.LastError = connErr.Message
		} else {
			checked.ErrorCode = classifyErrorCode(err)
			checked.LastError = err.Error()
		}
	}
	if client != nil {
		checked.Mode = client.Mode()
		if hs := client.HandshakeMs(); hs > 0 {
			checked.HandshakeMs = hs
		} else {
			checked.HandshakeMs = handshakeMs
		}
		rttStart := time.Now()
		if _, healthErr := client.Health(ctx); healthErr == nil {
			checked.RTTMs = int(time.Since(rttStart).Milliseconds())
			checked.LatencyMs = checked.RTTMs
		} else if rtt := client.RTTMs(); rtt > 0 {
			checked.RTTMs = rtt
			checked.LatencyMs = rtt
		}
		_ = client.Close()
	}
	m.mu.Lock()
	m.checks[id] = checked
	status := m.statusLocked()
	m.mu.Unlock()
	return status, nil
}
