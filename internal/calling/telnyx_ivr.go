package calling

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/integrations/telnyx"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// This file is the Telnyx PSTN counterpart to ivr.go.
//
// The original ivr.go runs an IVR flow against a WebRTC CallSession (Pion).
// For Telnyx-driven calls there is no WebRTC session in iReparo: Telnyx owns
// the audio path, and we drive the call by sending HTTP commands and
// reacting to webhook events. The IVR is therefore implemented as a
// **stateless dispatcher** — at each webhook event, the handler calls
// AdvanceTelnyxIVR(...) which inspects the current node, executes its
// Telnyx-side action, and stores the next-step state inside Telnyx's
// `client_state` opaque field. When the next webhook arrives we decode the
// state and continue.
//
// The data structure of the flow graph (IVRFlowGraph) is reused exactly as
// designed — admins build the same visual graph for both WhatsApp and
// Telnyx calls. Only the node executors differ.
//
// Supported node types (Phase 2.3):
//
//   - greeting       → telnyx.PlayAudio(audio_url)
//   - menu           → telnyx.GatherUsingAudio with single-digit valid_digits
//   - gather         → telnyx.GatherUsingAudio with multi-digit + terminator
//   - http_callback  → synchronous HTTP request, branch on status (2xx / non-2xx)
//   - goto_flow      → switch state.IVRFlowID and recurse into new flow's entry
//   - timing         → evaluate business hours schedule, branch on in/out hours
//   - transfer       → telnyx.Transfer(to: target)
//   - hangup         → telnyx.Hangup
//
// Nodes that interact with Telnyx (greeting, menu, gather, transfer, hangup)
// return after issuing the command and the webhook handler resumes execution
// on the next event (playback.ended, gather.ended, call.bridged, etc.).
//
// Nodes that are purely server-side (http_callback, goto_flow, timing)
// compute their outcome synchronously and advance immediately by
// recursing into AdvanceTelnyxIVR / executeTelnyxNode.

// TelnyxIVRState is the payload we encode into Telnyx's `client_state`
// to persist IVR continuation across webhook events.
//
// Variables stores values gathered during the IVR (e.g. the digits collected
// by a `gather` node) so later nodes (like `http_callback`) can interpolate
// them into URLs or request bodies. The map is serialized inside client_state
// which has an 8 KB limit — IVR designers should keep stored values short.
//
// The special key "__gather_store_as" is used internally by the dispatcher
// to remember where to stash the next gather.ended digits. It is stripped
// before the state is persisted at the end of the call.
type TelnyxIVRState struct {
	CallLogID   uuid.UUID         `json:"clog"`
	IVRFlowID   uuid.UUID         `json:"flow"`
	CurrentNode string            `json:"node"`
	Path        []string          `json:"path,omitempty"`
	Variables   map[string]string `json:"vars,omitempty"`
}

// EncodeTelnyxIVRState serializes the state to a base64 string that can be
// safely passed in Telnyx's client_state field (it accepts any string up to
// 8 KB).
func EncodeTelnyxIVRState(s *TelnyxIVRState) (string, error) {
	buf, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode telnyx ivr state: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// DecodeTelnyxIVRState reverses EncodeTelnyxIVRState.
func DecodeTelnyxIVRState(encoded string) (*TelnyxIVRState, error) {
	if encoded == "" {
		return nil, errors.New("empty client_state")
	}
	buf, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode telnyx ivr state: %w", err)
	}
	var s TelnyxIVRState
	if err := json.Unmarshal(buf, &s); err != nil {
		return nil, fmt.Errorf("unmarshal telnyx ivr state: %w", err)
	}
	return &s, nil
}

