package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/librecosta/uptime/internal/alerts"
)

const maxFeishuBytes = 20 * 1024

var botPath = regexp.MustCompile(`^/open-apis/bot/v2/hook/[A-Za-z0-9_-]{1,256}$`)

// FeishuConfig contains a custom-bot URL, optional signing secret and keyword.
// The URL and Secret are credentials and must never be logged or dumped.
type FeishuConfig struct{ URL, Secret, Keyword string }

// Feishu sends custom-bot text messages to official Feishu/Lark HTTPS endpoints.
type Feishu struct {
	client               senderClient
	url, secret, keyword string
}

var _ Sender = (*Feishu)(nil)

// NewFeishu validates the official host/path without I/O. Tests can inject a
// RoundTripper, while keeping the actual destination validation enabled.
func NewFeishu(cfg FeishuConfig, opts Options) (*Feishu, error) {
	u, err := cfg.destination()
	if err != nil {
		return nil, err
	}
	client, err := newClient(opts)
	if err != nil {
		return nil, err
	}
	return &Feishu{client: client, url: u.String(), secret: cfg.Secret, keyword: cfg.Keyword}, nil
}

// Validate checks custom-bot configuration without creating an HTTP client.
func (cfg FeishuConfig) Validate() error { _, err := cfg.destination(); return err }

func (cfg FeishuConfig) destination() (*url.URL, error) {
	u, err := destination(cfg.URL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || (u.Hostname() != "open.feishu.cn" && u.Hostname() != "open.larksuite.com") || (u.Port() != "" && u.Port() != "443") || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || !botPath.MatchString(u.Path) || len(cfg.Secret) > 4096 || !utf8.ValidString(cfg.Secret) || len(cfg.Keyword) > maxFeishuBytes || !validText(cfg.Keyword, false) {
		return nil, ErrInvalidConfig
	}
	return u, nil
}

// CloseIdleConnections releases idle connections after all Send calls finish.
func (s *Feishu) CloseIdleConnections() { s.client.http.CloseIdleConnections() }

type feishuPayload struct {
	Timestamp string `json:"timestamp,omitempty"`
	Sign      string `json:"sign,omitempty"`
	MsgType   string `json:"msg_type"`
	Content   struct {
		Text string `json:"text"`
	} `json:"content"`
}

// Send constructs a fresh timestamp/signature for this one attempt. Only an
// explicit successful business code in a 2xx response denotes success.
func (s *Feishu) Send(ctx context.Context, m Message) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := validateMessage(m); err != nil {
		return Result{}, err
	}
	now := s.client.now().UTC()
	if !validTime(now) || now.Unix() <= 0 {
		return Result{}, ErrInvalidConfig
	}
	payload := feishuPayload{MsgType: "text"}
	payload.Content.Text = fmt.Sprintf("%s\nEndpoint: %s\nEndpoint ID: %s\nEvent time (UTC): %s\nIncident ID: %s", m.Task.Type, displayName(m), m.Incident.EndpointID, m.Task.OccurredAt.UTC().Format(time.RFC3339Nano), m.Incident.ID)
	if s.keyword != "" {
		payload.Content.Text = s.keyword + "\n" + payload.Content.Text
	}
	if s.secret != "" {
		payload.Timestamp = strconv.FormatInt(now.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(payload.Timestamp+"\n"+s.secret))
		payload.Sign = base64.StdEncoding.EncodeToString(mac.Sum(nil)) // HMAC of the empty message.
	}
	body, err := marshal(payload, maxFeishuBytes)
	if err != nil {
		return Result{}, err
	}
	r, err := s.client.post(ctx, s.url, body, "", "")
	if err != nil {
		return s.client.failure(ctx, err)
	}
	result := httpResult(r, s.client.now())
	// A redirect never authorizes another destination, even with a business code.
	if r.status >= 300 && r.status < 400 {
		return result, nil
	}
	code, ok := feishuCode(r.body)
	if ok {
		switch code {
		case 11232:
			result.Status, result.ErrorCode = alerts.TaskPending, alerts.ErrorRateLimited
			result.RetryAt = retryAfter(r.header, result.AttemptedAt)
			return result, nil
		case 19001, 19021, 19022, 19024:
			return Result{Status: alerts.TaskFailed, ErrorCode: alerts.ErrorRejected, AttemptedAt: result.AttemptedAt}, nil
		}
	}
	if result.Status != alerts.TaskSent {
		return result, nil
	}
	if !ok || code != 0 {
		result.Status, result.ErrorCode = alerts.TaskFailed, alerts.ErrorRejected
	}
	return result, nil
}

func feishuCode(data []byte) (int, bool) {
	// Reject duplicate/contradictory codes as well as absent, null or mistyped
	// values: decoding into an int struct could mistake these for success.
	d := json.NewDecoder(bytes.NewReader(data))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return 0, false
	}
	var code, legacy *int
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return 0, false
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return 0, false
		}
		switch key {
		case "code":
			if code != nil || json.Unmarshal(raw, &code) != nil || code == nil {
				return 0, false
			}
		case "StatusCode":
			if legacy != nil || json.Unmarshal(raw, &legacy) != nil || legacy == nil {
				return 0, false
			}
		}
	}
	if _, err := d.Token(); err != nil {
		return 0, false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return 0, false
	}
	if code != nil {
		if legacy != nil && *legacy != *code {
			return 0, false
		}
		return *code, true
	}
	if legacy != nil {
		return *legacy, true
	}
	return 0, false
}
