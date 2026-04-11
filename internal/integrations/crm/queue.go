package crm

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

// QueueWorker is a background goroutine that drains the crm_event_queue
// table, retrying delivery of CRM events that previously failed.
//
// Lifecycle:
//
//	worker := NewQueueWorker(db, client, log)
//	go worker.Run(ctx)        // returns when ctx is cancelled
//
// Backoff schedule (per row, attempts capped at MaxAttempts):
//
//	attempt 1: instant
//	attempt 2: +30s
//	attempt 3: +1m
//	attempt 4: +2m
//	attempt 5: +5m
//	attempt 6: +15m
//	attempt 7: +30m
//	attempt 8: +1h
//	attempt 9: +2h
//	attempt 10: +6h
//	attempt 11+: dead_letter, no further retries (visible in admin UI)
const (
	MaxAttempts = 10
	tickPeriod  = 15 * time.Second
)

// QueueWorker drains the persistent CRM event queue with exponential backoff.
type QueueWorker struct {
	DB     *gorm.DB
	Client *Client
	Log    logf.Logger
}

// NewQueueWorker constructs a worker. The worker is a no-op if client is
// disabled — checks happen on every tick.
func NewQueueWorker(db *gorm.DB, client *Client, log logf.Logger) *QueueWorker {
	return &QueueWorker{DB: db, Client: client, Log: log}
}

// Run blocks until ctx is cancelled. Suitable for `go worker.Run(ctx)`.
func (w *QueueWorker) Run(ctx context.Context) {
	if w == nil || w.DB == nil || w.Client == nil {
		return
	}
	if !w.Client.Enabled() {
		w.Log.Info("CRM queue worker not started: integration disabled")
		return
	}
	w.Log.Info("CRM queue worker started", "tick", tickPeriod)
	ticker := time.NewTicker(tickPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.Log.Info("CRM queue worker stopping")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick processes any rows ready for delivery. Errors are logged and the
// row stays in the queue with an updated NextAttemptAt.
func (w *QueueWorker) tick(ctx context.Context) {
	now := time.Now().UTC()
	var rows []models.CRMEventQueue
	if err := w.DB.
		Where("status = ?", "pending").
		Where("next_attempt_at IS NULL OR next_attempt_at <= ?", now).
		Order("created_at ASC").
		Limit(50).
		Find(&rows).Error; err != nil {
		w.Log.Warn("CRM queue tick: query failed", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	for i := range rows {
		w.deliver(ctx, &rows[i])
	}
}

func (w *QueueWorker) deliver(ctx context.Context, row *models.CRMEventQueue) {
	env := &EventEnvelope{
		EventType: row.EventType,
		Body:      []byte(row.Payload),
		Signature: row.Signature,
		Timestamp: row.Timestamp,
		URL:       row.Endpoint,
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := w.Client.Send(sendCtx, env)
	if err == nil {
		now := time.Now().UTC()
		_ = w.DB.Model(row).Updates(map[string]any{
			"status":         "delivered",
			"delivered_at":   now,
			"attempt_count":  row.AttemptCount + 1,
			"next_attempt_at": gorm.Expr("NULL"),
			"last_error":     "",
		}).Error
		w.Log.Info("CRM queue: delivered",
			"event_type", row.EventType,
			"attempts", row.AttemptCount+1)
		return
	}

	row.AttemptCount++
	if row.AttemptCount >= MaxAttempts {
		_ = w.DB.Model(row).Updates(map[string]any{
			"status":        "dead_letter",
			"attempt_count": row.AttemptCount,
			"last_error":    err.Error(),
		}).Error
		w.Log.Warn("CRM queue: dead letter",
			"event_type", row.EventType,
			"attempts", row.AttemptCount,
			"error", err)
		return
	}

	delay := backoffDelay(row.AttemptCount)
	next := time.Now().UTC().Add(delay)
	_ = w.DB.Model(row).Updates(map[string]any{
		"attempt_count":   row.AttemptCount,
		"next_attempt_at": next,
		"last_error":      err.Error(),
	}).Error
	w.Log.Debug("CRM queue: retry scheduled",
		"event_type", row.EventType,
		"attempt", row.AttemptCount,
		"next_in", delay,
		"error", err)
}

// backoffDelay returns the wait time before the n-th retry (1-indexed).
// Caps at 6 hours.
func backoffDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 30 * time.Second
	case 2:
		return time.Minute
	case 3:
		return 2 * time.Minute
	case 4:
		return 5 * time.Minute
	case 5:
		return 15 * time.Minute
	case 6:
		return 30 * time.Minute
	case 7:
		return time.Hour
	case 8:
		return 2 * time.Hour
	default:
		// 6h cap with mild jitter via attempt number
		mins := math.Min(360, float64(60*attempt))
		return time.Duration(mins) * time.Minute
	}
}

// EnqueueEvent persists a previously-built event for later delivery. Used
// when an immediate Send() fails — instead of dropping the event, we
// store it and let the worker retry it.
func EnqueueEvent(db *gorm.DB, orgID uuid.UUID, env *EventEnvelope) error {
	if env == nil {
		return errors.New("crm: nil envelope")
	}
	row := models.CRMEventQueue{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		EventType:      env.EventType,
		Endpoint:       env.URL,
		Payload:        string(env.Body),
		Signature:      env.Signature,
		Timestamp:      env.Timestamp,
		Status:         "pending",
	}
	return db.Create(&row).Error
}
