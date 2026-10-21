// Package telegram implements a minimal, defensive client for the official
// Telegram Bot API over HTTPS. It supports connection validation, test
// messages, delivery with rate-limit awareness, and strict error redaction:
// the bot token never appears in logs or error strings.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spdeepak/notibridge/internal/notifications"
)

// DefaultBaseURL is the official Telegram Bot API endpoint. Tests override it
// with an httptest server.
const DefaultBaseURL = "https://api.telegram.org"

// redactedToken is what appears in errors instead of the real token.
const redactedToken = "[redacted-bot-token]"

// Client talks to a single bot.
type Client struct {
	baseURL string
	token   string
	chatID  string // numeric chat/channel id (64-bit) or @handle
	http    *http.Client
}

// New builds a client for the given bot token and destination chat. The chat
// ID may be a signed 64-bit integer as a string, or an @username.
func New(token, chatID string) *Client {
	return NewWithBaseURL(token, chatID, DefaultBaseURL, &http.Client{Timeout: 20 * time.Second})
}

// NewWithBaseURL is New with an explicit API base URL and HTTP client,
// primarily for tests.
func NewWithBaseURL(token, chatID, baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: strings.TrimSpace(token), chatID: strings.TrimSpace(chatID), http: hc}
}

// ChatID reports the configured destination chat.
func (c *Client) ChatID() string { return c.chatID }

// Token reports the bot token (callers must never log it).
func (c *Client) Token() string { return c.token }

// Configured reports whether the client has everything it needs to send.
func (c *Client) Configured() bool { return c.token != "" && c.chatID != "" }

// ValidateChatID accepts either a signed 64-bit integer (as a string, since
// chat IDs can exceed 32 bits) or an @username.
func ValidateChatID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("chat ID is required")
	}
	if strings.HasPrefix(id, "@") {
		if len(id) < 2 {
			return fmt.Errorf("invalid @username chat ID")
		}
		return nil
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return fmt.Errorf("chat ID must be a 64-bit integer or an @username: %w", err)
	}
	return nil
}

// APIError is a Telegram Bot API error. Description is sanitized: it never
// contains the bot token.
type APIError struct {
	StatusCode int
	// Method is the API method that failed.
	Method string
	// Description is the sanitized Telegram error description.
	Description string
	// RetryAfter is set for HTTP 429 responses.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("telegram %s failed (%d): %s", e.Method, e.StatusCode, e.Description)
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	return msg
}

// Retryable reports whether the error is transient and worth retrying:
// server-side (5xx), rate-limit (429) or network errors (StatusCode 0).
func (e *APIError) Retryable() bool {
	return e.StatusCode == 0 || e.StatusCode == 429 || e.StatusCode >= 500
}

// TransportError wraps network-level failures (timeouts, DNS). Timeouts are
// the classic duplicate-send risk documented in the README: Telegram may have
// accepted the message even though we never saw the response.
type TransportError struct {
	Method string
	Err    string
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("telegram %s transport error: %s", e.Method, e.Err)
}

func (e *TransportError) Retryable() bool { return true }

// Retryable reports whether err is worth retrying.
func Retryable(err error) bool {
	if r, ok := err.(interface{ Retryable() bool }); ok {
		return r.Retryable()
	}
	return false
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Result      json.RawMessage `json:"result"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

type sendMessageRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

type message struct {
	MessageID int64 `json:"message_id"`
}

// call performs a Bot API POST and decodes the response. Network errors and
// non-OK API responses become typed errors with the token redacted.
func (c *Client) call(ctx context.Context, method string, payload interface{}, out interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return &TransportError{Method: method, Err: c.redact(err.Error())}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &TransportError{Method: method, Err: c.redact(err.Error())}
	}

	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return &APIError{StatusCode: resp.StatusCode, Method: method,
			Description: c.redact(fmt.Sprintf("undecodable response (status %d)", resp.StatusCode))}
	}
	if !ar.OK {
		apiErr := &APIError{
			StatusCode:  resp.StatusCode,
			Method:      method,
			Description: c.redact(ar.Description),
		}
		if ar.Parameters.RetryAfter > 0 {
			apiErr.RetryAfter = time.Duration(ar.Parameters.RetryAfter) * time.Second
		} else if resp.StatusCode == http.StatusTooManyRequests {
			apiErr.RetryAfter = 30 * time.Second
		}
		return apiErr
	}
	if out != nil {
		if err := json.Unmarshal(ar.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

// redact removes the bot token from any string destined for logs or errors.
func (c *Client) redact(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, redactedToken)
}

// Validate calls getMe and returns the bot's username.
func (c *Client) Validate(ctx context.Context) (string, error) {
	if c.token == "" {
		return "", fmt.Errorf("bot token is not configured")
	}
	var bot struct {
		Username string `json:"username"`
		ID       int64  `json:"id"`
	}
	if err := c.call(ctx, "getMe", struct{}{}, &bot); err != nil {
		return "", err
	}
	return bot.Username, nil
}

// SendText delivers text to the configured chat and returns the Telegram
// message ID. Callers must only treat the message as delivered when this
// method returns a nil error.
func (c *Client) SendText(ctx context.Context, text string) (int64, error) {
	if !c.Configured() {
		return 0, fmt.Errorf("telegram is not configured (bot token and chat ID required)")
	}
	var msg message
	req := sendMessageRequest{ChatID: c.chatID, Text: text, ParseMode: "HTML", DisableWebPagePreview: true}
	if err := c.call(ctx, "sendMessage", req, &msg); err != nil {
		return 0, err
	}
	if msg.MessageID == 0 {
		return 0, fmt.Errorf("telegram sendMessage returned no message id")
	}
	return msg.MessageID, nil
}

// SendTestMessage sends a short confirmation message to the configured chat.
func (c *Client) SendTestMessage(ctx context.Context) error {
	_, err := c.SendText(ctx, "✅ <b>NotiBridge test message</b>\n\nForwarding is configured correctly.")
	return err
}

// Format renders a notification as Telegram HTML. Every user-controlled value
// is HTML-escaped, so notification content can never inject markup or break
// out of the message structure. Fields are only included when present.
func Format(n notifications.Notification) string {
	esc := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch r {
			case '&':
				b.WriteString("&amp;")
			case '<':
				b.WriteString("&lt;")
			case '>':
				b.WriteString("&gt;")
			case '"':
				b.WriteString("&quot;")
			default:
				b.WriteRune(r)
			}
		}
		return b.String()
	}

	var b strings.Builder
	b.WriteString("🔔 <b>macOS Notification</b>\n")
	if n.AppID != "" {
		appLocation := strings.Split(esc(n.AppID), ".")
		appName := appLocation[len(appLocation)-1]
		b.WriteString("<b>App:</b> " + appName + "\n")
	}
	if n.Title != "" {
		b.WriteString("<b>Title:</b> " + esc(n.Title) + "\n")
	}
	if n.Subtitle != "" {
		b.WriteString("<b>Subtitle:</b> " + esc(n.Subtitle) + "\n")
	}
	if n.Body != "" {
		b.WriteString("<b>Message:</b> " + esc(n.Body) + "\n")
	}
	if !n.DeliveredAt.IsZero() {
		b.WriteString("<b>Time:</b> " + esc(n.DeliveredAt.Local().Format("2006-01-02 15:04:05")) + "\n")
	}
	if n.Category != "" {
		b.WriteString("<b>Category:</b> " + esc(n.Category) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// BaseURL returns the configured API endpoint (for diagnostics).
func (c *Client) BaseURL() string { return c.baseURL }
