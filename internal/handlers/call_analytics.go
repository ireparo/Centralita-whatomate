package handlers

import (
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// call_analytics.go serves the Call Analytics dashboard.
//
// One endpoint: GET /api/analytics/calls
// Query parameters:
//
//   - start_date   YYYY-MM-DD  (inclusive; defaults to 30 days ago)
//   - end_date     YYYY-MM-DD  (inclusive; defaults to today)
//   - channel      whatsapp | telnyx_pstn | "" (all)
//   - direction    incoming | outgoing | "" (all)
//
// The response is a single JSON object with all the KPIs the dashboard
// renders — we do not split across multiple endpoints to keep the UI
// single-request and avoid inconsistent filter state across panels.
//
// All SQL aggregation happens in Postgres (via GORM raw). Rows are not
// streamed into Go — we ask Postgres for aggregates directly so the
// endpoint stays fast even with 100 k+ call logs.

// CallAnalyticsResponse is the shape the frontend receives.
type CallAnalyticsResponse struct {
	Summary           CallAnalyticsSummary     `json:"summary"`
	DailyTrend        []CallDailyTrendPoint    `json:"daily_trend"`
	HourlyDistribution []CallHourlyBucket      `json:"hourly_distribution"`
	StatusBreakdown   []CallStatusBucket       `json:"status_breakdown"`
	ChannelBreakdown  []CallChannelBucket      `json:"channel_breakdown"`
	TopIVRFlows       []CallIVRFlowBucket      `json:"top_ivr_flows"`
	TopAgents         []CallAgentBucket        `json:"top_agents"`
	Range             CallAnalyticsRange       `json:"range"`
}

// CallAnalyticsSummary is the headline KPI card row at the top of the view.
type CallAnalyticsSummary struct {
	TotalCalls        int64   `json:"total_calls"`
	AnsweredCalls     int64   `json:"answered_calls"`
	MissedCalls       int64   `json:"missed_calls"`
	OutgoingCalls     int64   `json:"outgoing_calls"`
	IncomingCalls     int64   `json:"incoming_calls"`
	AnsweredRate      float64 `json:"answered_rate"`       // 0..1
	MissedRate        float64 `json:"missed_rate"`         // 0..1
	AvgDurationSecs   float64 `json:"avg_duration_secs"`   // across answered calls
	TotalDurationSecs int64   `json:"total_duration_secs"` // across answered calls
}

// CallDailyTrendPoint is a single entry in the time-series trend chart.
// Date is formatted as YYYY-MM-DD in the organization's (or UTC) timezone.
type CallDailyTrendPoint struct {
	Date         string `json:"date"`
	Total        int64  `json:"total"`
	Answered     int64  `json:"answered"`
	Missed       int64  `json:"missed"`
	AvgDuration  float64 `json:"avg_duration_secs"`
}

// CallHourlyBucket is a 0..23 hour-of-day bucket showing when calls arrive,
// aggregated across the entire requested range. Useful for staffing.
type CallHourlyBucket struct {
	Hour  int   `json:"hour"`  // 0..23
	Total int64 `json:"total"`
}

// CallStatusBucket feeds the "status distribution" donut.
type CallStatusBucket struct {
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

// CallChannelBucket shows the split between WhatsApp calls and Telnyx PSTN
// calls. Empty channel rows (legacy) are reported as "whatsapp" — matches
// the default column value.
type CallChannelBucket struct {
	Channel string `json:"channel"`
	Count   int64  `json:"count"`
}

// CallIVRFlowBucket is the top N IVR flows by call volume.
type CallIVRFlowBucket struct {
	FlowID   string `json:"flow_id"`
	FlowName string `json:"flow_name"`
	Count    int64  `json:"count"`
}

// CallAgentBucket is the top N agents by calls handled.
type CallAgentBucket struct {
	AgentID           string  `json:"agent_id"`
	AgentName         string  `json:"agent_name"`
	Handled           int64   `json:"handled"`
	AvgDurationSecs   float64 `json:"avg_duration_secs"`
	TotalDurationSecs int64   `json:"total_duration_secs"`
}

// CallAnalyticsRange echoes back the effective date range the aggregation
// used, so the UI can label charts correctly when callers omit dates.
type CallAnalyticsRange struct {
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	Channel   string `json:"channel,omitempty"`
	Direction string `json:"direction,omitempty"`
}

// GetCallAnalytics handles GET /api/analytics/calls.
func (a *App) GetCallAnalytics(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	// Reuse call_logs:read — analysts who can list calls can aggregate them.
	// An admin with analytics:read but no call_logs:read could also be
	// argued for, but that permission combination is unusual in practice.
	if !a.HasPermission(userID, models.ResourceCallLogs, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to view call analytics", nil, "")
	}

	q := r.RequestCtx.QueryArgs()
	startStr := string(q.Peek("start_date"))
	endStr := string(q.Peek("end_date"))
	if startStr == "" {
		startStr = time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	}
	if endStr == "" {
		endStr = time.Now().UTC().Format("2006-01-02")
	}
	startTime, endTime, errMsg := parseDateRange(startStr, endStr)
	if errMsg != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, errMsg, nil, "")
	}

	channel := string(q.Peek("channel"))
	direction := string(q.Peek("direction"))

	// Common WHERE clause applied to every aggregate query below.
	apply := func(db *gorm.DB) *gorm.DB {
		db = db.Where("organization_id = ?", orgID).
			Where("created_at BETWEEN ? AND ?", startTime, endTime)
		if channel != "" {
			db = db.Where("channel = ?", channel)
		}
		if direction != "" {
			db = db.Where("direction = ?", direction)
		}
		return db
	}

	out := CallAnalyticsResponse{
		Range: CallAnalyticsRange{
			StartDate: startStr,
			EndDate:   endStr,
			Channel:   channel,
			Direction: direction,
		},
		DailyTrend:         []CallDailyTrendPoint{},
		HourlyDistribution: []CallHourlyBucket{},
		StatusBreakdown:    []CallStatusBucket{},
		ChannelBreakdown:   []CallChannelBucket{},
		TopIVRFlows:        []CallIVRFlowBucket{},
		TopAgents:          []CallAgentBucket{},
	}

	// --- Summary (one query, multiple aggregates) ------------------------
	//
	// We use filtered aggregates (FILTER clause) instead of multiple
	// separate queries. This is faster and keeps the counts consistent
	// (all aggregates see the same underlying rowset).
	var summaryRow struct {
		Total        int64
		Answered     int64
		Missed       int64
		Outgoing     int64
		Incoming     int64
		AvgDuration  float64
		TotalDuration int64
	}
	summaryQuery := apply(a.DB.Model(&models.CallLog{}))
	if err := summaryQuery.Select(`
		COUNT(*) AS total,
		COUNT(*) FILTER (WHERE status IN ('answered','completed','accepted')) AS answered,
		COUNT(*) FILTER (WHERE status = 'missed') AS missed,
		COUNT(*) FILTER (WHERE direction = 'outgoing') AS outgoing,
		COUNT(*) FILTER (WHERE direction = 'incoming') AS incoming,
		COALESCE(AVG(duration) FILTER (WHERE status IN ('completed','accepted') AND duration > 0), 0) AS avg_duration,
		COALESCE(SUM(duration) FILTER (WHERE status IN ('completed','accepted') AND duration > 0), 0) AS total_duration
	`).Scan(&summaryRow).Error; err != nil {
		a.Log.Error("call analytics: summary query failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to compute analytics", nil, "")
	}
	out.Summary = CallAnalyticsSummary{
		TotalCalls:        summaryRow.Total,
		AnsweredCalls:     summaryRow.Answered,
		MissedCalls:       summaryRow.Missed,
		OutgoingCalls:     summaryRow.Outgoing,
		IncomingCalls:     summaryRow.Incoming,
		AvgDurationSecs:   summaryRow.AvgDuration,
		TotalDurationSecs: summaryRow.TotalDuration,
	}
	if summaryRow.Total > 0 {
		out.Summary.AnsweredRate = float64(summaryRow.Answered) / float64(summaryRow.Total)
		out.Summary.MissedRate = float64(summaryRow.Missed) / float64(summaryRow.Total)
	}

	// --- Daily trend -----------------------------------------------------
	var trendRows []struct {
		Day         time.Time
		Total       int64
		Answered    int64
		Missed      int64
		AvgDuration float64
	}
	if err := apply(a.DB.Model(&models.CallLog{})).
		Select(`
			DATE_TRUNC('day', created_at) AS day,
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE status IN ('answered','completed','accepted')) AS answered,
			COUNT(*) FILTER (WHERE status = 'missed') AS missed,
			COALESCE(AVG(duration) FILTER (WHERE status IN ('completed','accepted') AND duration > 0), 0) AS avg_duration
		`).
		Group("day").
		Order("day ASC").
		Scan(&trendRows).Error; err != nil {
		a.Log.Error("call analytics: daily trend failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to compute analytics", nil, "")
	}
	for _, row := range trendRows {
		out.DailyTrend = append(out.DailyTrend, CallDailyTrendPoint{
			Date:        row.Day.Format("2006-01-02"),
			Total:       row.Total,
			Answered:    row.Answered,
			Missed:      row.Missed,
			AvgDuration: row.AvgDuration,
		})
	}

	// --- Hourly distribution ---------------------------------------------
	var hourRows []struct {
		Hour  int
		Total int64
	}
	if err := apply(a.DB.Model(&models.CallLog{})).
		Select(`EXTRACT(HOUR FROM created_at)::int AS hour, COUNT(*) AS total`).
		Group("hour").
		Order("hour ASC").
		Scan(&hourRows).Error; err != nil {
		a.Log.Error("call analytics: hourly distribution failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to compute analytics", nil, "")
	}
	// Fill missing hours with 0 so the frontend can render a flat 24-hour
	// axis without gaps.
	byHour := map[int]int64{}
	for _, row := range hourRows {
		byHour[row.Hour] = row.Total
	}
	for h := 0; h < 24; h++ {
		out.HourlyDistribution = append(out.HourlyDistribution, CallHourlyBucket{Hour: h, Total: byHour[h]})
	}

	// --- Status breakdown ------------------------------------------------
	var statusRows []struct {
		Status string
		Count  int64
	}
	if err := apply(a.DB.Model(&models.CallLog{})).
		Select(`status, COUNT(*) AS count`).
		Group("status").
		Order("count DESC").
		Scan(&statusRows).Error; err != nil {
		a.Log.Error("call analytics: status breakdown failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to compute analytics", nil, "")
	}
	for _, row := range statusRows {
		out.StatusBreakdown = append(out.StatusBreakdown, CallStatusBucket{Status: row.Status, Count: row.Count})
	}

	// --- Channel breakdown -----------------------------------------------
	// Only meaningful when no channel filter is active; otherwise the row
	// has a single entry matching the filter.
	var channelRows []struct {
		Channel string
		Count   int64
	}
	if err := apply(a.DB.Model(&models.CallLog{})).
		Select(`COALESCE(NULLIF(channel, ''), 'whatsapp') AS channel, COUNT(*) AS count`).
		Group("channel").
		Order("count DESC").
		Scan(&channelRows).Error; err != nil {
		a.Log.Error("call analytics: channel breakdown failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to compute analytics", nil, "")
	}
	for _, row := range channelRows {
		out.ChannelBreakdown = append(out.ChannelBreakdown, CallChannelBucket{Channel: row.Channel, Count: row.Count})
	}

	// --- Top IVR flows (by volume) --------------------------------------
	var flowRows []struct {
		IVRFlowID uuid.UUID
		Name      string
		Count     int64
	}
	if err := apply(a.DB.Table("call_logs")).
		Select(`call_logs.ivr_flow_id AS ivr_flow_id, ivr_flows.name AS name, COUNT(*) AS count`).
		Joins("LEFT JOIN ivr_flows ON ivr_flows.id = call_logs.ivr_flow_id").
		Where("call_logs.ivr_flow_id IS NOT NULL").
		Group("call_logs.ivr_flow_id, ivr_flows.name").
		Order("count DESC").
		Limit(10).
		Scan(&flowRows).Error; err != nil {
		a.Log.Error("call analytics: top ivr flows failed", "error", err)
		// Non-fatal — leave TopIVRFlows empty and continue.
	}
	for _, row := range flowRows {
		name := row.Name
		if name == "" {
			name = "(deleted flow)"
		}
		out.TopIVRFlows = append(out.TopIVRFlows, CallIVRFlowBucket{
			FlowID:   row.IVRFlowID.String(),
			FlowName: name,
			Count:    row.Count,
		})
	}

	// --- Top agents (by calls handled) -----------------------------------
	var agentRows []struct {
		AgentID       uuid.UUID
		Name          string
		Handled       int64
		AvgDuration   float64
		TotalDuration int64
	}
	if err := apply(a.DB.Table("call_logs")).
		Select(`
			call_logs.agent_id AS agent_id,
			users.full_name AS name,
			COUNT(*) AS handled,
			COALESCE(AVG(call_logs.duration) FILTER (WHERE call_logs.duration > 0), 0) AS avg_duration,
			COALESCE(SUM(call_logs.duration) FILTER (WHERE call_logs.duration > 0), 0) AS total_duration
		`).
		Joins("LEFT JOIN users ON users.id = call_logs.agent_id").
		Where("call_logs.agent_id IS NOT NULL").
		Group("call_logs.agent_id, users.full_name").
		Order("handled DESC").
		Limit(10).
		Scan(&agentRows).Error; err != nil {
		a.Log.Error("call analytics: top agents failed", "error", err)
	}
	for _, row := range agentRows {
		name := row.Name
		if name == "" {
			name = "(deactivated)"
		}
		out.TopAgents = append(out.TopAgents, CallAgentBucket{
			AgentID:           row.AgentID.String(),
			AgentName:         name,
			Handled:           row.Handled,
			AvgDurationSecs:   row.AvgDuration,
			TotalDurationSecs: row.TotalDuration,
		})
	}

	return r.SendEnvelope(out)
}