// TelnyxIVRDeps groups the dependencies needed by the dispatcher functions.
// Reduces argument count and lets tests inject fakes.
type TelnyxIVRDeps struct {
	DB     *gorm.DB
	Telnyx *telnyx.Client

	// AudioURLResolver converts a locally-stored audio filename (the one the
	// WhatsApp WebRTC IVR stores in node.Config["audio_file"]) into an
	// HTTP(S) URL that Telnyx can fetch. Typically returns a short-lived
	// signed URL to the public IVR audio endpoint.
	//
	// Optional — if nil, nodes that rely on audio_file (and have no explicit
	// audio_url) are treated as silent. The webhook handler wires this to
	// App.BuildSignedIVRAudioURL.
	AudioURLResolver func(filename string) string
}

// resolveAudio returns the HTTP(S) URL Telnyx should fetch to play a prompt
// for the given IVR node. It prefers an explicit "audio_url" (set by admins
// who host their own prompts externally, e.g. on a CDN) and falls back to
// building a signed URL from "audio_file" (the local TTS-generated file).
//
// Returns "" when neither is set or the resolver is not wired.
func resolveTelnyxAudio(deps *TelnyxIVRDeps, node *IVRNode, key string) string {
	if node == nil {
		return ""
	}
	if key == "" {
		key = "audio_url"
	}
	if url, _ := node.Config[key].(string); url != "" {
		return url
	}
	// Fallback: local file → signed public URL.
	fileKey := "audio_file"
	if key == "invalid_audio_url" {
		fileKey = "invalid_audio_file"
	}
	file, _ := node.Config[fileKey].(string)
	if file == "" || deps == nil || deps.AudioURLResolver == nil {
		return ""
	}
	return deps.AudioURLResolver(file)
}

// LoadIVRFlowGraph parses an IVRFlow.Menu JSONB into a usable IVRFlowGraph.
// Returns nil + error if the menu is missing or invalid.
func LoadIVRFlowGraph(flow *models.IVRFlow) (*IVRFlowGraph, error) {
	if flow == nil || flow.Menu == nil {
		return nil, errors.New("ivr flow has no menu")
	}
	menuBytes, err := json.Marshal(flow.Menu)
	if err != nil {
		return nil, fmt.Errorf("marshal menu: %w", err)
	}
	var graph IVRFlowGraph
	if err := json.Unmarshal(menuBytes, &graph); err != nil {
		return nil, fmt.Errorf("unmarshal menu: %w", err)
	}
	if graph.Version != 2 || graph.EntryNode == "" {
		return nil, fmt.Errorf("invalid graph version=%d entry=%q", graph.Version, graph.EntryNode)
	}
	graph.buildMaps()
	return &graph, nil
}

// StartTelnyxIVR is invoked by the webhook handler when a `call.initiated`
// event arrives for a call routed to one of our Telnyx numbers.
//
// It answers the call (so Telnyx will start billing inbound minutes — which
// for Spanish geographic numbers is free for both sides) and immediately
// kicks off the IVR by executing the entry node.
func StartTelnyxIVR(
	ctx context.Context,
	deps *TelnyxIVRDeps,
	callControlID string,
	callLog *models.CallLog,
	flow *models.IVRFlow,
) error {
	if deps == nil || deps.Telnyx == nil {
		return errors.New("telnyx ivr: missing deps")
	}

	graph, err := LoadIVRFlowGraph(flow)
	if err != nil {
		// No flow configured or broken. Answer the call, play a generic
		// "this number is not configured" message, hang up.
		if answerErr := deps.Telnyx.Answer(ctx, callControlID); answerErr != nil {
			return fmt.Errorf("answer fallback: %w", answerErr)
		}
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("load ivr graph: %w", err)
	}

	// Answer the call. Telnyx will then send a call.answered event back
	// which we use to update the CallLog. Recording (if enabled) starts
	// after the answer too — we issue a record_start in a separate
	// command for clarity.
	if err := deps.Telnyx.Answer(ctx, callControlID); err != nil {
		return fmt.Errorf("answer: %w", err)
	}

	// Begin executing the entry node.
	state := &TelnyxIVRState{
		CallLogID:   callLog.ID,
		IVRFlowID:   flow.ID,
		CurrentNode: graph.EntryNode,
		Path:        []string{graph.EntryNode},
	}
	return executeTelnyxNode(ctx, deps, callControlID, callLog, graph, state)
}

