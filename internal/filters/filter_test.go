package filters

import (
	"testing"

	"github.com/spdeepak/notibridge/internal/notifications"
)

func TestExactExclusion(t *testing.T) {
	f := New([]string{"com.apple.Safari"})
	if !f.Excluded("com.apple.Safari") {
		t.Error("expected exact match to be excluded")
	}
	if !f.Excluded("COM.APPLE.SAFARI") {
		t.Error("expected case-insensitive match")
	}
	if f.Excluded("com.apple.SafariTechnologyPreview") {
		t.Error("exact match must not exclude other bundles")
	}
}

func TestWildcardExclusion(t *testing.T) {
	f := New([]string{"com.apple.*"})
	if !f.Excluded("com.apple.Safari") || !f.Excluded("com.apple.MobileSMS") {
		t.Error("wildcard should exclude com.apple.* bundles")
	}
	if f.Excluded("com.slack.Slack") {
		t.Error("wildcard leaked to other vendors")
	}
}

func TestEmptyAndBlankPatterns(t *testing.T) {
	f := New([]string{"", "   ", "com.slack.Slack"})
	if f.Excluded("com.apple.finder") {
		t.Error("blank patterns must not exclude anything")
	}
	if !f.Excluded("com.slack.Slack") {
		t.Error("configured exclusion lost")
	}
}

func TestMatchNotification(t *testing.T) {
	f := New([]string{"com.example.noise"})
	if !f.Match(notifications.Notification{AppID: "com.whatsapp.WhatsApp"}) {
		t.Error("non-excluded app should match")
	}
	if f.Match(notifications.Notification{AppID: "com.example.noise"}) {
		t.Error("excluded app should not match")
	}
}
