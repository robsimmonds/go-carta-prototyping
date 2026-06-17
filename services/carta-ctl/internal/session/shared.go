package session

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/CARTAvis/go-carta/pkg/cartaDefinitions"
	"github.com/CARTAvis/go-carta/services/carta-ctl/internal/cartaHelpers"
	"github.com/CARTAvis/go-carta/services/carta-ctl/internal/spawnerHelpers"
)

func sendHandler(channel <-chan []byte, conn *websocket.Conn, name string) {
	slog.Debug("Starting send handler", "name", name, "channel", fmt.Sprintf("%p", channel))
	for byteData := range channel {
		err := conn.WriteMessage(websocket.BinaryMessage, byteData)
		if err != nil {
			slog.Error("Error sending message", "name", name, "channel", fmt.Sprintf("%p", channel), "error", err)
			// Continue processing other messages even if one fails
		}
	}
	slog.Debug("Send handler exiting", "name", name)
}

// handleProxiedMessage proxies unhandled messages to the appropriate worker.
// Messages that target an opened file go to that file's worker; everything else
// (notably FILE_LIST_REQUEST) goes to the shared listing worker. If the shared
// worker isn't connected yet, the message is buffered and flushed once it is.
func (s *Session) handleProxiedMessage(eventType cartaDefinitions.EventType, requestId uint32, bytes []byte) error {
	messageBytes := cartaHelpers.PrepareBinaryMessage(bytes, eventType, requestId)

	// If the message targets a specific opened file, route to that worker.
	fileId, hasFileId := cartaHelpers.ExtractFileIdFromBytes(eventType, bytes)
	if hasFileId && s.fileMap != nil {
		if worker, exists := s.fileMap[fileId]; exists {
			slog.Debug("Proxying message to file worker", "eventType", eventType, "fileId", fileId)
			worker.sendChan <- messageBytes
			return nil
		}
	}

	// Otherwise route to the selected site. "home"/"" uses the local shared
	// listing worker (buffering until it is ready); a remote site uses its
	// per-session connection.
	s.mu.Lock()
	site := s.selectedSite
	if site == "" || site == "home" {
		if s.sharedWorker == nil {
			s.pendingShared = append(s.pendingShared, messageBytes)
			s.mu.Unlock()
			slog.Debug("Buffered message until shared listing worker is ready", "eventType", eventType, "requestId", requestId)
			return nil
		}
		worker := s.sharedWorker
		s.mu.Unlock()
		slog.Debug("Proxying message to home shared listing worker", "eventType", eventType, "requestId", requestId)
		worker.sendChan <- messageBytes
		return nil
	}

	worker := s.siteWorkers[site]
	s.mu.Unlock()
	if worker == nil {
		slog.Warn("Dropping proxied message: selected site has no connection", "site", site, "eventType", eventType, "requestId", requestId)
		return nil
	}
	slog.Debug("Proxying message to remote site", "site", site, "eventType", eventType, "requestId", requestId)
	worker.sendChan <- messageBytes
	return nil
}

// connectSharedWorker dials the carta_backend that a carta-list agent spawned
// for file listing, registers a viewer so it will accept FILE_LIST_REQUEST,
// attaches it as the session's shared worker, and flushes any messages that
// were buffered while waiting for it.
func (s *Session) connectSharedWorker(backendAddress string) error {
	addr := backendAddress
	if !strings.HasPrefix(addr, "ws://") && !strings.HasPrefix(addr, "wss://") {
		addr = "ws://" + addr
	}

	conn, _, err := websocket.DefaultDialer.DialContext(s.Context, addr, nil)
	if err != nil {
		return fmt.Errorf("dial shared listing worker at %s: %w", addr, err)
	}

	worker := &SessionWorker{
		conn:           conn,
		clientSendChan: s.clientSendChan,
	}
	worker.handleInit()

	// Register a viewer so the backend establishes a session. Its
	// REGISTER_VIEWER_ACK is swallowed by the shared-worker path in
	// workerMessageHandler (the client already received one from carta-ctl).
	reg := &cartaDefinitions.RegisterViewer{SessionId: 0, ClientFeatureFlags: 0}
	if err := worker.proxyMessageToWorker(reg, cartaDefinitions.EventType_REGISTER_VIEWER, 1); err != nil {
		worker.disconnect()
		return fmt.Errorf("register viewer with shared listing worker: %w", err)
	}

	s.mu.Lock()
	s.sharedWorker = worker
	pending := s.pendingShared
	s.pendingShared = nil
	s.mu.Unlock()

	for _, m := range pending {
		worker.sendChan <- m
	}
	if len(pending) > 0 {
		slog.Info("Flushed buffered messages to shared listing worker", "count", len(pending))
	}
	return nil
}

func (s *Session) handleStatusMessage(_ cartaDefinitions.EventType, _ uint32, _ []byte) error {
	if s.Info.WorkerId == "" {
		slog.Debug("Ignoring status request because no worker exists yet")
		return nil
	}
	status, err := spawnerHelpers.GetWorkerStatus(s.Info.WorkerId, s.SpawnerAddress)
	if err != nil {
		return fmt.Errorf("error getting worker status: %v", err)
	} else {
		slog.Info("Worker status", "alive", status.Alive, "reachable", status.IsReachable)
	}
	return nil
}