// AdvanceTelnyxIVR is invoked by the webhook handler when a node-completion
// event arrives (call.playback.ended, call.gather.ended, etc.). It decodes
// the client_state, resolves the next node based on the event outcome, and
// executes it.
//
//	outcome — the edge condition string ("default", "digit:1", "timeout", etc.)
func AdvanceTelnyxIVR(
	ctx context.Context,
	deps *TelnyxIVRDeps,
	callControlID string,
	clientState string,
	outcome string,
) error {
	state, err := DecodeTelnyxIVRState(clientState)
	if err != nil {
		// Without state we cannot continue, so just hang up.
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("decode client_state: %w", err)
	}

	// Reload flow + call log from DB on every step. This makes the
	// dispatcher resilient to process restarts and to multiple
	// instances of iReparo running behind a load balancer.
	var flow models.IVRFlow
	if err := deps.DB.Where("id = ?", state.IVRFlowID).First(&flow).Error; err != nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("reload flow: %w", err)
	}
	graph, err := LoadIVRFlowGraph(&flow)
	if err != nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("reload graph: %w", err)
	}

	var callLog models.CallLog
	if err := deps.DB.Where("id = ?", state.CallLogID).First(&callLog).Error; err != nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("reload call log: %w", err)
	}

	// Resolve the next node from the current edge.
	nextNodeID := graph.resolveEdge(state.CurrentNode, outcome)
	if nextNodeID == "" {
		// No outgoing edge for this outcome — flow is over. Hang up.
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return nil
	}

	state.CurrentNode = nextNodeID
	state.Path = append(state.Path, nextNodeID)
	return executeTelnyxNode(ctx, deps, callControlID, &callLog, graph, state)
}

// AdvanceTelnyxIVRAfterGather is the specialized continuation invoked by
// the webhook handler when a call.gather.ended event arrives. It:
//
//  1. Decodes the client_state.
//  2. Loads the current node to know whether this gather belongs to a
//     `menu` or a `gather` node (they route differently).
//  3. For `gather` nodes: stores the digits in state.Variables under the
//     key flagged as "__gather_store_as", and routes along the "default"
//     edge (or "timeout" if no digits were collected).
//  4. For `menu` nodes: routes along "digit:N" (or "timeout").
//  5. Delegates the rest of the walk to AdvanceTelnyxIVR with the
//     mutated state.
//
// Splitting this from AdvanceTelnyxIVR keeps the hot path (playback.ended,
// call.bridged, …) free of DB work it does not need.
func AdvanceTelnyxIVRAfterGather(
	ctx context.Context,
	deps *TelnyxIVRDeps,
	callControlID string,
	clientState string,
	digits string,
) error {
	state, err := DecodeTelnyxIVRState(clientState)
	if err != nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("decode client_state: %w", err)
	}

	var flow models.IVRFlow
	if err := deps.DB.Where("id = ?", state.IVRFlowID).First(&flow).Error; err != nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("reload flow: %w", err)
	}
	graph, err := LoadIVRFlowGraph(&flow)
	if err != nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("reload graph: %w", err)
	}
	node := graph.getNode(state.CurrentNode)

	var outcome string
	switch {
	case node == nil:
		outcome = "default"

	case node.Type == IVRNodeGather:
		storeAs := ""
		if state.Variables != nil {
			storeAs = state.Variables["__gather_store_as"]
			delete(state.Variables, "__gather_store_as")
		}
		if digits == "" {
			outcome = "timeout"
		} else {
			if storeAs != "" {
				if state.Variables == nil {
					state.Variables = map[string]string{}
				}
				state.Variables[storeAs] = digits
			}
			outcome = "default"
		}

	default: // menu (and any other caller using gather_using_audio)
		if digits == "" {
			outcome = "timeout"
		} else {
			outcome = "digit:" + digits
		}
	}

	newClientState, err := EncodeTelnyxIVRState(state)
	if err != nil {
		return fmt.Errorf("re-encode client_state: %w", err)
	}
	return AdvanceTelnyxIVR(ctx, deps, callControlID, newClientState, outcome)
}

