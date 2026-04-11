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
// In Phase 2.2 we support these node types end-to-end:
//
//   - greeting   → telnyx.PlayAudio(audio_url)
//   - hangup     → telnyx.Hangup
//   - transfer   → telnyx.Transfer(to: target)
//
// The other node types (menu, gather, http_callback, goto_flow, timing) are
// stubbed: when encountered we log a warning and play a "feature not
// available" message before hanging up. They land in Phase 2.3.

// TelnyxIVRState is the payload we encode into Telnyx's `client_state`
// to persist IVR continuation across webhook events.
type TelnyxIVRState struct {
	CallLogID   uuid.UUID `json:"clog"`
	IVRFlowID   uuid.UUID `json:"flow"`
	CurrentNode string    `json:"node"`
	Path        []string  `json:"path,omitempty"`
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
		audioURL, _ := node.Config["audio_url"].(string)
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

	case IVRNodeMenu, IVRNodeGather, IVRNodeHTTPCallback, IVRNodeGotoFlow, IVRNodeTiming:
		// Phase 2.3 territory. For now log and skip to default edge so the
		// flow does not deadlock at an unsupported node.
		return AdvanceTelnyxIVR(ctx, deps, callControlID, encoded, "default")

	default:
		_ = deps.Telnyx.Hangup(ctx, callControlID)
		return fmt.Errorf("unsupported telnyx ivr node type: %s", node.Type)
	}
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
