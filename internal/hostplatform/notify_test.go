package hostplatform

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

type notifyRecorder struct {
	mu      sync.Mutex
	records []Notification
	err     error
}

func (r *notifyRecorder) send(_ context.Context, notification Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, notification)
	return r.err
}

func (r *notifyRecorder) list() []Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Notification(nil), r.records...)
}

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(step)
}

func TestNotificationCommandKeepsPayloadOutOfAppleScript(t *testing.T) {
	const (
		hostileTitle    = `"完成" & (do shell script "touch /tmp/vantry-pwned")`
		hostileSubtitle = "thread\nwith newline \\ backslash"
		hostileBody     = "模型输出：'; rm -rf ~ # \n\"quoted\" «class PNGf»"
	)
	lookUp := func(string) (string, error) {
		t.Fatal("darwin uses the absolute osascript path")
		return "", nil
	}
	spec, ok := notificationCommand("darwin", lookUp, Notification{Title: hostileTitle, Subtitle: hostileSubtitle, Body: hostileBody})
	if !ok {
		t.Fatal("darwin must send notifications")
	}
	if spec.Name != "/usr/bin/osascript" {
		t.Fatalf("name=%q", spec.Name)
	}
	want := []string{"-", hostileTitle, hostileSubtitle, hostileBody}
	if !slices.Equal(spec.Args, want) {
		t.Fatalf("payload must travel as argv:\n got %q\nwant %q", spec.Args, want)
	}
	for _, payload := range []string{hostileTitle, hostileSubtitle, hostileBody} {
		if strings.Contains(spec.Stdin, payload) {
			t.Fatalf("payload reached the AppleScript source: %q", payload)
		}
	}
	if strings.Contains(spec.Stdin, "do shell script") {
		t.Fatalf("script source contains a payload statement: %q", spec.Stdin)
	}
	for _, want := range []string{"on run argv", "item 1 of argv", "item 2 of argv", "item 3 of argv"} {
		if !strings.Contains(spec.Stdin, want) {
			t.Fatalf("script %q missing %q", spec.Stdin, want)
		}
	}

	// osascript only parses options up to the program file, so a payload that
	// looks like an option stays a script argument instead of becoming one.
	dashes, ok := notificationCommand("darwin", lookUp, Notification{Title: "-e", Subtitle: "-l", Body: "--"})
	if !ok {
		t.Fatal("darwin must send notifications")
	}
	if !slices.Equal(dashes.Args, []string{"-", "-e", "-l", "--"}) {
		t.Fatalf("option-like payload was not passed through as argv: %q", dashes.Args)
	}
}

func TestNotificationCommandLinuxUsesArgumentVector(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name != "notify-send" {
			t.Fatalf("unexpected tool lookup %q", name)
		}
		return "/usr/bin/notify-send", nil
	}
	spec, ok := notificationCommand("linux", lookPath, Notification{
		Title:    `--urgency=critical`,
		Subtitle: "thread",
		Body:     "-danger",
	})
	if !ok {
		t.Fatal("linux with notify-send must send notifications")
	}
	if spec.Name != "/usr/bin/notify-send" {
		t.Fatalf("name=%q", spec.Name)
	}
	if len(spec.Args) < 2 || spec.Args[0] != "--" {
		t.Fatalf("notify-send needs an end-of-options marker: %v", spec.Args)
	}
	want := []string{"--", "--urgency=critical", "thread · -danger"}
	if !slices.Equal(spec.Args, want) {
		t.Fatalf("args=%q want %q", spec.Args, want)
	}
}

func TestNotificationCommandWithoutToolDegrades(t *testing.T) {
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	if _, ok := notificationCommand("linux", missing, Notification{Title: "完成"}); ok {
		t.Fatal("linux without notify-send must degrade")
	}
	if _, ok := notificationCommand("windows", missing, Notification{Title: "完成"}); ok {
		t.Fatal("unsupported platform claimed a notifier")
	}
}