// executeTelnyxNode is the dispatcher heart: it looks at the current node
// type and emits the corresponding Telnyx command, with `state` encoded as
// the new client_state so the next webhook event can find its way back here.
func executeTelnyxNode(
	ctx context.Context,
	deps *TelnyxIVRDeps,
	callControlID string,
	callLog *models.CallLog,
	graph *IVRFlowGraph,
	state *TelnyxIVRState,
) error {
	node := graph.getNode(state.CurrentNode)
	if node == nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("node not found: %s", state.CurrentNode)
	}

	encoded, err := EncodeTelnyxIVRState(state)
	if err != nil {
		return err
	}

	// Persist the IVR path on the CallLog so we have observability into
	// which nodes the call walked through.
	persistTelnyxIVRPath(deps.DB, callLog.ID, state)

	switch node.Type {
	case IVRNodeGreeting:
		audioURL := resolveTelnyxAudio(deps, node, "audio_url")
		if audioURL == "" {
			// Greeting with no audio — skip directly to the next node.
			return AdvanceTelnyxIVR(ctx, deps, callControlID, encoded, "default")
		}
		return deps.Telnyx.PlayAudio(ctx, callControlID, &telnyx.PlaybackRequest{
			AudioURL:    audioURL,
			ClientState: encoded,
		})

	case IVRNodeTransfer:
		target, _ := node.Config["target_phone"].(string)
		if target == "" {
			// Transfer with no target — fall through to default edge.
			return AdvanceTelnyxIVR(ctx, deps, callControlID, encoded, "default")
		}
		fromNumber, _ := node.Config["from_number"].(string)
		return deps.Telnyx.Transfer(ctx, callControlID, &telnyx.TransferRequest{
			To:          target,
			From:        fromNumber,
			ClientState: encoded,
		})

	case IVRNodeHangup:
		return deps.Telnyx.Hangup(ctx, callControlID)

	case IVRNodeMenu:
		return executeTelnyxMenu(ctx, deps, callControlID, node, state, encoded)

	case IVRNodeGather:
		return executeTelnyxGather(ctx, deps, callControlID, node, state)

	case IVRNodeHTTPCallback:
		outcome := executeTelnyxHTTPCallback(node, state)
		return AdvanceTelnyxIVR(ctx, deps, callControlID, encoded, outcome)

	case IVRNodeTiming:
		outcome := executeTelnyxTiming(node, time.Now())
		return AdvanceTelnyxIVR(ctx, deps, callControlID, encoded, outcome)

	case IVRNodeGotoFlow:
		return executeTelnyxGotoFlow(ctx, deps, callControlID, callLog, node, state)

	default:
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("unsupported telnyx ivr node type: %s", node.Type)
	}
}

// executeTelnyxMenu plays a prompt and gathers a single digit constrained
// by the node's `options` map. On gather.ended, the webhook handler decodes
// the digits and advances the flow along the "digit:N" edge (or "timeout"
// if no digit was collected).
//
// Config fields:
//   - audio_url (required)   — URL of the prompt audio (MP3/WAV over HTTPS)
//   - options (required)     — map of {digit: {label, ...}}. Keys are the
//                              valid digits (e.g. {"1": {...}, "2": {...}}).
//   - timeout_seconds        — per-attempt timeout (default 10s)
//   - invalid_audio_url      — played by Telnyx if caller presses an
//                              unlisted digit (optional)
func executeTelnyxMenu(
	ctx context.Context,
	deps *TelnyxIVRDeps,
	callControlID string,
	node *IVRNode,
	state *TelnyxIVRState,
	encoded string,
) error {
	audioURL := resolveTelnyxAudio(deps, node, "audio_url")
	if audioURL == "" {
		// No prompt → skip directly to default edge (the flow designer
		// probably meant to chain a greeting node in front).
		return AdvanceTelnyxIVR(ctx, deps, callControlID, encoded, "default")
	}

	validDigits := menuValidDigits(node)
	timeoutMs := getConfigIntT(node.Config, "timeout_seconds", 10) * 1000
	invalidAudioURL := resolveTelnyxAudio(deps, node, "invalid_audio_url")

	return deps.Telnyx.GatherUsingAudio(ctx, callControlID, &telnyx.GatherUsingAudioRequest{
		AudioURL:        audioURL,
		MinDigits:       1,
		MaxDigits:       1,
		TimeoutMillis:   timeoutMs,
		ValidDigits:     validDigits,
		InvalidAudioURL: invalidAudioURL,
		ClientState:     encoded,
	})
}

