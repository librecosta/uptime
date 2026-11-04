package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/librecosta/uptime/internal/alerts"
)

const testBotURL = "https://open.feishu.cn/open-apis/bot/v2/hook/test-private-token"

func TestFeishuMessagesAndSignature(t *testing.T) {
	now := testNow
	var bodies []feishuPayload
	opts := options(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != testBotURL || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" {
			t.Fatal("incorrect bot request")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "demo") || strings.Contains(string(data), "test-private-token") || len(data) > maxFeishuBytes {
			t.Fatal("credential leak/body limit")
		}
		var payload feishuPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, payload)
		return reply(200, `{"code":0,"StatusCode":0}`), nil
	}))
	opts.Now = func() time.Time { return now }
	s, err := NewFeishu(FeishuConfig{URL: testBotURL, Secret: "demo", Keyword: "operations"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []alerts.EventType{alerts.Down, alerts.Down, alerts.Recovered} {
		m := message(kind)
		got, err := s.Send(t.Context(), m)
		if err != nil || got.Status != alerts.TaskSent || !got.AttemptedAt.Equal(now) {
			t.Fatalf("Send = %+v, %v", got, err)
		}
		payload := bodies[len(bodies)-1]
		for _, field := range []string{"operations\n" + string(kind), m.EndpointName, m.Incident.EndpointID, m.Incident.ID, m.Task.OccurredAt.Format(time.RFC3339Nano)} {
			if !strings.Contains(payload.Content.Text, field) {
				t.Fatalf("missing message field %q", field)
			}
		}
		if payload.MsgType != "text" {
			t.Fatal("not a text message")
		}
		now = now.Add(time.Second)
	}
	// Reference generated independently with Python hmac/hashlib, using key
	// b"1599360473\ndemo" and an empty message (not the reverse).
	if bodies[0].Timestamp != "1599360473" || bodies[0].Sign != "l1N0gAcBjdwBvGm1xMjOF0XSyaLRpR7tuO5dHfhAYc8=" {
		t.Fatal("signature differs from independent test vector")
	}
	if bodies[0].Sign == bodies[1].Sign || bodies[1].Timestamp != "1599360474" {
		t.Fatal("attempt reused a stale timestamp/signature")
	}
}

func TestFeishuUnsignedFallback(t *testing.T) {
	s, err := NewFeishu(FeishuConfig{URL: "https://open.larksuite.com/open-apis/bot/v2/hook/test"}, options(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["timestamp"]; ok {
			t.Fatal("unsigned message has timestamp")
		}
		if _, ok := fields["sign"]; ok {
			t.Fatal("unsigned message has signature")
		}
		if !strings.Contains(string(fields["content"]), "Endpoint: api") {
			t.Fatal("missing ID fallback")
		}
		return reply(200, `{"StatusCode":0,"StatusMessage":"success"}`), nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	m := message(alerts.Down)
	m.EndpointName = ""
	got, err := s.Send(t.Context(), m)
	checkResult(t, got, err, alerts.TaskSent, "")
}

func TestFeishuClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   alerts.TaskStatus
		code   alerts.ErrorCode
	}{
		{"modern", 200, `{"code":0}`, alerts.TaskSent, ""},
		{"legacy", 200, `{"StatusCode":0}`, alerts.TaskSent, ""},
		{"rate", 200, `{"code":11232}`, alerts.TaskPending, alerts.ErrorRateLimited},
		{"rate-on-4xx", 400, `{"code":11232}`, alerts.TaskPending, alerts.ErrorRateLimited},
		{"invalid-hook", 200, `{"code":19001}`, alerts.TaskFailed, alerts.ErrorRejected},
		{"signature", 200, `{"code":19021}`, alerts.TaskFailed, alerts.ErrorRejected},
		{"ip", 200, `{"code":19022}`, alerts.TaskFailed, alerts.ErrorRejected},
		{"keyword", 200, `{"StatusCode":19024}`, alerts.TaskFailed, alerts.ErrorRejected},
		{"unknown", 200, `{"code":99999,"msg":"private-response"}`, alerts.TaskFailed, alerts.ErrorRejected},
		{"http-rate", 429, `not JSON`, alerts.TaskPending, alerts.ErrorRateLimited},
		{"http-timeout", 408, ``, alerts.TaskPending, alerts.ErrorTimeout},
		{"unavailable", 503, `{"code":99999}`, alerts.TaskPending, alerts.ErrorUnavailable},
		{"bad-gateway", 502, `not JSON`, alerts.TaskPending, alerts.ErrorUnavailable},
		{"permanent-overrides-5xx", 500, `{"code":19021}`, alerts.TaskFailed, alerts.ErrorRejected},
		{"success-does-not-override-5xx", 500, `{"code":0}`, alerts.TaskPending, alerts.ErrorUnavailable},
		{"redirect", 302, `{"code":0}`, alerts.TaskFailed, alerts.ErrorRejected},
		{"redirect-rate", 307, `{"code":11232}`, alerts.TaskFailed, alerts.ErrorRejected},
		{"http-rejection", 403, `{"code":0}`, alerts.TaskFailed, alerts.ErrorRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(tc.body)}
			s, err := NewFeishu(FeishuConfig{URL: testBotURL}, options(roundTripFunc(func(*http.Request) (*http.Response, error) {
				r := reply(tc.status, "")
				r.Body = body
				r.Header.Set("Retry-After", "120")
				return r, nil
			})))
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Send(t.Context(), message(alerts.Down))
			checkResult(t, got, err, tc.want, tc.code)
			if !body.closed {
				t.Fatal("response not closed")
			}
			if tc.want == alerts.TaskPending && !got.RetryAt.Equal(testNow.Add(2*time.Minute)) || tc.want != alerts.TaskPending && !got.RetryAt.IsZero() {
				t.Fatal("incorrect retry hint")
			}
			if strings.Contains(fmt.Sprint(got, err), "private") {
				t.Fatal("business response leaked")
			}
		})
	}
	for _, body := range []string{"", `{`, `null`, `[]`, `{"msg":"success"}`, `{"code":null}`, `{"code":"0"}`, `{"code":0.0}`, `{"Code":0}`, `{"code":0,"StatusCode":19021}`, `{"code":19021,"code":0}`, `{"StatusCode":0,"StatusCode":0}`, `{"code":0} {"code":0}`} {
		s, err := NewFeishu(FeishuConfig{URL: testBotURL}, options(roundTripFunc(func(*http.Request) (*http.Response, error) { return reply(200, body), nil })))
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Send(t.Context(), message(alerts.Down))
		checkResult(t, got, err, alerts.TaskFailed, alerts.ErrorRejected)
	}
}

