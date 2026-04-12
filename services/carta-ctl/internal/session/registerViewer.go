package session

import (
	"log/slog"

	"github.com/CARTAvis/go-carta/pkg/cartaDefinitions"
	"github.com/CARTAvis/go-carta/services/carta-ctl/internal/cartaHelpers"
)

// RegisterViewer is the first message we receive from the frontend.
// In prototype idle mode we acknowledge it immediately without starting any
// worker or lister process. This allows the frontend to connect cleanly to
// carta-ctl before any runtime process exists.
func (s *Session) handleRegisterViewerMessage(_ cartaDefinitions.EventType, requestId uint32, msg []byte) error {
	var payload cartaDefinitions.RegisterViewer
	err := s.checkAndParse(&payload, requestId, msg)
	if err != nil {
		return err
	}

	slog.Info("Register viewer in idle mode; not starting worker", "sessionId", payload.SessionId, "username", s.User.Username)

	ack := &cartaDefinitions.RegisterViewerAck{
		SessionId:          payload.SessionId,
		Success:            true,
		Message:            "",
		SessionType:        cartaDefinitions.SessionType_NEW,
		ServerFeatureFlags: 0,
		UserPreferences:    map[string]string{},
		UserLayouts:        map[string]string{},
		PlatformStrings:    map[string]string{},
	}

	reply, err := cartaHelpers.PrepareMessagePayload(
		ack,
		cartaDefinitions.EventType_REGISTER_VIEWER_ACK,
		requestId,
	)
	if err != nil {
		return err
	}

	s.clientSendChan <- reply
	return nil
}
