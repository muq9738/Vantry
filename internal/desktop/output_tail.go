package desktop

import "sync"

// tailBuffer keeps the last bytes of a stream. An embedded terminal reads
// its program's output as it comes; keeping a copy of the end lets Vantry
// say why a program such as ssh ended, in words, even though the screen has
// already scrolled or the program printed nothing but its reason.
type tailBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func newTailBuffer(limit int) *tailBuffer {
	if limit <= 0 {
		limit = 4 << 10
	}
	return &tailBuffer{limit: limit}
}

func (b *tailBuffer) Write(p []byte) {
	if b == nil || len(p) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
}

func (b *tailBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func (b *tailBuffer) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.data)
}
