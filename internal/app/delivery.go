package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"time"

	"github.com/gofiber/fiber/v3/log"
	"github.com/librecosta/uptime/internal/alerts"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/internal/notify"
)

const (
	deliveryScanInterval = time.Second
	deliveryPageSize     = 50
	deliveryPagesPerTick = 8
	feishuSendInterval   = time.Second // At most 60/minute per bot, without bursts.
)

// deliveryWorker has a single owner. A retained outcome fences all further sends
// until its exact receipt is reconciled; no I/O error is permission to resend.
// The fence is in memory: a crash after remote acceptance can still duplicate a
// message. This worker requires one executor per alert database/Redis namespace.
type deliveryWorker struct {
	store          alerts.DeliveryStore
	incidents      alerts.Store
	senders        map[string]notify.Sender
	names          map[string]string
	botKeys        map[string]string
	nextBotSend    map[string]time.Time
	closeSenders   func()
	now            func() time.Time
	warn           func(string, string, alerts.ErrorCode)
	lastLog        time.Time
	opTimeout      time.Duration
	pending        *alerts.Outcome
	pendingChannel string
	cursor         string
	passAt         time.Time
	cancel         context.CancelFunc
	done           chan struct{}
}

func newDeliveryWorker(store alerts.Store, cfg config.Config) (*deliveryWorker, error) {
	ds, ok := store.(alerts.DeliveryStore)
	if !ok {
		return nil, errors.New("alert delivery storage is required")
	}
	d := &deliveryWorker{store: ds, incidents: store, senders: make(map[string]notify.Sender), names: make(map[string]string), botKeys: make(map[string]string), nextBotSend: make(map[string]time.Time), now: time.Now, opTimeout: cfg.Server.ShutdownTimeout}
	var closers []func()
	d.closeSenders = func() {
		for _, close := range closers {
			close()
		}
	}
	for _, c := range cfg.Alerts.Channels {
		var sender notify.Sender
		var err error
		switch {
		case c.Type == "webhook" && c.Webhook != nil:
			var s *notify.Webhook
			s, err = notify.NewWebhook(*c.Webhook, notify.Options{})
			if err == nil {
				sender = s
				closers = append(closers, s.CloseIdleConnections)
			}
		case c.Type == "feishu" && c.Feishu != nil:
			var s *notify.Feishu
			s, err = notify.NewFeishu(*c.Feishu, notify.Options{})
			if err == nil {
				sender = s
				closers = append(closers, s.CloseIdleConnections)
				// Distinct channel IDs for the same bot share a conservative limit.
				u, _ := url.Parse(c.Feishu.URL)
				sum := sha256.Sum256([]byte(u.Hostname() + u.Path))
				d.botKeys[c.ID] = hex.EncodeToString(sum[:])
			}
		default:
			err = notify.ErrInvalidConfig
		}
		if err != nil {
			d.closeSenders()
			return nil, err
		}
		d.senders[c.ID] = sender
	}
	for _, e := range cfg.Endpoints {
		d.names[e.ID] = e.Name
	}
	d.warn = func(event, channel string, code alerts.ErrorCode) {
		log.Warnf("alert delivery deferred or failed: event=%q channel=%q code=%s", event, channel, code)
	}
	return d, nil
}