func TestNotifierSanitizesAndLimitsPayload(t *testing.T) {
	recorder := &notifyRecorder{}
	notifier := NewNotifier(NotifierOptions{Send: recorder.send})
	title := "完成\x00\t\n" + strings.Repeat("长", 260)
	body := "第一行\r\n第二行\u200b隐藏\x1b[31m"
	if err := notifier.Notify(context.Background(), Notification{Key: "t1", Title: title, Body: body}); err != nil {
		t.Fatal(err)
	}
	records := recorder.list()
	if len(records) != 1 {
		t.Fatalf("sent %d notifications", len(records))
	}
	sent := records[0]
	if got := utf8.RuneCountInString(sent.Title); got > maxNotificationRunes {
		t.Fatalf("title has %d runes", got)
	}
	if !strings.HasPrefix(sent.Title, "完成 长长") {
		t.Fatalf("title=%q", sent.Title)
	}
	if !strings.HasSuffix(sent.Title, "…") {
		t.Fatalf("long title was not truncated: %q", sent.Title)
	}
	for name, value := range map[string]string{"title": sent.Title, "subtitle": sent.Subtitle, "body": sent.Body} {
		if strings.IndexFunc(value, unicode.IsControl) != -1 {
			t.Fatalf("%s still holds a control character: %q", name, value)
		}
		if strings.ContainsAny(value, "\u200b") {
			t.Fatalf("%s still holds a format character: %q", name, value)
		}
	}
	if sent.Body != "第一行 第二行隐藏[31m" {
		t.Fatalf("body=%q", sent.Body)
	}
}

func TestNotifierThrottlesAndMergesSameThread(t *testing.T) {
	recorder := &notifyRecorder{}
	clock := &testClock{at: time.Unix(1_700_000_000, 0)}
	notifier := NewNotifier(NotifierOptions{Now: clock.now, Send: recorder.send})
	ctx := context.Background()

	if err := notifier.Notify(ctx, Notification{Key: "thread-1", Title: "完成", Body: "第一条"}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := notifier.Notify(ctx, Notification{Key: "thread-1", Title: "完成", Body: "第二条"}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := notifier.Notify(ctx, Notification{Key: "thread-1", Title: "完成", Body: "第三条"}); err != nil {
		t.Fatal(err)
	}
	if records := recorder.list(); len(records) != 1 {
		t.Fatalf("cooldown did not merge repeats: %d sent", len(records))
	}

	clock.advance(2 * time.Second)
	if err := notifier.Notify(ctx, Notification{Key: "thread-1", Title: "完成", Body: "第四条"}); err != nil {
		t.Fatal(err)
	}
	records := recorder.list()
	if len(records) != 2 {
		t.Fatalf("sent %d notifications after cooldown", len(records))
	}
	if got := records[1].Body; got != "第四条（另有 2 条更新）" {
		t.Fatalf("merged body=%q", got)
	}

	if err := notifier.Notify(ctx, Notification{Key: "thread-2", Title: "失败"}); err != nil {
		t.Fatal(err)
	}
	if records := recorder.list(); len(records) != 3 {
		t.Fatalf("another thread must not inherit the cooldown: %d sent", len(records))
	}
}

func TestNotifierRejectsEmptyTitle(t *testing.T) {
	recorder := &notifyRecorder{}
	notifier := NewNotifier(NotifierOptions{Send: recorder.send})
	if err := notifier.Notify(context.Background(), Notification{Key: "t1", Title: " \n\x00 "}); err == nil {
		t.Fatal("blank title accepted")
	}
	if records := recorder.list(); len(records) != 0 {
		t.Fatalf("sent %d notifications for a blank title", len(records))
	}
}

func TestNotifierReturnsDeliveryError(t *testing.T) {
	recorder := &notifyRecorder{err: errors.New("no display")}
	notifier := NewNotifier(NotifierOptions{Send: recorder.send})
	err := notifier.Notify(context.Background(), Notification{Key: "t1", Title: "完成"})
	if err == nil || !strings.Contains(err.Error(), "no display") {
		t.Fatalf("err=%v", err)
	}
	if notification := recorder.list(); len(notification) != 1 {
		t.Fatalf("delivery was not attempted: %d", len(notification))
	}
}
