package remoteaccess

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const mdnsType = "_vantry._tcp.local."

var (
	mdnsIPv4Addr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	mdnsIPv6Addr = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}
)

func utf8Truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

type mdnsAddrEntry struct {
	ip        net.IP
	expiresAt time.Time
}

type mdnsServiceEntry struct {
	instance  string
	id        string
	name      string
	target    string
	port      int
	expiresAt time.Time
}

type mdnsCache struct {
	services map[string]*mdnsServiceEntry
	addrs    map[string][]mdnsAddrEntry
}

func newMDNSCache() *mdnsCache {
	return &mdnsCache{
		services: map[string]*mdnsServiceEntry{},
		addrs:    map[string][]mdnsAddrEntry{},
	}
}

func validAdvertisedIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return ip.IsGlobalUnicast() || ip.IsPrivate()
}

func (c *mdnsCache) ingest(message dnsmessage.Message, now time.Time) []NearbyDevice {
	records := append(append([]dnsmessage.Resource{}, message.Answers...), message.Additionals...)
	records = append(records, message.Authorities...)

	touchedTargets := map[string]bool{}
	touchedInstances := map[string]bool{}
	var removed []NearbyDevice

	for _, record := range records {
		name := record.Header.Name.String()
		ttl := time.Duration(record.Header.TTL) * time.Second
		switch body := record.Body.(type) {
		case *dnsmessage.TXTResource:
			if !strings.HasSuffix(name, "."+mdnsType) {
				continue
			}
			var id, devName string
			for _, item := range body.TXT {
				k, v, _ := strings.Cut(item, "=")
				if k == "id" {
					id = v
				} else if k == "name" {
					devName = v
				}
			}
			if id == "" || len(id) > 128 || len(devName) > 256 {
				continue
			}
			if record.Header.TTL == 0 {
				if existing := c.services[name]; existing != nil {
					id = existing.id
				}
				delete(c.services, name)
				removed = append(removed, NearbyDevice{ID: id, Name: devName, Removed: true})
				continue
			}
			entry := c.services[name]
			if entry == nil {
				entry = &mdnsServiceEntry{instance: name}
				c.services[name] = entry
			}
			entry.id = id
			if devName != "" {
				entry.name = devName
			}
			entry.expiresAt = now.Add(ttl)
			touchedInstances[name] = true
		case *dnsmessage.SRVResource:
			if !strings.HasSuffix(name, "."+mdnsType) {
				continue
			}
			if record.Header.TTL == 0 {
				if existing := c.services[name]; existing != nil {
					delete(c.services, name)
					if existing.id != "" {
						removed = append(removed, NearbyDevice{ID: existing.id, Name: existing.name, Removed: true})
					}
				}
				continue
			}
			entry := c.services[name]
			if entry == nil {
				entry = &mdnsServiceEntry{instance: name}
				c.services[name] = entry
			}
			entry.port = int(body.Port)
			entry.target = body.Target.String()
			entry.expiresAt = now.Add(ttl)
			touchedInstances[name] = true
		case *dnsmessage.AResource:
			ip := net.IP(body.A[:]).To4()
			if ip == nil {
				continue
			}
			c.updateAddr(name, ip, record.Header.TTL, now)
			touchedTargets[name] = true
		case *dnsmessage.AAAAResource:
			ip := net.IP(body.AAAA[:])
			if ip == nil || ip.To4() != nil {
				continue
			}
			c.updateAddr(name, ip, record.Header.TTL, now)
			touchedTargets[name] = true
		}
	}

	for inst, entry := range c.services {
		if touchedTargets[entry.target] {
			touchedInstances[inst] = true
		}
	}

	var updates []NearbyDevice
	updates = append(updates, removed...)
	for inst := range touchedInstances {
		entry := c.services[inst]
		if entry == nil || entry.id == "" || entry.port <= 0 || entry.target == "" || !now.Before(entry.expiresAt) {
			continue
		}
		var addrs []string
		for _, rec := range c.addrs[entry.target] {
			if !now.Before(rec.expiresAt) || !validAdvertisedIP(rec.ip) {
				continue
			}
			formatted := net.JoinHostPort(rec.ip.String(), strconv.Itoa(entry.port))
			if !slices.Contains(addrs, formatted) {
				addrs = append(addrs, formatted)
			}
		}
		if len(addrs) == 0 {
			continue
		}
		slices.Sort(addrs)
		updates = append(updates, NearbyDevice{
			ID:        entry.id,
			Name:      entry.name,
			Addresses: addrs,
			Port:      entry.port,
		})
	}
	return updates
}

