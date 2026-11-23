package hostplatform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultNotificationCooldown groups repeated notifications for the same
	// thread, so a burst of run updates does not become a wall of banners.
	DefaultNotificationCooldown = 3 * time.Second
	// maxNotificationRunes caps every field: long model output belongs in the
	// workbench, not in a system banner.
	maxNotificationRunes = 200
	darwinScriptTool     = "/usr/bin/osascript"
)

// notificationScript receives the payload through argv. The title, subtitle, and
// body are never interpolated into this source, so quotes, backslashes,
// newlines, or AppleScript statements coming from a thread title, a path, or
// model output stay ordinary arguments and cannot execute as script code.
const notificationScript = `on run argv
	display notification (item 3 of argv) with title (item 1 of argv) subtitle (item 2 of argv)
end run
`

// Notification is one desktop banner. Title is required; Key groups banners for
// the same subject (normally the thread id) so the cooldown can merge them.
type Notification struct {
	Key      string
	Title    string
	Subtitle string
	Body     string
}

// NotifyFunc delivers a cleaned notification. It exists so a host without a
// notifier, or a test, can replace delivery.
type NotifyFunc func(context.Context, Notification) error

// NotifierOptions configures a notifier. The zero value sends native desktop
// notifications with the default cooldown.
type NotifierOptions struct {
	// Cooldown suppresses repeated notifications for one Key. Zero uses
	// DefaultNotificationCooldown; a negative value disables throttling.
	Cooldown time.Duration
	// Now is the clock used for throttling. Nil uses time.Now.
	Now func() time.Time
	// Send delivers the notification. Nil uses the native desktop notifier.
	Send NotifyFunc
}

// Notifier sends native desktop notifications for background runs: a session
// finishing, a run failing, or an approval request waiting for the user. It
// sanitizes every field (controls and unprintable format characters removed,
// whitespace collapsed, 200-rune cap) and merges repeats for the same key
// inside the cooldown window so a busy session does not spam the desktop.
//
// It is safe for concurrent use.
type Notifier struct {
	mu       sync.Mutex
	cooldown time.Duration
	now      func() time.Time
	send     NotifyFunc
	recent   map[string]notificationRecord
}

type notificationRecord struct {
	at     time.Time
	merged int
}

// NewNotifier returns a notifier using the options, filling in defaults.
func NewNotifier(options NotifierOptions) *Notifier {
	notifier := &Notifier{
		cooldown: options.Cooldown,
		now:      options.Now,
		send:     options.Send,
		recent:   make(map[string]notificationRecord),
	}
	if notifier.cooldown == 0 {
		notifier.cooldown = DefaultNotificationCooldown
	}
	if notifier.now == nil {
		notifier.now = time.Now
	}
	if notifier.send == nil {
		notifier.send = sendNativeNotification
	}
	return notifier
}

// Notify cleans and delivers a notification. A repeat for the same Key inside
// the cooldown is folded into the next banner instead of being shown, so two
// threads stay independent while one thread cannot flood the desktop.
func (n *Notifier) Notify(ctx context.Context, notification Notification) error {
	if n == nil {
		return nil
	}
	title := sanitizeNotificationText(notification.Title)
	if title == "" {
		return errors.New("通知标题不能为空")
	}
	cleaned := Notification{
		Key:      strings.TrimSpace(notification.Key),
		Title:    title,
		Subtitle: sanitizeNotificationText(notification.Subtitle),
		Body:     sanitizeNotificationText(notification.Body),
	}
	now := n.now()
	n.mu.Lock()
	n.pruneLocked(now)
	if n.cooldown > 0 && cleaned.Key != "" {
		if record, seen := n.recent[cleaned.Key]; seen {
			if now.Sub(record.at) < n.cooldown {
				record.merged++
				n.recent[cleaned.Key] = record
				n.mu.Unlock()
				return nil
			}
			if record.merged > 0 {
				cleaned.Body = mergeNotificationBody(cleaned.Body, record.merged)
			}
		}
		n.recent[cleaned.Key] = notificationRecord{at: now}
	}
	n.mu.Unlock()
	return n.send(ctx, cleaned)
}

// pruneLocked keeps the throttle table bounded on a long session. Entries with
// a pending merge count stay longer so their "另有 N 条更新" note is delivered.
func (n *Notifier) pruneLocked(now time.Time) {
	if len(n.recent) < 128 {
		return
	}
	for key, record := range n.recent {
		window := n.cooldown
		if record.merged > 0 {
			window = 10 * n.cooldown
		}
		if now.Sub(record.at) >= window {
			delete(n.recent, key)
		}
	}
}

// mergeNotificationBody reports how many updates were folded, keeping the note
// inside the same length cap as ordinary text.
func mergeNotificationBody(body string, merged int) string {
	suffix := fmt.Sprintf("（另有 %d 条更新）", merged)
	room := maxNotificationRunes - utf8.RuneCountInString(suffix)
	runes := []rune(body)
	if room <= 0 {
		return suffix
	}
	if len(runes) > room {
		runes = runes[:room]
	}
	return string(runes) + suffix
}

// sanitizeNotificationText removes control characters and unprintable format
// characters, collapses whitespace runs (including newlines) to one space, and
// truncates to maxNotificationRunes runes.
func sanitizeNotificationText(text string) string {
	runes := make([]rune, 0, len(text))
	space := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if !unicode.IsPrint(r) {
			continue
		}
		if space && len(runes) > 0 {
			runes = append(runes, ' ')
		}
		space = false
		runes = append(runes, r)
	}
	if len(runes) > maxNotificationRunes {
		runes = append(runes[:maxNotificationRunes-1], '…')
	}
	return string(runes)
}

// notificationCommand builds the native notifier invocation for goos. On darwin
// the payload travels as argv to `osascript -`, never inside the script text;
// on linux notify-send gets `--` so a title starting with a dash stays a title.
func notificationCommand(goos string, lookPath func(string) (string, error), notification Notification) (CommandSpec, bool) {
	switch goos {
	case "darwin":
		return CommandSpec{
			Name:  darwinScriptTool,
			Args:  []string{"-", notification.Title, notification.Subtitle, notification.Body},
			Stdin: notificationScript,
		}, true
	case "linux":
		executable, err := lookPath("notify-send")
		if err != nil {
			return CommandSpec{}, false
		}
		body := notification.Body
		switch {
		case notification.Subtitle == "":
		case body == "":
			body = notification.Subtitle
		default:
			body = notification.Subtitle + " · " + body
		}
		args := []string{"--", notification.Title}
		if body != "" {
			args = append(args, body)
		}
		return CommandSpec{Name: executable, Args: args}, true
	default:
		return CommandSpec{}, false
	}
}

func sendNativeNotification(ctx context.Context, notification Notification) error {
	spec, ok := notificationCommand(runtime.GOOS, exec.LookPath, notification)
	if !ok {
		// Nothing to show the banner with: a finished run must not fail because
		// this host has no notifier.
		return nil
	}
	return runCommand(ctx, spec)
}
