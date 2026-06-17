package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/CARTAvis/go-carta/pkg/cartaDefinitions"
	"github.com/CARTAvis/go-carta/services/carta-ctl/internal/auth"
	"github.com/CARTAvis/go-carta/services/carta-ctl/internal/cartaHelpers"
	"github.com/CARTAvis/go-carta/services/carta-ctl/internal/spawnerHelpers"
)

type contextKey string

const UserContextKey contextKey = "sessionUser"

type Session struct {
	Info            spawnerHelpers.WorkerInfo
	CartaListInfo   spawnerHelpers.ListProcessInfo
	SpawnerAddress  string
	CallbackBaseURL string
	WebSocket       *websocket.Conn
	User            *auth.User
	SessionID       string
	Context         context.Context
	Cancel          context.CancelFunc

	clientSendChan chan outboundMessage
	// maps incoming file IDs to the internal IDs of the workers
	fileMap map[int32]*SessionWorker

	// mu guards sharedWorker, pendingShared, selectedSite and siteWorkers,
	// which are written from out-of-band goroutines (carta-list registration
	// callback, control-message handling) and read from the websocket loop.
	mu           sync.Mutex
	sharedWorker *SessionWorker
	// pendingShared buffers proxied messages (e.g. FILE_LIST_REQUEST) that
	// arrive before the shared listing worker has connected. They are flushed
	// in order once it is ready.
	pendingShared [][]byte
	// selectedSite is the site that non-file-specific proxied traffic (file
	// listing) is routed to. "" or "home" means the local shared worker.
	selectedSite string
	// siteWorkers holds connections to remote carta-ctl sites, keyed by site id.
	// "home" is not stored here — it uses sharedWorker.
	siteWorkers map[string]*SessionWorker
}

var handlerMap = map[cartaDefinitions.EventType]func(*Session, cartaDefinitions.EventType, uint32, []byte) error{
	cartaDefinitions.EventType_REGISTER_VIEWER: (*Session).handleRegisterViewerMessage,
	cartaDefinitions.EventType_OPEN_FILE:       (*Session).handleOpenFile,
	// TODO: We need to handle CLOSE_FILE separately as well, because it will require shutting down a worker
	cartaDefinitions.EventType_EMPTY_EVENT: (*Session).handleStatusMessage,
}

func NewSession(conn *websocket.Conn, workerAddr string, callbackBaseURL string, user *auth.User) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		WebSocket:       conn,
		SpawnerAddress:  workerAddr,
		CallbackBaseURL: callbackBaseURL,
		User:            user,
		Context:         ctx,
		Cancel:          cancel,
	}
}

func (s *Session) checkAndParse(msg proto.Message, requestId uint32, rawMsg []byte) error {
	// Register viewer messages are allowed without a worker connection
	if s.sharedWorker == nil {
		switch msg.(type) {
		case *cartaDefinitions.RegisterViewer:
			break
		default:
			return fmt.Errorf("missing worker connection")
		}
	}

	if requestId == 0 {
		return fmt.Errorf("invalid or missing request id")
	}

	err := proto.Unmarshal(rawMsg, msg)

	if err != nil {
		return err
	}

	return nil
}

func (s *Session) HandleConnection() {
	s.clientSendChan = make(chan outboundMessage, 100)
	go clientSendHandler(s.clientSendChan, s.WebSocket, "client")
}

func (s *Session) HandleMessage(msg []byte) error {
	// Message prefix is used for determining message type and matching requests to responses
	prefix, err := cartaHelpers.DecodeMessagePrefix(msg)
	if err != nil {
		return fmt.Errorf("failed to unmarshal message: %v", err)
	}

	handler, ok := handlerMap[prefix.EventType]
	if !ok {
		// Any messages that don't have a specific handler are simply proxied to the worker
		err = s.handleProxiedMessage(prefix.EventType, prefix.RequestId, msg[8:])
	} else {
		err = handler(s, prefix.EventType, prefix.RequestId, msg[8:])
	}

	if err != nil {
		return fmt.Errorf("error handling message: %v", err)
	}
	return nil
}

func (s *Session) HandleDisconnect() {
	// Close the client channel to signal the sender goroutine to stop
	if s.clientSendChan != nil {
		close(s.clientSendChan)
	}
	if s.SessionID != "" {
		UnregisterLiveSession(s.SessionID)
	}

	if s.CartaListInfo.ListId != "" {
		err := spawnerHelpers.RequestCartaListShutdown(s.CartaListInfo.ListId, s.SpawnerAddress)
		if err != nil {
			slog.Error("Error shutting down carta-list", "error", err)
		} else {
			slog.Info("Shut down carta-list", "listId", s.CartaListInfo.ListId)
		}
	}

	// Tear down the shared listing worker (the carta_backend behind carta-list)
	// and any remote site connections. Runs regardless of per-file workers.
	s.mu.Lock()
	shared := s.sharedWorker
	s.sharedWorker = nil
	sites := s.siteWorkers
	s.siteWorkers = nil
	s.mu.Unlock()
	if shared != nil {
		shared.disconnect()
	}
	for id, w := range sites {
		if w != nil {
			slog.Info("Disconnecting remote site", "siteId", id)
			w.disconnect()
		}
	}

	if s.Info.WorkerId == "" {
		return
	}

	err := spawnerHelpers.RequestWorkerShutdown(s.Info.WorkerId, s.SpawnerAddress)
	if err != nil {
		slog.Error("Error shutting down worker", "error", err)
	}
	slog.Info("Shut down worker", "workerId", s.Info.WorkerId)

}
