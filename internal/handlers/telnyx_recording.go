package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/integrations/telnyx"
	"github.com/shridarpatil/whatomate/internal/models"
)

// enqueueTelnyxRecordingDownload spawns a tracked goroutine that pulls the
// recording mp3 from Telnyx's storage and stores it locally (or in S3 if
// the deployment is configured for S3).
//
// We do this off the hot path so the webhook handler returns 200 to Telnyx
// in <100 ms — recordings can be hundreds of KB and we do not want to
// block Telnyx waiting on the download.
//
// The download writes to:
//
//	<media_root>/recordings/telnyx/<call_log_id>.mp3
//
// and the resulting path is stored in the CallLog.RecordingS3Key column
// (the column name is legacy from the WhatsApp implementation; for local
// storage we just store the relative path).
func (a *App) enqueueTelnyxRecordingDownload(orgID uuid.UUID, rec *telnyx.CallRecordingSavedPayload) {
	if rec == nil {
		return
	}
	url := rec.URLs.MP3
	if url == "" {
		url = rec.URLs.WAV
	}
	if url == "" {
		a.Log.Warn("Telnyx recording event has no download URL",
			"call_control_id", rec.CallControlID,
			"recording_id", rec.RecordingID)
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := a.downloadTelnyxRecording(ctx, orgID, rec, url); err != nil {
			a.Log.Warn("Telnyx recording download failed",
				"recording_id", rec.RecordingID,
				"call_control_id", rec.CallControlID,
				"error", err)
		}
	}()
}

func (a *App) downloadTelnyxRecording(
	ctx context.Context,
	orgID uuid.UUID,
	rec *telnyx.CallRecordingSavedPayload,
	url string,
) error {
	// Find the CallLog this recording belongs to.
	var callLog models.CallLog
	if err := a.DB.
		Where("whatsapp_call_id = ? AND organization_id = ?", rec.CallControlID, orgID).
		First(&callLog).Error; err != nil {
		return fmt.Errorf("call log lookup: %w", err)
	}

	// Build the local filesystem destination.
	subdir := "recordings/telnyx"
	if err := a.ensureMediaDir(subdir); err != nil {
		return fmt.Errorf("ensure dir: %w", err)
	}
	relPath := filepath.Join(subdir, callLog.ID.String()+".mp3")
	absPath := filepath.Join(a.getMediaStoragePath(), relPath)

	// Telnyx serves recordings from a public CDN URL with a short TTL,
	// so we use the standard HTTP client without auth.
	httpClient := a.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status: %d", resp.StatusCode)
	}

	// Write to a temp file first, then rename atomically.
	tmpPath := absPath + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open temp file: %w", err)
	}
	written, err := io.Copy(out, resp.Body)
	if closeErr := out.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmpPath, absPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	// Compute duration from the recording event timestamps.
	durationSec := int(rec.EndedAt.Sub(rec.StartedAt).Seconds())
	if durationSec < 0 {
		durationSec = 0
	}

	// Persist the recording path on the CallLog.
	if err := a.DB.Model(&callLog).Updates(map[string]any{
		"recording_s3_key":   relPath,
		"recording_duration": durationSec,
	}).Error; err != nil {
		return fmt.Errorf("update call log: %w", err)
	}

	a.Log.Info("Telnyx recording downloaded",
		"call_log_id", callLog.ID,
		"path", relPath,
		"size_bytes", written,
		"duration_sec", durationSec)
	return nil
}