// executeTelnyxGather plays a prompt and gathers multiple DTMF digits up to
// the configured terminator or max_digits. The `store_as` key is stashed in
// state.Variables under the special key "__gather_store_as" so the webhook
// handler knows where to put the digits when gather.ended arrives.
//
// Config fields:
//   - audio_url (optional)   — prompt audio; if empty, gather starts silently
//   - max_digits             — default 10
//   - terminator             — default "#"
//   - timeout_seconds        — default 10
//   - store_as               — variable name to store the collected digits
func executeTelnyxGather(
	ctx context.Context,
	deps *TelnyxIVRDeps,
	callControlID string,
	node *IVRNode,
	state *TelnyxIVRState,
) error {
	audioURL := resolveTelnyxAudio(deps, node, "audio_url")
	maxDigits := getConfigIntT(node.Config, "max_digits", 10)
	terminator, _ := node.Config["terminator"].(string)
	if terminator == "" {
		terminator = "#"
	}
	timeoutMs := getConfigIntT(node.Config, "timeout_seconds", 10) * 1000
	storeAs, _ := node.Config["store_as"].(string)

	// Remember where to stash the digits when gather.ended arrives.
	if storeAs != "" {
		if state.Variables == nil {
			state.Variables = map[string]string{}
		}
		state.Variables["__gather_store_as"] = storeAs
	}

	// Re-encode state after the mutation above.
	encoded, err := EncodeTelnyxIVRState(state)
	if err != nil {
		return err
	}

	if audioURL == "" {
		// No prompt → plain gather (no audio). Telnyx requires audio_url
		// on gather_using_audio, so fall back to the silent gather action.
		return deps.Telnyx.Gather(ctx, callControlID, &telnyx.GatherRequest{
			MinDigits:        1,
			MaxDigits:        maxDigits,
			TerminatingDigit: terminator,
			TimeoutMillis:    timeoutMs,
			ClientState:      encoded,
		})
	}

	return deps.Telnyx.GatherUsingAudio(ctx, callControlID, &telnyx.GatherUsingAudioRequest{
		AudioURL:         audioURL,
		MinDigits:        1,
		MaxDigits:        maxDigits,
		TerminatingDigit: terminator,
		TimeoutMillis:    timeoutMs,
		ClientState:      encoded,
	})
}

