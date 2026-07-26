package processors

import (
	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/logger"
	"github.com/weedge/pipeline-go/pkg/processors"

	"achatbot/pkg/params"
	achatbot_frames "achatbot/pkg/types/frames"
)

// interruptionSerializer is implemented by serializers that encode an
// interruption natively on the wire (the Telnyx one emits a "clear" event).
// Serializers without it — notably the browser's protobuf format, whose IDL
// only defines text/audio/image frames — need the control message below.
type interruptionSerializer interface {
	SupportsInterruption() bool
}

// turnStarter is implemented by serializers that need to know when a new bot
// turn begins, so they can stop discarding audio left over from an interrupted
// one. See telnyx.Serializer.BeginTurn.
type turnStarter interface {
	BeginTurn()
}

// InterruptControlName tags the TextFrame sent to clients whose wire format has
// no interruption frame. The browser treats it as "stop playback and drop
// everything buffered"; without it the server interrupts the pipeline while the
// browser keeps playing the reply the user just talked over.
const InterruptControlName = "interrupt"

// WebsocketServerOutputProcessor processes output for  WebSocket server
type WebsocketServerOutputProcessor struct {
	*AudioCameraOutputProcessor
	params *params.WebsocketServerParams
}

// NewWebsocketServerOutputProcessor creates a new WebsocketServerOutputProcessor
func NewWebsocketServerOutputProcessor(
	name string,
	params *params.WebsocketServerParams,
) *WebsocketServerOutputProcessor {
	p := &WebsocketServerOutputProcessor{
		AudioCameraOutputProcessor: NewAudioCameraOutputProcessor(name, params.AudioCameraParams),
		params:                     params,
	}

	return p
}

// ProcessFrame processes a frame
func (p *WebsocketServerOutputProcessor) ProcessFrame(frame frames.Frame, direction processors.FrameDirection) {
	// Call parent implementation
	p.AudioCameraOutputProcessor.ProcessFrame(frame, direction)

	// Handle specific frame types
	switch f := frame.(type) {
	case *achatbot_frames.VADStateAudioRawFrame:
		p.handleAudio(f.AudioRawFrame)
	case *achatbot_frames.TTSStartedFrame:
		// A new reply is starting, so outbound audio is no longer the tail of an
		// interrupted one.
		if ts, ok := p.params.Serializer.(turnStarter); ok {
			ts.BeginTurn()
		}
	case *frames.StartInterruptionFrame:
		p.sendInterruption(f)
	}
}

// sendInterruption tells the client to stop playing immediately. Serializers
// that encode interruptions natively get the frame itself; the rest get a named
// TextFrame, because attempting the raw frame would only fail to serialize and
// leave the client playing audio the user has already interrupted.
func (p *WebsocketServerOutputProcessor) sendInterruption(f *frames.StartInterruptionFrame) {
	if s, ok := p.params.Serializer.(interruptionSerializer); ok && s.SupportsInterruption() {
		if err := p.transportWriter.WriteFrame(f); err != nil {
			logger.Error("Error send StartInterruptionFrame", "error", err)
		}
		return
	}
	ctrl := &frames.TextFrame{
		DataFrame: frames.NewDataFrameWithName(InterruptControlName),
		Text:      "",
	}
	if err := p.transportWriter.WriteFrame(ctrl); err != nil {
		logger.Error("Error send interrupt control frame", "error", err)
	}
}
