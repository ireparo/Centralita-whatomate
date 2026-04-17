package handlers_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// createTestCRMQueueItem inserts a CRMEventQueue row for the given org.
func createTestCRMQueueItem(t *testing.T, app *handlers.App, orgID uuid.UUID, status, eventType string) *models.CRMEventQueue {
	t.Helper()
	row := &models.CRMEventQueue{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		EventType:      eventType,
		Endpoint:       "https://sat.ireparo.es/api/pbx/call-event",
		Payload:        `{"event":"` + eventType + `","data":{}}`,
		Signature:      "sha256=stub",
		Timestamp:      time.Now().Unix(),
		Status:         status,
		AttemptCount:   1,
	}
	require.NoError(t, app.DB.Create(row).Error)
	return row
}

func TestApp_ListCRMQueue_Success(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	createTestCRMQueueItem(t, app, org.ID, "pending", "call.ringing")
	createTestCRMQueueItem(t, app, org.ID, "dead_letter", "call.ended")
	createTestCRMQueueItem(t, app, org.ID, "delivered", "message.inbound")

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.ListCRMQueue(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Queue   []handlers.CRMQueueResponse `json:"queue"`
			Total   int64                       `json:"total"`
			Summary map[string]int64            `json:"summary"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, "success", result.Status)
	assert.Equal(t, int64(3), result.Data.Total)
	assert.Len(t, result.Data.Queue, 3)
	assert.Equal(t, int64(1), result.Data.Summary["pending"])
	assert.Equal(t, int64(1), result.Data.Summary["dead_letter"])
	assert.Equal(t, int64(1), result.Data.Summary["delivered"])
}

func TestApp_ListCRMQueue_FilterByStatus(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	createTestCRMQueueItem(t, app, org.ID, "pending", "call.ringing")
	dead := createTestCRMQueueItem(t, app, org.ID, "dead_letter", "call.ended")
	createTestCRMQueueItem(t, app, org.ID, "delivered", "message.inbound")

	req := testutil.NewGETRequest(t)
	testutil.SetQueryParam(req, "status", "dead_letter")
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.ListCRMQueue(req))

	var result struct {
		Data struct {
			Queue []handlers.CRMQueueResponse `json:"queue"`
			Total int64                       `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))

	assert.Equal(t, int64(1), result.Data.Total)
	require.Len(t, result.Data.Queue, 1)
	assert.Equal(t, dead.ID, result.Data.Queue[0].ID)
	assert.Equal(t, "dead_letter", result.Data.Queue[0].Status)
}

func TestApp_ListCRMQueue_OnlyCurrentOrg(t *testing.T) {
	// Orgs are isolated — a user in org A must never see events
	// queued by org B.
	app := newTestApp(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, orgA.ID)
	user := testutil.CreateTestUser(t, app.DB, orgA.ID, testutil.WithRoleID(&adminRole.ID))

	createTestCRMQueueItem(t, app, orgA.ID, "pending", "call.ringing")
	createTestCRMQueueItem(t, app, orgB.ID, "pending", "call.ringing")

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, orgA.ID, user.ID)
	require.NoError(t, app.ListCRMQueue(req))

	var result struct {
		Data struct {
			Total int64 `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, int64(1), result.Data.Total, "should only see own org's rows")
}

func TestApp_GetCRMQueueItem_IncludesPayload(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	row := createTestCRMQueueItem(t, app, org.ID, "dead_letter", "call.ended")

	req := testutil.NewGETRequest(t)
	testutil.SetPathParam(req, "id", row.ID.String())
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.GetCRMQueueItem(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var result struct {
		Data handlers.CRMQueueDetailResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &result))
	assert.Equal(t, row.ID, result.Data.ID)
	assert.NotEmpty(t, result.Data.Payload, "detail endpoint must include payload")
	assert.NotEmpty(t, result.Data.Signature)
}

func TestApp_ReplayCRMQueueItem_ResetsToPending(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	row := createTestCRMQueueItem(t, app, org.ID, "dead_letter", "call.ringing")
	// Simulate 10 prior attempts with an error.
	app.DB.Model(row).Updates(map[string]any{"attempt_count": 10, "last_error": "http 500"})

	req := testutil.NewGETRequest(t)
	testutil.SetPathParam(req, "id", row.ID.String())
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.ReplayCRMQueueItem(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var reloaded models.CRMEventQueue
	require.NoError(t, app.DB.First(&reloaded, "id = ?", row.ID).Error)
	assert.Equal(t, "pending", reloaded.Status)
	assert.Equal(t, 0, reloaded.AttemptCount)
	assert.Empty(t, reloaded.LastError)
	assert.NotNil(t, reloaded.NextAttemptAt)
}

func TestApp_ReplayCRMQueueItem_RejectsAlreadyDelivered(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	row := createTestCRMQueueItem(t, app, org.ID, "delivered", "call.ended")

	req := testutil.NewGETRequest(t)
	testutil.SetPathParam(req, "id", row.ID.String())
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.ReplayCRMQueueItem(req))
	assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
}

func TestApp_DiscardCRMQueueItem_DeletesRow(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	row := createTestCRMQueueItem(t, app, org.ID, "dead_letter", "call.ended")

	req := testutil.NewGETRequest(t)
	testutil.SetPathParam(req, "id", row.ID.String())
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.DiscardCRMQueueItem(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var count int64
	app.DB.Model(&models.CRMEventQueue{}).Where("id = ?", row.ID).Count(&count)
	assert.Equal(t, int64(0), count, "row should have been deleted")
}

func TestApp_DiscardCRMQueueItem_CannotTouchOtherOrg(t *testing.T) {
	// User from org A attempts to discard a row from org B — must 404.
	app := newTestApp(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, orgA.ID)
	user := testutil.CreateTestUser(t, app.DB, orgA.ID, testutil.WithRoleID(&adminRole.ID))

	row := createTestCRMQueueItem(t, app, orgB.ID, "dead_letter", "call.ended")

	req := testutil.NewGETRequest(t)
	testutil.SetPathParam(req, "id", row.ID.String())
	testutil.SetAuthContext(req, orgA.ID, user.ID)

	require.NoError(t, app.DiscardCRMQueueItem(req))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))

	var count int64
	app.DB.Model(&models.CRMEventQueue{}).Where("id = ?", row.ID).Count(&count)
	assert.Equal(t, int64(1), count, "row from other org must remain untouched")
}