// executeTelnyxHTTPCallback makes a synchronous HTTP request and returns
// "http:2xx" or "http:non2xx" so the flow can branch on the result. The URL,
// headers and body template are interpolated with state.Variables so they
// can carry data gathered earlier in the flow (e.g. a customer PIN).
//
// Config fields:
//   - url (required)
//   - method                 — default "GET"
//   - headers                — map of header → value (value templated)
//   - body_template          — raw string templated with {{var}} placeholders
//   - timeout_seconds        — default 10
//   - response_store_as      — optional variable to store the response body
func executeTelnyxHTTPCallback(node *IVRNode, state *TelnyxIVRState) string {
	url, _ := node.Config["url"].(string)
	if url == "" {
		return "http:non2xx"
	}
	method, _ := node.Config["method"].(string)
	if method == "" {
		method = "GET"
	}
	bodyTemplate, _ := node.Config["body_template"].(string)
	timeoutSecs := getConfigIntT(node.Config, "timeout_seconds", 10)
	responseStoreAs, _ := node.Config["response_store_as"].(string)

	// Snapshot variables for template interpolation (strip internal keys).
	vars := publicVariables(state.Variables)

	headers := map[string]string{}
	if headersRaw, ok := node.Config["headers"].(map[string]any); ok {
		for k, v := range headersRaw {
			if s, ok := v.(string); ok {
				headers[k] = interpolateTemplate(s, vars)
			}
		}
	}

	url = interpolateTemplate(url, vars)
	body := interpolateTemplate(bodyTemplate, vars)

	result, err := executeHTTPCallback(url, method, headers, body, time.Duration(timeoutSecs)*time.Second)
	if err != nil {
		return "http:non2xx"
	}
	if responseStoreAs != "" {
		if state.Variables == nil {
			state.Variables = map[string]string{}
		}
		// Cap stored response to 1 KB so client_state stays under the 8 KB
		// ceiling even after accumulated gather results.
		stored := result.Body
		if len(stored) > 1024 {
			stored = stored[:1024]
		}
		state.Variables[responseStoreAs] = stored
	}
	if result.StatusCode >= 200 && result.StatusCode < 300 {
		return "http:2xx"
	}
	return "http:non2xx"
}

// executeTelnyxTiming evaluates the node's business-hours schedule against
// the supplied time and returns "in_hours" or "out_of_hours". The time is
// injected (rather than read from time.Now) to make the function unit
// testable.
//
// Config.schedule is an array of entries:
//
//	[
//	  {"day": "monday", "enabled": true,  "start_time": "09:00", "end_time": "18:00"},
//	  {"day": "sunday", "enabled": false, "start_time": "00:00", "end_time": "00:00"}
//	]
func executeTelnyxTiming(node *IVRNode, now time.Time) string {
	dayName := strings.ToLower(now.Weekday().String())
	scheduleRaw, _ := node.Config["schedule"].([]any)
	for _, item := range scheduleRaw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if day, _ := entry["day"].(string); strings.ToLower(day) != dayName {
			continue
		}
		if enabled, _ := entry["enabled"].(bool); !enabled {
			return "out_of_hours"
		}
		startStr, _ := entry["start_time"].(string)
		endStr, _ := entry["end_time"].(string)
		startT, err1 := time.Parse("15:04", startStr)
		endT, err2 := time.Parse("15:04", endStr)
		if err1 != nil || err2 != nil {
			return "out_of_hours"
		}
		nowMins := now.Hour()*60 + now.Minute()
		startMins := startT.Hour()*60 + startT.Minute()
		endMins := endT.Hour()*60 + endT.Minute()
		if nowMins >= startMins && nowMins < endMins {
			return "in_hours"
		}
		return "out_of_hours"
	}
	return "out_of_hours"
}

// executeTelnyxGotoFlow switches the in-flight IVR to a different flow by
// updating state.IVRFlowID and continuing from the new flow's entry node.
// On any error (target disabled, missing, corrupt graph) the call is hung
// up — not silently dropped — because the flow designer explicitly pointed
// at it and we have no sensible fallback.
//
// Config fields:
//   - flow_id (required)  — UUID of the target IVRFlow
func executeTelnyxGotoFlow(
	ctx context.Context,
	deps *TelnyxIVRDeps,
	callControlID string,
	callLog *models.CallLog,
	node *IVRNode,
	state *TelnyxIVRState,
) error {
	flowIDStr, _ := node.Config["flow_id"].(string)
	if flowIDStr == "" {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return errors.New("telnyx goto_flow: missing flow_id")
	}
	targetID, err := uuid.Parse(flowIDStr)
	if err != nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("telnyx goto_flow: invalid flow_id: %w", err)
	}

	var target models.IVRFlow
	if err := deps.DB.Where("id = ? AND is_active = ?", targetID, true).First(&target).Error; err != nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("telnyx goto_flow: load target flow: %w", err)
	}
	targetGraph, err := LoadIVRFlowGraph(&target)
	if err != nil {
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("telnyx goto_flow: parse target graph: %w", err)
	}

	// Update state to point at the new flow's entry node. Append a marker
	// in the path so observability shows the jump.
	state.IVRFlowID = target.ID
	state.CurrentNode = targetGraph.EntryNode
	state.Path = append(state.Path, "goto:"+target.Name, targetGraph.EntryNode)

	// Reflect the jump on the CallLog so the admin UI shows the right
	// flow for the rest of the call.
	_ = deps.DB.Model(&models.CallLog{}).
		Where("id = ?", callLog.ID).
		Update("ivr_flow_id", target.ID).Error

	return executeTelnyxNode(ctx, deps, callControlID, callLog, targetGraph, state)
}