func TestFeishuValidationAndLimits(t *testing.T) {
	for _, raw := range []string{
		"http://open.feishu.cn/open-apis/bot/v2/hook/test", "https://open.feishu.cn.evil.invalid/open-apis/bot/v2/hook/test",
		"https://evil.invalid/open-apis/bot/v2/hook/test", "https://open.feishu.cn:8443/open-apis/bot/v2/hook/test",
		testBotURL + "?secret=private", testBotURL + "?", testBotURL + "#fragment", testBotURL + "/extra",
		"https://open.feishu.cn/open-apis/bot/v2/hook/", "https://open.feishu.cn/open-apis/bot/v2/hook/%74est",
		"https://open.feishu.cn/open-apis/bot/v2/hook/../test", "https://user:secret@open.feishu.cn/open-apis/bot/v2/hook/test",
	} {
		if _, err := NewFeishu(FeishuConfig{URL: raw}, Options{}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("unsafe URL accepted: %v", err)
		}
	}
	for _, cfg := range []FeishuConfig{{URL: testBotURL, Secret: strings.Repeat("s", 4097)}, {URL: testBotURL, Keyword: "key\nword"}} {
		if _, err := NewFeishu(cfg, Options{}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("bad config accepted")
		}
	}
	for _, long := range []string{strings.Repeat("x", maxFeishuBytes), strings.Repeat("界", maxFeishuBytes/3), strings.Repeat("<", maxFeishuBytes/6)} {
		s, err := NewFeishu(FeishuConfig{URL: testBotURL}, options(roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("oversize message sent"); return nil, nil })))
		if err != nil {
			t.Fatal(err)
		}
		m := message(alerts.Down)
		m.EndpointName = long
		got, err := s.Send(t.Context(), m)
		if !errors.Is(err, ErrInvalidMessage) || got != (Result{}) {
			t.Fatal("oversize message accepted")
		}
	}
	// Feishu shares the bounded HTTP/cancellation path; no external request occurs.
	ctx, cancel := context.WithCancel(t.Context())
	s, err := NewFeishu(FeishuConfig{URL: testBotURL, Secret: "private-secret"}, options(roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, errors.New("URL contains test-private-token")
	})))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Send(ctx, message(alerts.Down))
	cancel()
	if !errors.Is(err, context.Canceled) || got != (Result{}) {
		t.Fatal("cancellation treated as delivery")
	}
}