func (d *deliveryWorker) start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel, d.done = cancel, make(chan struct{})
	go func() {
		defer close(d.done)
		ticker := time.NewTicker(deliveryScanInterval)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			d.scan(ctx) // Also recover historical PENDING tasks at startup.
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (d *deliveryWorker) stop() {
	d.cancel()
	<-d.done // Never close storage or transports beneath Send/RecordOutcome.
	d.closeSenders()
}

func (d *deliveryWorker) report(event, channel string, code alerts.ErrorCode) {
	now := d.now()
	if d.lastLog.IsZero() || now.Sub(d.lastLog) >= 5*time.Second {
		d.lastLog = now
		d.warn(event, channel, code)
	}
}

func (d *deliveryWorker) scan(ctx context.Context) {
	if d.pending != nil && !d.record() {
		return
	}
	if d.passAt.IsZero() {
		d.passAt = d.now().UTC()
	}
	for range deliveryPagesPerTick {
		if ctx.Err() != nil {
			return
		}
		opCtx, cancel := context.WithTimeout(ctx, d.opTimeout)
		page, err := d.store.QueryDue(opCtx, alerts.DueQuery{Now: d.passAt, Cursor: d.cursor, Limit: deliveryPageSize})
		cancel()
		if err != nil {
			d.report("", "", alerts.ErrorUnavailable)
			d.cursor, d.passAt = "", time.Time{}
			return
		}
		for _, task := range page.Tasks {
			if ctx.Err() != nil {
				return
			}
			d.deliver(ctx, task)
			if d.pending != nil {
				return
			}
		}
		d.cursor = page.NextCursor
		if d.cursor == "" {
			d.passAt = time.Time{}
			return
		}
		// An empty page is not the end of a pass. Retain Now across bounded ticks.
	}
}

func (d *deliveryWorker) deliver(ctx context.Context, candidate alerts.Task) {
	opCtx, cancel := context.WithTimeout(ctx, d.opTimeout)
	task, incident, err := d.store.ReadDelivery(opCtx, candidate.EventID, d.now())
	cancel()
	if err != nil {
		d.report(candidate.EventID, candidate.ChannelID, alerts.ErrorUnavailable)
		return
	}
	if task == nil {
		return
	}
	// HSCAN can repeat records, and a resumed page can contain stale tasks.
	if task.Status != alerts.TaskPending || task.Attempts != candidate.Attempts || task.OccurredAt.After(d.now()) || task.NextAttemptAt.After(d.now()) {
		return
	}
	sender := d.senders[task.ChannelID]
	if sender == nil {
		d.report(task.EventID, task.ChannelID, alerts.ErrorUnavailable)
		return
	}
	bot := d.botKeys[task.ChannelID]
	if bot != "" && d.now().Before(d.nextBotSend[bot]) {
		return
	}
	if ctx.Err() != nil {
		return
	}
	id := rand.Text()
	if bot != "" {
		d.nextBotSend[bot] = d.now().Add(feishuSendInterval)
	}
	result, err := sender.Send(ctx, notify.Message{Task: *task, Incident: *incident, EndpointName: d.names[incident.EndpointID]})
	if err != nil {
		// Adapters return no completed result for cancellation/validation failure.
		// Never invent a completed attempt or persist an underlying error string.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			d.report(task.EventID, task.ChannelID, alerts.ErrorInvalidRequest)
		}
		return
	}
	if bot != "" && result.RetryAt.After(d.nextBotSend[bot]) {
		// A provider cooldown applies to this bot even through another channel ID.
		d.nextBotSend[bot] = result.RetryAt
	}
	d.pending = &alerts.Outcome{EventID: task.EventID, AttemptID: id, ExpectedAttempts: task.Attempts, AttemptedAt: result.AttemptedAt, Status: result.Status, ErrorCode: result.ErrorCode, RetryAt: result.RetryAt}
	d.pendingChannel = task.ChannelID
	if result.Status != alerts.TaskSent {
		d.report(task.EventID, task.ChannelID, result.ErrorCode)
	}
	d.record()
}

// A completed send is recorded with an independent bounded context, even if
// shutdown canceled its HTTP context just after the response completed. On a
// lost commit reply, retain the same Outcome/ID and block sends until reconciled.
func (d *deliveryWorker) record() bool {
	ctx, cancel := context.WithTimeout(context.Background(), d.opTimeout)
	defer cancel()
	o := *d.pending
	if _, err := d.store.RecordOutcome(ctx, o); err == nil {
		d.pending = nil
		return true
	}
	task, err := d.store.GetTask(ctx, o.EventID)
	if err == nil && task != nil {
		matched, receiptErr := task.HasOutcome(o)
		if receiptErr == nil && matched {
			d.pending = nil
			return true
		}
		if receiptErr == nil && task.Type == alerts.Reminder && task.Status == alerts.TaskSkipped && task.LastErrorCode == alerts.ErrorIncidentRecovered {
			// Recovery may cancel an in-flight reminder without recording its send.
			// Prove the closed incident and unchanged history before releasing the
			// fence; an unavailable/mismatched read still leaves the outcome unknown.
			incident, err := d.incidents.Get(ctx, task.IncidentID)
			if err == nil && incident != nil {
				if canceled, err := task.CanceledByRecovery(*incident, o); err == nil && canceled {
					d.pending = nil
					return true
				}
			}
		}
	}
	d.report(o.EventID, d.pendingChannel, alerts.ErrorUnavailable)
	return false
}
