package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/CARTAvis/go-carta/pkg/cartaDefinitions"
)

// controlMessage is a small JSON envelope the frontend sends over the single
// client websocket (as a text frame) to manage multi-site routing, without
// touching the binary CARTA protocol. The home carta-ctl proxies the session
// to remote sites so the browser only ever holds one socket.
type controlMessage struct {
	Control string `json:"control"` // "site.connect" | "site.select"
	ID      string `json:"id"`      // site id ("home" for the local site)
	Address string `json:"address"` // remote carta-ctl ws/http address (site.connect)
}

// HandleControl parses and dispatches a text control message from the client.
func (s *Session) HandleControl(raw []byte) error {
	var msg controlMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return fmt.Errorf("invalid control message: %w", err)
	}

	switch msg.Control {
	case "site.connect":
		if msg.ID == "" || msg.ID == "home" {
			// "home" is always the local shared worker; just select it.
			s.selectSite("home")
			return nil
		}
		if err := s.connectRemoteSite(msg.ID, msg.Address); err != nil {
			return fmt.Errorf("connect site %q: %w", msg.ID, err)
		}
		return nil
	case "site.select":
		s.selectSite(msg.ID)
		return nil
	default:
		return fmt.Errorf("unknown control %q", msg.Control)
	}
}

func (s *Session) selectSite(id string) {
	s.mu.Lock()
	s.selectedSite = id
	s.mu.Unlock()
	slog.Info("Selected site for proxied traffic", "siteId", id, "sessionId", s.SessionID)
}

// toWebSocketURL normalises a site address into a websocket URL pointing at the
// remote carta-ctl client endpoint.
func toWebSocketURL(address string) string {
	addr := address
	switch {
	case strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://"):
		// already a ws url
	case strings.HasPrefix(addr, "https://"):
		addr = "wss://" + strings.TrimPrefix(addr, "https://")
	case strings.HasPrefix(addr, "http://"):
		addr = "ws://" + strings.TrimPrefix(addr, "http://")
	default:
		addr = "ws://" + addr
	}
	return addr
}

// connectRemoteSite dials a remote site's CARTA websocket endpoint, registers a
// viewer so it is ready to serve listings, stores it as a per-session site
// worker, and makes it the selected site. Responses from the remote site are
// forwarded to the client; the remote REGISTER_VIEWER_ACK is swallowed (the
// client already registered with the home controller).
func (s *Session) connectRemoteSite(id, address string) error {
	if address == "" {
		return fmt.Errorf("missing address for site %q", id)
	}
	addr := toWebSocketURL(address)

	conn, _, err := websocket.DefaultDialer.DialContext(s.Context, addr, nil)
	if err != nil {
		return fmt.Errorf("dial remote site at %s: %w", addr, err)
	}

	worker := &SessionWorker{
		conn:           conn,
		clientSendChan: s.clientSendChan,
	}
	worker.handleInit()

	reg := &cartaDefinitions.RegisterViewer{SessionId: 0, ClientFeatureFlags: 0}
	if err := worker.proxyMessageToWorker(reg, cartaDefinitions.EventType_REGISTER_VIEWER, 1); err != nil {
		worker.disconnect()
		return fmt.Errorf("register viewer with remote site %q: %w", id, err)
	}

	s.mu.Lock()
	if s.siteWorkers == nil {
		s.siteWorkers = make(map[string]*SessionWorker)
	}
	if old := s.siteWorkers[id]; old != nil {
		old.disconnect()
	}
	s.siteWorkers[id] = worker
	s.selectedSite = id
	s.mu.Unlock()

	slog.Info("Connected remote site", "siteId", id, "address", addr, "sessionId", s.SessionID)
	return nil
}
