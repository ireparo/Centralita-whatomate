package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// telnyx_audio.go exposes a SIGNED PUBLIC endpoint that serves IVR audio
// files without authentication, so Telnyx can fetch them to stream to the
// PSTN caller.
//
// Security model:
//
//   URL format:
//     /api/public/ivr-audio/<filename>?e=<expiry_unix>&s=<hmac_hex>
//
//   The signature is:
//     HMAC-SHA256(secret = JWT secret, message = "<filename>|<expiry_unix>")
//
//   The server:
//     1. Rejects requests whose expiry is in the past.
//     2. Recomputes the HMAC and constant-time compares.
//     3. Rejects directory traversal, symlinks, absolute paths.
//
// URLs are short-lived (5 minutes — just enough for Telnyx to fetch the
// file during a call). The dispatcher builds them on the fly when it
// issues PlayAudio / GatherUsingAudio commands, so they never live longer
// than a single HTTP hop.
//
// Why not reuse ServeIVRAudio: that endpoint requires JWT auth, which
// Telnyx cannot present. A public endpoint is necessary; signing it
// prevents the audio dir from being enumerable by anyone who guesses a
// filename.

const (
	// telnyxAudioURLExpiry is how long a signed audio URL stays valid. Long
	// enough for Telnyx to fetch (typically < 1s) + slack for long IVR
	// playback/recording jobs. Keep it short — these URLs are one-shot.
	telnyxAudioURLExpiry = 15 * time.Minute
)

// BuildSignedIVRAudioURL returns an absolute HTTPS URL to the given IVR audio
// filename, with an expiring HMAC signature suitable for passing to Telnyx's
// playback_start / gather_using_audio actions.
//
// baseURL should be the publicly reachable origin of this PBX instance,
// e.g. "https://pbx.ireparo.es" (no trailing slash).
func (a *App) BuildSignedIVRAudioURL(baseURL, filename string) string {
	filename = sanitizeFilename(filename)
	if filename == "" {
		return ""
	}
	expiry := time.Now().Add(telnyxAudioURLExpiry).Unix()
	sig := signIVRAudio(a.Config.JWT.Secret, filename, expiry)
	return fmt.Sprintf("%s/api/public/ivr-audio/%s?e=%d&s=%s",
		strings.TrimRight(baseURL, "/"),
		filename,
		expiry,
		sig,
	)
}

// ServeSignedIVRAudio is the public handler. Rejects any request that does
// not carry a valid, non-expired HMAC matching the filename.
//
// This handler must NOT be behind JWT middleware. It is wired directly by
// setupRoutes.
func (a *App) ServeSignedIVRAudio(r *fastglue.Request) error {
	filename, _ := r.RequestCtx.UserValue("filename").(string)
	filename = sanitizeFilename(filename)
	if filename == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid filename", nil, "")
	}

	q := r.RequestCtx.QueryArgs()
	expiryStr := string(q.Peek("e"))
	sig := string(q.Peek("s"))

	if expiryStr == "" || sig == "" {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Missing signature", nil, "")
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Invalid signature", nil, "")
	}
	if time.Now().Unix() > expiry {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "URL expired", nil, "")
	}

	expected := signIVRAudio(a.Config.JWT.Secret, filename, expiry)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Invalid signature", nil, "")
	}

	// Resolve + serve the file with the same hardening ServeIVRAudio uses.
	audioDir := a.getAudioDir()
	baseDir, err := filepath.Abs(audioDir)
	if err != nil {
		a.Log.Error("Failed to resolve audio directory", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Storage configuration error", nil, "")
	}
	fullPath, err := filepath.Abs(filepath.Join(baseDir, filename))
	if err != nil || !strings.HasPrefix(fullPath, baseDir+string(os.PathSeparator)) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid file path", nil, "")
	}
	info, err := os.Lstat(fullPath)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "File not found", nil, "")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid file path", nil, "")
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		a.Log.Error("Failed to read audio file", "path", fullPath, "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to read file", nil, "")
	}

	ext := strings.ToLower(filepath.Ext(filename))
	r.RequestCtx.Response.Header.Set("Content-Type", getMimeTypeFromExtension(ext))
	// Telnyx caches audio URLs briefly, which is fine — even without the
	// cache they re-sign every attempt, so there is no collision risk.
	r.RequestCtx.Response.Header.Set("Cache-Control", "public, max-age=600")
	r.RequestCtx.SetBody(data)
	return nil
}

// signIVRAudio computes the HMAC used in the URL. Extracted so the
// dispatcher and the handler build / verify the signature identically.
func signIVRAudio(secret, filename string, expiry int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(filename))
	mac.Write([]byte("|"))
	mac.Write([]byte(strconv.FormatInt(expiry, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}