// menuValidDigits builds the Telnyx `valid_digits` constraint string from a
// menu node's options map. Options are stored as {"1": {...}, "2": {...}}
// so the valid digits are the keys concatenated.
func menuValidDigits(node *IVRNode) string {
	opts, ok := node.Config["options"].(map[string]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for digit := range opts {
		if len(digit) == 1 && (digit[0] >= '0' && digit[0] <= '9' || digit[0] == '*' || digit[0] == '#') {
			b.WriteString(digit)
		}
	}
	return b.String()
}

// publicVariables returns a copy of the variables map with internal keys
// (those prefixed with "__") stripped. Used before templating so callers
// cannot accidentally expose dispatcher bookkeeping in URLs / headers.
func publicVariables(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if strings.HasPrefix(k, "__") {
			continue
		}
		out[k] = v
	}
	return out
}

// getConfigIntT extracts an int from a config map with a default fallback.
// Mirrors getConfigInt in ivr.go but lives here to avoid coupling the Telnyx
// dispatcher to the WebRTC IVR executor.
func getConfigIntT(config map[string]any, key string, defaultVal int) int {
	v, ok := config[key]
	if !ok {
		return defaultVal
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	}
	return defaultVal
}

// persistTelnyxIVRPath appends the current node to the CallLog's ivr_path
// JSONB column for observability. Best effort — failures are not fatal.
func persistTelnyxIVRPath(db *gorm.DB, callLogID uuid.UUID, state *TelnyxIVRState) {
	steps := make([]map[string]string, 0, len(state.Path))
	for _, n := range state.Path {
		steps = append(steps, map[string]string{
			"node": n,
			"at":   time.Now().UTC().Format(time.RFC3339Nano),
		})
	}
	path := models.JSONB{"steps": steps, "channel": "telnyx_pstn"}
	_ = db.Model(&models.CallLog{}).
		Where("id = ?", callLogID).
		Update("ivr_path", path).Error
}

// MapTelnyxOutcome converts a Telnyx event type into the IVRFlowGraph edge
// condition that should be used for resolving the next node.
//
// This is the bridge between Telnyx's event vocabulary and the visual flow
// editor's edge conditions ("default", "timeout", "digit:1", etc.).
func MapTelnyxOutcome(eventType string, payload map[string]any) string {
	switch eventType {
	case telnyx.EventPlaybackEnded:
		// Playback ended naturally → walk the default edge.
		return "default"
	case telnyx.EventGatherEnded:
		if d, ok := payload["digits"].(string); ok && d != "" {
			return "digit:" + d
		}
		return "timeout"
	case telnyx.EventDTMFReceived:
		if d, ok := payload["digit"].(string); ok && d != "" {
			return "digit:" + d
		}
	case telnyx.EventCallBridged:
		return "default"
	}
	return "default"
}

// PhoneToE164 normalizes a phone string to E.164 without the leading "+".
// Mirrors the format the CRM uses (preg_replace('/\D/', '', $phone)).
//
//	+34 873 94 07 02   →  34873940702
//	0034873940702      →  34873940702 (drops the 00)
//	34873940702        →  34873940702
func PhoneToE164(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if strings.HasPrefix(out, "00") {
		out = out[2:]
	}
	return out
}
