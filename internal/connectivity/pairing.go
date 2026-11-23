package connectivity

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

const Session = "session"
const Desktop = "desktop"

func Token() string {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(value[:])
}

func Code(scope string) string {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	for i := range value {
		value[i] = alphabet[int(value[i])%len(alphabet)]
	}
	prefix := "SESSION-"
	if scope == Desktop {
		prefix = "DESK-"
	}
	return prefix + string(value[:4]) + "-" + string(value[4:8]) + "-" + string(value[8:])
}

func NormalizeCode(code string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), " ", ""))
}

type Pairing struct {
	ID         string          `json:"id"`
	Code       string          `json:"code"`
	Scope      string          `json:"scope"`
	Name       string          `json:"name"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Status     string          `json:"status"`
	PairedWith string          `json:"pairedWith,omitempty"`
	Owner      string          `json:"-"`
}

// Pairings is bounded, expires entries before use, and consumes under one mutex.
// Session and desktop credentials never share a namespace or authorization.
type Pairings struct {
	mu      sync.Mutex
	entries map[string]Pairing
}

func (p *Pairings) prune() {
	for code, entry := range p.entries {
		if !time.Now().Before(entry.ExpiresAt.Add(5 * time.Minute)) {
			delete(p.entries, code)
		}
	}
}
func (p *Pairings) Put(scope, name string, payload json.RawMessage, ttl time.Duration, owner string) (Pairing, error) {
	if scope != Session && scope != Desktop {
		return Pairing{}, errors.New("配对用途无效")
	}
	if len(payload) == 0 || len(payload) > 16<<10 || !json.Valid(payload) {
		return Pairing{}, errors.New("配对资料无效")
	}
	if ttl <= 0 || ttl > 10*time.Minute {
		ttl = 5 * time.Minute
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	if p.entries == nil {
		p.entries = map[string]Pairing{}
	}
	if len(p.entries) >= 1024 {
		return Pairing{}, errors.New("等待配对的设备过多，请稍后重试")
	}
	entry := Pairing{ID: Token(), Code: Code(scope), Scope: scope, Name: name, ExpiresAt: time.Now().Add(ttl), Payload: append(json.RawMessage(nil), payload...), Owner: owner, Status: "waiting"}
	p.entries[entry.Code] = entry
	return entry, nil
}
func (p *Pairings) Redeem(scope, code string, name ...string) (Pairing, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	code = NormalizeCode(code)
	entry, exists := p.entries[code]
	if !exists || entry.Status != "waiting" || !time.Now().Before(entry.ExpiresAt) {
		return Pairing{}, errors.New("配对码无效、已兑换或已过期，请重新生成")
	}
	if entry.Scope != scope {
		return Pairing{}, errors.New("配对码用途不符：会话配对码与桌面共享码不能混用")
	}
	stored := entry
	stored.Payload = nil
	stored.Status = "paired"
	if len(name) > 0 {
		stored.PairedWith = name[0][:min(len(name[0]), 128)]
	}
	p.entries[code] = stored
	return entry, nil
}
func (p *Pairings) Info(id, owner string) (Pairing, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	for _, entry := range p.entries {
		if entry.ID == id && entry.Owner == owner {
			entry.Payload = nil
			if entry.Status == "waiting" && !time.Now().Before(entry.ExpiresAt) {
				entry.Status = "expired"
			}
			return entry, true
		}
	}
	return Pairing{}, false
}

func (p *Pairings) Revoke(id, owner string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for code, entry := range p.entries {
		if entry.ID == id && entry.Owner == owner {
			entry.Status = "revoked"
			entry.Payload = nil
			p.entries[code] = entry
			return true
		}
	}
	return false
}
