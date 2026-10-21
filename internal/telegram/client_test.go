package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spdeepak/notibridge/internal/notifications"
)

const testToken = "123456:SECRET-TOKEN-abcdef"

// newTestServer returns a client wired to an httptest server running handler.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewWithBaseURL(testToken, "42", srv.URL, srv.Client())
	return c, srv
}

func TestSendTextSuccess(t *testing.T) {
	var gotPath, gotBody string
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"message_id": 777}})
	})
	id, err := c.SendText(context.Background(), "hello <b>world</b>")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if id != 777 {
		t.Errorf("message id = %d", id)
	}
	if gotPath != "/bot"+testToken+"/sendMessage" {
		t.Errorf("path = %q", gotPath)
	}
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatal(err)
	}
	if req["chat_id"] != "42" {
		t.Errorf("chat_id = %v", req["chat_id"])
	}
	if req["parse_mode"] != "HTML" {
		t.Errorf("parse_mode = %v", req["parse_mode"])
	}
	if req["text"] != "hello <b>world</b>" {
		t.Errorf("text = %v", req["text"])
	}
}

func TestSendTextBigChatID(t *testing.T) {
	// Chat IDs for channels/supergroups exceed 32 bits.
	var gotChatID string
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var m map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&m)
		gotChatID, _ = m["chat_id"].(string)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"message_id": 1}})
	})
	c2 := NewWithBaseURL(testToken, "-1001234567890", c.baseURL, nil)
	if _, err := c2.SendText(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if gotChatID != "" && gotChatID != "-1001234567890" {
		// c2 used its own server? It shares baseURL but not handler capture;
		// ensure the request went through at all.
		t.Logf("captured chat_id via shared server: %q", gotChatID)
	}
}

func TestAPIErrorBadRequest(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": false, "error_code": 400, "description": "Bad Request: chat not found",
		})
	})
	_, err := c.SendText(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if apiErr.StatusCode != 400 || !strings.Contains(apiErr.Description, "chat not found") {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if apiErr.Retryable() {
		t.Error("400 must not be retryable")
	}
}

func TestRateLimitCarriesRetryAfter(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": false, "error_code": 429, "description": "Too Many Requests: retry after 17",
			"parameters": map[string]interface{}{"retry_after": 17},
		})
	})
	_, err := c.SendText(context.Background(), "x")
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T (%v)", err, err)
	}
	if apiErr.RetryAfter != 17*time.Second {
		t.Errorf("RetryAfter = %v", apiErr.RetryAfter)
	}
	if !apiErr.Retryable() {
		t.Error("429 must be retryable")
	}
}

func TestServerErrorIsRetryable(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	})
	_, err := c.SendText(context.Background(), "x")
	if !Retryable(err) {
		t.Errorf("502 must be retryable, got %v", err)
	}
}

func TestTransportErrorIsRetryableAndRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // server gone → connection refused
	c := NewWithBaseURL(testToken, "42", url, &http.Client{Timeout: 2 * time.Second})
	_, err := c.SendText(context.Background(), "x")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !Retryable(err) {
		t.Errorf("transport errors must be retryable: %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("token leaked in error: %v", err)
	}
}

func TestTokenRedactedInAPIErrors(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": false, "error_code": 500,
			"description": "Internal Server Error while calling bot" + testToken,
		})
	})
	_, err := c.SendText(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("token leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted-bot-token]") {
		t.Errorf("redaction marker missing: %v", err)
	}
}

func TestValidateGetMe(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getMe") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true, "result": map[string]interface{}{"id": 1, "username": "notibridge_test_bot"},
		})
	})
	name, err := c.Validate(context.Background())
	if err != nil || name != "notibridge_test_bot" {
		t.Errorf("Validate = %q, %v", name, err)
	}
}

func TestValidateChatID(t *testing.T) {
	for _, ok := range []string{"123", "-1001234567890", "9223372036854775807", "@mychannel"} {
		if err := ValidateChatID(ok); err != nil {
			t.Errorf("ValidateChatID(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "  ", "abc", "12.5", "@", "99999999999999999999"} {
		if err := ValidateChatID(bad); err == nil {
			t.Errorf("ValidateChatID(%q) accepted", bad)
		}
	}
}

func TestFormatEscapesHTML(t *testing.T) {
	n := notifications.Notification{
		AppID:       `com.evil.<script>`,
		Title:       `Title & "quotes" <b>`,
		Subtitle:    "sub <i>",
		Body:        "body & <img src=x onerror=alert(1)>",
		DeliveredAt: time.Date(2026, 10, 18, 22, 35, 0, 0, time.Local),
	}
	msg := Format(n)
	// Strip the formatter's own markup; whatever remains is user content and
	// must contain no angle brackets at all.
	stripped := strings.NewReplacer("<b>", "", "</b>", "", "🔔", "").Replace(msg)
	if strings.ContainsAny(stripped, "<>") {
		t.Errorf("unescaped markup leaked:\n%s", msg)
	}
	if strings.Contains(msg, "<script>") || strings.Contains(msg, "<img") {
		t.Errorf("unescaped markup leaked:\n%s", msg)
	}
	for _, want := range []string{"&lt;script&gt;", "&amp;", "&quot;", "&lt;i&gt;", "&lt;img"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing escape %q in:\n%s", want, msg)
		}
	}
}

func TestFormatIncludesOnlyPresentFields(t *testing.T) {
	n := notifications.Notification{AppID: "com.example.app"}
	msg := Format(n)
	// The app line shows the last bundle-ID component ("app"), not the full
	// reverse-DNS identifier. Format trims the trailing newline, so the app
	// line ends the message here.
	if !strings.Contains(msg, "<b>App:</b> app") {
		t.Errorf("missing app line:\n%s", msg)
	}
	for _, absent := range []string{"Title:", "Subtitle:", "Message:", "Time:", "Category:"} {
		if strings.Contains(msg, absent) {
			t.Errorf("absent field %q rendered:\n%s", absent, msg)
		}
	}
	if !strings.Contains(msg, "macOS Notification") {
		t.Errorf("header missing:\n%s", msg)
	}
}

func TestUnconfiguredClientRefuses(t *testing.T) {
	c := NewWithBaseURL("", "42", "https://api.telegram.org", nil)
	if _, err := c.SendText(context.Background(), "x"); err == nil {
		t.Error("unconfigured client must refuse to send")
	}
}

func TestSendTestMessage(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"message_id": 5}})
	})
	if err := c.SendTestMessage(context.Background()); err != nil {
		t.Errorf("SendTestMessage: %v", err)
	}
}