func (c *mdnsCache) updateAddr(target string, ip net.IP, ttl uint32, now time.Time) {
	if !validAdvertisedIP(ip) {
		return
	}
	existing := c.addrs[target]
	filtered := existing[:0]
	for _, rec := range existing {
		if !rec.ip.Equal(ip) && now.Before(rec.expiresAt) {
			filtered = append(filtered, rec)
		}
	}
	if ttl > 0 {
		filtered = append(filtered, mdnsAddrEntry{
			ip:        append(net.IP(nil), ip...),
			expiresAt: now.Add(time.Duration(ttl) * time.Second),
		})
	}
	if len(filtered) == 0 {
		delete(c.addrs, target)
	} else {
		c.addrs[target] = filtered
	}
}

func (c *mdnsCache) prune(now time.Time) []string {
	for target, list := range c.addrs {
		kept := list[:0]
		for _, rec := range list {
			if now.Before(rec.expiresAt) {
				kept = append(kept, rec)
			}
		}
		if len(kept) == 0 {
			delete(c.addrs, target)
		} else {
			c.addrs[target] = kept
		}
	}
	var expired []string
	for inst, entry := range c.services {
		if !now.Before(entry.expiresAt) || len(c.addrs[entry.target]) == 0 {
			if entry.id != "" && !slices.Contains(expired, entry.id) {
				expired = append(expired, entry.id)
			}
			delete(c.services, inst)
		}
	}
	return expired
}

type mdnsService struct {
	conn4   *net.UDPConn
	conn6   *net.UDPConn
	p4      *ipv4.PacketConn
	p6      *ipv6.PacketConn
	ifaces  []net.Interface
	done    chan struct{}
	events  chan NearbyDevice
	once    sync.Once
	wg      sync.WaitGroup
	mu      sync.Mutex
	answers []dnsmessage.Resource
	cache   *mdnsCache
}

func dnsName(value string) dnsmessage.Name { name, _ := dnsmessage.NewName(value); return name }

func buildMDNSAnswers(name, id string, addresses []string, port int) []dnsmessage.Resource {
	shortID := id[:min(len(id), 16)]
	instance := dnsName("vantry-" + shortID + "." + mdnsType)
	host := dnsName("vantry-" + shortID + ".local.")
	header := func(n dnsmessage.Name, kind dnsmessage.Type) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: n, Type: kind, Class: dnsmessage.ClassINET, TTL: 60}
	}
	answers := []dnsmessage.Resource{
		{Header: header(dnsName(mdnsType), dnsmessage.TypePTR), Body: &dnsmessage.PTRResource{PTR: instance}},
		{Header: header(instance, dnsmessage.TypeSRV), Body: &dnsmessage.SRVResource{Port: uint16(port), Target: host}},
		{Header: header(instance, dnsmessage.TypeTXT), Body: &dnsmessage.TXTResource{TXT: []string{"id=" + id, "name=" + utf8Truncate(name, 240)}}},
	}
	for _, address := range addresses {
		raw, _, err := net.SplitHostPort(address)
		if err != nil {
			continue
		}
		ip := net.ParseIP(raw)
		if ip == nil {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			answers = append(answers, dnsmessage.Resource{Header: header(host, dnsmessage.TypeA), Body: &dnsmessage.AResource{A: [4]byte(ip4)}})
		} else if ip16 := ip.To16(); ip16 != nil {
			answers = append(answers, dnsmessage.Resource{Header: header(host, dnsmessage.TypeAAAA), Body: &dnsmessage.AAAAResource{AAAA: [16]byte(ip16)}})
		}
	}
	return answers
}

func multicastInterfaces() []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, iface := range all {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		out = append(out, iface)
	}
	return out
}

func startMDNS(answers []dnsmessage.Resource) (*mdnsService, error) {
	ifaces := multicastInterfaces()
	var conn4, conn6 *net.UDPConn
	var p4 *ipv4.PacketConn
	var p6 *ipv6.PacketConn

	c4, err4 := net.ListenMulticastUDP("udp4", nil, mdnsIPv4Addr)
	if err4 == nil {
		conn4 = c4
		p4 = ipv4.NewPacketConn(c4)
		for i := range ifaces {
			_ = p4.JoinGroup(&ifaces[i], mdnsIPv4Addr)
		}
	}
	c6, err6 := net.ListenMulticastUDP("udp6", nil, mdnsIPv6Addr)
	if err6 == nil {
		conn6 = c6
		p6 = ipv6.NewPacketConn(c6)
		for i := range ifaces {
			_ = p6.JoinGroup(&ifaces[i], mdnsIPv6Addr)
		}
	}
	if conn4 == nil && conn6 == nil {
		if err4 != nil {
			return nil, err4
		}
		return nil, err6
	}
	s := &mdnsService{
		conn4:   conn4,
		conn6:   conn6,
		p4:      p4,
		p6:      p6,
		ifaces:  ifaces,
		done:    make(chan struct{}),
		events:  make(chan NearbyDevice, 64),
		answers: answers,
		cache:   newMDNSCache(),
	}
	if conn4 != nil {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.readConn(conn4) }()
	}
	if conn6 != nil {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.readConn(conn6) }()
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.periodic() }()
	return s, nil
}

