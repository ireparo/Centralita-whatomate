package telnyx

import (
	"encoding/json"
	"fmt"
	"time"
)

// Event types Telnyx publishes via webhook for Call Control Applications.
// Full reference: https://developers.telnyx.com/docs/voice/programmable-voice/call-events
//
// We only model here the events iReparo cares about. Anything else is
// accepted but ignored at the dispatch layer.
const (
	EventCallInitiated     = "call.initiated"
	EventCallRinging       = "call.ringing"
	EventCallAnswered      = "call.answered"
	EventCallHangup        = "call.hangup"
	EventCallBridged       = "call.bridged"
	EventDTMFReceived      = "call.dtmf.received"
	EventPlaybackStarted   = "call.playback.started"
	EventPlaybackEnded     = "call.playback.ended"
	EventGatherEnded       = "call.gather.ended"
	EventMachineDetected   = "call.machine.detection.ended"
	EventRecordingSaved    = "call.recording.saved"
	EventCallSpeakStarted  = "call.speak.started"
	EventCallSpeakEnded    = "call.speak.ended"
)

// Envelope is the outer wrapper Telnyx puts around every webhook payload.
//
//	{
//	  "data": {
//	    "event_type": "call.initiated",
//	    "id": "...",
//	    "occurred_at": "2026-04-09T18:12:34.567Z",
//	    "payload": { ... event-specific fields ... }
//	  },
//	  "meta": { ... }
//	}
type Envelope struct {
	Data EventData `json:"data"`
	Meta json.RawMessage `json:"meta,omitempty"`
}

// EventData is the inner "data" object from a Telnyx webhook envelope.
// The Payload field carries the type-specific fields and we decode it lazily
// based on EventType.
type EventData struct {
	RecordType string          `json:"record_type"`
	EventType  string          `json:"event_type"`
	ID         string          `json:"id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
}

// CallEventPayload contains the fields common to most call.* events.
// Specific events (CallHangupPayload, CallRecordingSavedPayload) embed this.
type CallEventPayload struct {
	CallControlID    string `json:"call_control_id"`
	CallLegID        string `json:"call_leg_id"`
	CallSessionID    string `json:"call_session_id"`
	ConnectionID     string `json:"connection_id"`
	From             string `json:"from"`
	To               string `json:"to"`
	Direction        string `json:"direction"` // "incoming" or "outgoing"
	State            string `json:"state"`
	ClientState      string `json:"client_state"`
	StartTime        *time.Time `json:"start_time,omitempty"`
	AnswerTime       *time.Time `json:"answer_time,omitempty"`
	EndTime          *time.Time `json:"end_time,omitempty"`
}

// CallHangupPayload extends CallEventPayload with the cause and signaling
// reason. Used to determine if a call ended cleanly, was rejected, etc.
type CallHangupPayload struct {
	CallEventPayload
	HangupCause string `json:"hangup_cause"`
	HangupSource string `json:"hangup_source"`
	SipHangupCause string `json:"sip_hangup_cause"`
}

// DTMFReceivedPayload is sent for every DTMF digit received during a call.
type DTMFReceivedPayload struct {
	CallEventPayload
	Digit string `json:"digit"`
}

// CallRecordingSavedPayload is sent when Telnyx finishes uploading a
// recording to its storage and the URL is ready to download. iReparo
// downloads from this URL and stores the audio in its own volume / S3.
type CallRecordingSavedPayload struct {
	CallControlID string   `json:"call_control_id"`
	CallLegID     string   `json:"call_leg_id"`
	CallSessionID string   `json:"call_session_id"`
	ConnectionID  string   `json:"connection_id"`
	From          string   `json:"from"`
	To            string   `json:"to"`
	RecordingID   string   `json:"recording_id"`
	Channels      string   `json:"channels"`
	URLs          struct {
		MP3 string `json:"mp3"`
		WAV string `json:"wav"`
	} `json:"recording_urls"`
	StartedAt time.Time `json:"recording_started_at"`
	EndedAt   time.Time `json:"recording_ended_at"`
	ClientState string  `json:"client_state"`
}

// MachineDetectedPayload tells iReparo whether a human or an answering
// machine picked up an outbound call. Useful for campaigns.
type MachineDetectedPayload struct {
	CallEventPayload
	Result string `json:"result"` // "human", "machine", "not_sure"
}

// ParseCallPayload extracts a CallEventPayload (the common subset) from any
// call.* event envelope. Returns an error if the event_type is not a call.*
// event.
func ParseCallPayload(env *Envelope) (*CallEventPayload, error) {
	if !isCallEvent(env.Data.EventType) {
		return nil, fmt.Errorf("telnyx: not a call event: %s", env.Data.EventType)
	}
	var payload CallEventPayload
	if err := json.Unmarshal(env.Data.Payload, &payload); err != nil {
		return nil, fmt.Errorf("telnyx: parse call payload: %w", err)
	}
	return &payload, nil
}

// ParseHangupPayload returns the typed hangup payload for call.hangup events.
func ParseHangupPayload(env *Envelope) (*CallHangupPayload, error) {
	if env.Data.EventType != EventCallHangup {
		return nil, fmt.Errorf("telnyx: not a hangup event: %s", env.Data.EventType)
	}
	var payload CallHangupPayload
	if err := json.Unmarshal(env.Data.Payload, &payload); err != nil {
		return nil, fmt.Errorf("telnyx: parse hangup payload: %w", err)
	}
	return &payload, nil
}

// ParseDTMFPayload returns the typed DTMF payload for call.dtmf.received events.
func ParseDTMFPayload(env *Envelope) (*DTMFReceivedPayload, error) {
	if env.Data.EventType != EventDTMFReceived {
		return nil, fmt.Errorf("telnyx: not a DTMF event: %s", env.Data.EventType)
	}
	var payload DTMFReceivedPayload
	if err := json.Unmarshal(env.Data.Payload, &payload); err != nil {
		return nil, fmt.Errorf("telnyx: parse dtmf payload: %w", err)
	}
	return &payload, nil
}

// ParseRecordingSavedPayload returns the typed payload for
// call.recording.saved events.
func ParseRecordingSavedPayload(env *Envelope) (*CallRecordingSavedPayload, error) {
	if env.Data.EventType != EventRecordingSaved {
		return nil, fmt.Errorf("telnyx: not a recording.saved event: %s", env.Data.EventType)
	}
	var payload CallRecordingSavedPayload
	if err := json.Unmarshal(env.Data.Payload, &payload); err != nil {
		return nil, fmt.Errorf("telnyx: parse recording.saved payload: %w", err)
	}
	return &payload, nil
}

func isCallEvent(eventType string) bool {
	switch eventType {
	case EventCallInitiated, EventCallRinging, EventCallAnswered,
		EventCallHangup, EventCallBridged, EventDTMFReceived,
		EventPlaybackStarted, EventPlaybackEnded, EventGatherEnded,
		EventMachineDetected, EventCallSpeakStarted, EventCallSpeakEnded:
		return true
	}
	return false
}
