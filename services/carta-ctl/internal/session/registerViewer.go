package session

import (
	"log/slog"
	"strconv"

	"github.com/CARTAvis/go-carta/pkg/cartaDefinitions"
	"github.com/CARTAvis/go-carta/services/carta-ctl/internal/cartaHelpers"
	"github.com/CARTAvis/go-carta/services/carta-ctl/internal/spawnerHelpers"
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

	sessionID := strconv.FormatUint(uint64(payload.SessionId), 10)
	s.SessionID = sessionID
	slog.Info("Register viewer in idle mode; starting carta-list instead of worker", "sessionId", payload.SessionId, "username", s.User.Username)
	if s.User != nil && s.User.Username != "" {
		listInfo, startErr := spawnerHelpers.RequestCartaListStartup(s.SpawnerAddress, spawnerHelpers.ListStartupRequest{
			Username:   s.User.Username,
			SessionID:  sessionID,
			SiteID:     "home",
			Token:      sessionID,
			CtlAddress: s.CallbackBaseURL,
			BaseFolder: ".",
		})
		if startErr != nil {
			slog.Warn("Failed to start carta-list", "error", startErr, "username", s.User.Username)
		} else {
			s.CartaListInfo = listInfo
			slog.Info("Requested carta-list startup", "listId", listInfo.ListId, "pid", listInfo.Pid, "sessionId", payload.SessionId)
		}
	}

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
