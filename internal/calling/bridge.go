package calling

import (
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// AudioBridge forwards RTP packets bidirectionally between two WebRTC tracks.
// It bridges the caller's remote track to the agent's local track, and vice versa.
type AudioBridge struct {
	stop     chan struct{}
	wg       sync.WaitGroup
	recorder *CallRecorder // optional, may be nil

	// lastCallerSeq and lastCallerTS track the last RTP sequence number and
	// timestamp forwarded to the caller's track (agent→caller direction).
	// Used to maintain RTP stream continuity when switching to hold music.
	lastCallerSeq uint16
	lastCallerTS  uint32
}

// NewAudioBridge creates a new audio bridge with an optional call recorder.
// If recorder is nil, no recording is performed.
func NewAudioBridge(recorder *CallRecorder) *AudioBridge {
	return &AudioBridge{
		stop:     make(chan struct{}),
		recorder: recorder,
	}
}

// Start begins bidirectional RTP forwarding. It blocks until both directions end.
func (b *AudioBridge) Start(
	callerRemote *webrtc.TrackRemote, agentLocal *webrtc.TrackLocalStaticRTP,
	agentRemote *webrtc.TrackRemote, callerLocal *webrtc.TrackLocalStaticRTP,
) {
	b.wg.Add(2)

	// Caller audio → Agent speaker
	go b.forward(callerRemote, agentLocal, false)

	// Agent mic → Caller speaker (track seq/ts for hold music continuity)
	go b.forward(agentRemote, callerLocal, true)

	b.wg.Wait()
}

// forward reads RTP packets from src and writes them to dst until stopped.
// If a recorder is attached, the Opus payload of each packet is teed to it.
func (b *AudioBridge) forward(src *webrtc.TrackRemote, dst *webrtc.TrackLocalStaticRTP, trackSeq bool) {
	defer b.wg.Done()

	buf := make([]byte, 1500)
	for {
		select {
		case <-b.stop:
			return
		default:
		}

		n, _, err := src.Read(buf)
		if err != nil {
			return
		}

		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}

		// Parse packet for recording and/or seq tracking.
		if b.recorder != nil || trackSeq {
			pkt := &rtp.Packet{}
			if err := pkt.Unmarshal(buf[:n]); err == nil {
				if trackSeq {
					b.lastCallerSeq = pkt.Header.SequenceNumber
					b.lastCallerTS = pkt.Header.Timestamp
				}
				if b.recorder != nil && len(pkt.Payload) > 0 {
					b.recorder.WritePacket(pkt.Payload)
				}
			}
		}
	}
}

// Stop terminates both forwarding goroutines.
func (b *AudioBridge) Stop() {
	safeClose(b.stop)
}

// Wait blocks until both forwarding goroutines have exited.
func (b *AudioBridge) Wait() {
	b.wg.Wait()
}

// LastCallerSeq returns the last RTP sequence number and timestamp forwarded
// to the caller's track. Only valid after Stop()+Wait().
func (b *AudioBridge) LastCallerSeq() (uint16, uint32) {
	return b.lastCallerSeq, b.lastCallerTS
}