func advertiseMDNS(name, id string, addresses []string, port int) (*mdnsService, error) {
	return startMDNS(buildMDNSAnswers(name, id, addresses, port))
}

func (s *mdnsService) update(name, id string, addresses []string, port int) {
	if s == nil {
		return
	}
	answers := buildMDNSAnswers(name, id, addresses, port)
	s.mu.Lock()
	s.answers = answers
	s.mu.Unlock()
	s.send()
}

func (s *mdnsService) send() {
	s.mu.Lock()
	answers := append([]dnsmessage.Resource(nil), s.answers...)
	ifaces := s.ifaces
	s.mu.Unlock()
	s.sendPacket(answers, ifaces)
}

func (s *mdnsService) sendGoodbye() {
	s.mu.Lock()
	if len(s.answers) == 0 {
		s.mu.Unlock()
		return
	}
	answers := make([]dnsmessage.Resource, len(s.answers))
	for i, res := range s.answers {
		res.Header.TTL = 0
		answers[i] = res
	}
	ifaces := s.ifaces
	s.mu.Unlock()
	s.sendPacket(answers, ifaces)
}

func (s *mdnsService) sendPacket(answers []dnsmessage.Resource, ifaces []net.Interface) {
	message := dnsmessage.Message{}
	if len(answers) > 0 {
		message.Header = dnsmessage.Header{Response: true, Authoritative: true}
		message.Answers = answers
	} else {
		message.Questions = []dnsmessage.Question{{Name: dnsName(mdnsType), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET}}
	}
	data, err := message.Pack()
	if err != nil {
		return
	}
	if s.conn4 != nil {
		if s.p4 != nil && len(ifaces) > 0 {
			for i := range ifaces {
				if err := s.p4.SetMulticastInterface(&ifaces[i]); err == nil {
					_, _ = s.conn4.WriteToUDP(data, mdnsIPv4Addr)
				}
			}
		} else {
			_, _ = s.conn4.WriteToUDP(data, mdnsIPv4Addr)
		}
	}
	if s.conn6 != nil {
		if s.p6 != nil && len(ifaces) > 0 {
			for i := range ifaces {
				if err := s.p6.SetMulticastInterface(&ifaces[i]); err == nil {
					_, _ = s.conn6.WriteToUDP(data, mdnsIPv6Addr)
				}
			}
		} else {
			_, _ = s.conn6.WriteToUDP(data, mdnsIPv6Addr)
		}
	}
}

func (s *mdnsService) periodic() {
	s.send()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			s.ifaces = multicastInterfaces()
			expired := s.cache.prune(time.Now())
			s.mu.Unlock()
			for _, id := range expired {
				select {
				case s.events <- NearbyDevice{ID: id, Removed: true}:
				default:
				}
			}
			s.send()
		case <-s.done:
			return
		}
	}
}

func (s *mdnsService) readConn(conn *net.UDPConn) {
	buffer := make([]byte, 9000)
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		var message dnsmessage.Message
		if message.Unpack(buffer[:n]) != nil {
			continue
		}
		if !message.Header.Response {
			s.mu.Lock()
			hasAnswers := len(s.answers) > 0
			s.mu.Unlock()
			if hasAnswers {
				for _, question := range message.Questions {
					if question.Name.String() == mdnsType {
						s.send()
						break
					}
				}
			}
			continue
		}
		s.mu.Lock()
		updates := s.cache.ingest(message, time.Now())
		s.mu.Unlock()
		for _, device := range updates {
			select {
			case s.events <- device:
			default:
			}
		}
	}
}

func (s *mdnsService) close() {
	s.once.Do(func() {
		s.sendGoodbye()
		close(s.done)
		if s.conn4 != nil {
			_ = s.conn4.Close()
		}
		if s.conn6 != nil {
			_ = s.conn6.Close()
		}
		s.wg.Wait()
		close(s.events)
	})
}
