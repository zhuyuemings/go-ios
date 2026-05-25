package instruments

import (
	"fmt"

	"github.com/danielpaulus/go-ios/ios"
	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	log "github.com/sirupsen/logrus"
)

const (
	coreanimationName  = "com.apple.instruments.server.services.coreanimation"
	graphicsOpenglName = "com.apple.instruments.server.services.graphics.opengl"
)

type graphicsMsgDispatcher struct {
	messages chan dtx.Message
	conn     *dtx.Connection
}

func newGraphicsMsgDispatcher() *graphicsMsgDispatcher {
	return &graphicsMsgDispatcher{messages: make(chan dtx.Message, 1000)}
}

func (p *graphicsMsgDispatcher) Dispatch(m dtx.Message) {
	if p.conn != nil {
		dtx.SendAckIfNeeded(p.conn, m)
	}
	p.messages <- m
}

// GraphicsService handles FPS monitoring
type GraphicsService struct {
	channel       *dtx.Channel
	conn          *dtx.Connection
	msgDispatcher *graphicsMsgDispatcher
}

// NewGraphicsService creates a new GraphicsService with fallback logic.
func NewGraphicsService(device ios.DeviceEntry) (*GraphicsService, error) {
	msgDispatcher := newGraphicsMsgDispatcher()
	dtxConn, err := connectInstrumentsWithMsgDispatcher(device, msgDispatcher)
	if err != nil {
		return nil, err
	}
	msgDispatcher.conn = dtxConn

	// Open both channels and send async start request.
	// The device will only stream from the one it supports.
	coreChan := dtxConn.RequestChannelIdentifier(coreanimationName, msgDispatcher)
	coreChan.MethodCallAsync("startSamplingAtTimeInterval:", float64(0.0))

	openglChan := dtxConn.RequestChannelIdentifier(graphicsOpenglName, msgDispatcher)
	openglChan.MethodCallAsync("startSamplingAtTimeInterval:", float64(0.0))

	return &GraphicsService{channel: openglChan, conn: dtxConn, msgDispatcher: msgDispatcher}, nil
}

// Close closes the connection
func (s *GraphicsService) Close() error {
	// stop sampling before close
	s.channel.MethodCallAsync("stopSampling")
	close(s.msgDispatcher.messages)
	return s.conn.Close()
}

// GraphicsMessage represents FPS data
type GraphicsMessage struct {
	FPS float64
}

// ReceiveFPS returns a channel that streams GraphicsMessage
func (s *GraphicsService) ReceiveFPS() chan GraphicsMessage {
	messages := make(chan GraphicsMessage)
	go func() {
		defer close(messages)

		firstFPS := true
		for msg := range s.msgDispatcher.messages {
			if len(msg.Payload) == 0 {
				continue
			}

			// Parse payload to get FPS
			// The payload is typically []interface{} containing arrays/dicts
			fps, err := extractFPS(msg.Payload)
			if err != nil {
				// Avoid logging every unrecognized message as it might be noisy
				continue
			}

			if firstFPS && fps == 0 {
				firstFPS = false
				continue
			}
			firstFPS = false

			messages <- GraphicsMessage{FPS: fps}
		}
		log.Infof("graphics message dispatcher channel closed")
	}()

	return messages
}

func extractFPS(payload []interface{}) (float64, error) {
	if len(payload) == 0 {
		return 0, fmt.Errorf("empty payload")
	}

	var resultMap map[string]interface{}
	var ok bool

	// Check if payload[0] is directly a map
	resultMap, ok = payload[0].(map[string]interface{})
	if !ok {
		// Try fallback: maybe it's nested inside an array?
		resultArray, okArray := payload[0].([]interface{})
		if okArray && len(resultArray) > 0 {
			resultMap, ok = resultArray[0].(map[string]interface{})
		}
	}

	if !ok {
		return 0, fmt.Errorf("not a dict")
	}

	// Try extracting CoreAnimationFramesPerSecond
	if val, ok := resultMap["CoreAnimationFramesPerSecond"]; ok {
		if fps, valid := ToFloat64(val); valid {
			return fps, nil
		}
	}

	// Fallback check for FPS if it's named differently
	if val, ok := resultMap["FPS"]; ok {
		if fps, valid := ToFloat64(val); valid {
			return fps, nil
		}
	}

	return 0, fmt.Errorf("fps field not found")
}
