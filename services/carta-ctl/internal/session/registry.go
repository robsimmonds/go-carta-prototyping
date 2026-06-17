package session

import (
	"log/slog"
	"sync"
)

// liveSessions tracks active client Sessions keyed by their session ID. It lets
// out-of-band callbacks — in particular carta-list agents that register back
// with carta-ctl — be routed to the corresponding live Session.
var liveSessions sync.Map // map[string]*Session

// RegisterLiveSession records a live Session under its session ID.
func RegisterLiveSession(sessionID string, s *Session) {
	if sessionID == "" || s == nil {
		return
	}
	liveSessions.Store(sessionID, s)
}

// UnregisterLiveSession removes a Session from the live registry.
func UnregisterLiveSession(sessionID string) {
	if sessionID == "" {
		return
	}
	liveSessions.Delete(sessionID)
}

// GetLiveSession returns the live Session for the given session ID, if present.
func GetLiveSession(sessionID string) (*Session, bool) {
	v, ok := liveSessions.Load(sessionID)
	if !ok {
		return nil, false
	}
	s, ok := v.(*Session)
	return s, ok
}

// ApplyCartaListRegistration links a carta-list agent's registration callback to
// the live Session identified by sessionID. It is invoked from the carta-ctl HTTP
// handler when a carta-list agent registers back with the controller. When the
// registration reports a backendAddress (the carta_backend the agent spawned for
// listing), the session connects to it as its shared listing worker.
func ApplyCartaListRegistration(sessionID, siteID, username, token, backendAddress string, pid int) {
	s, ok := GetLiveSession(sessionID)
	if !ok {
		slog.Warn("carta-list registration for unknown session",
			"sessionId", sessionID, "siteId", siteID, "username", username)
		return
	}
	slog.Info("Linked carta-list registration to live session",
		"sessionId", sessionID, "siteId", siteID, "username", username, "pid", pid, "backendAddress", backendAddress)

	if backendAddress == "" {
		slog.Warn("carta-list registration has no backend address; file listing will not work for this session",
			"sessionId", sessionID)
		return
	}
	if err := s.connectSharedWorker(backendAddress); err != nil {
		slog.Error("Failed to connect shared listing worker", "error", err,
			"sessionId", sessionID, "backendAddress", backendAddress)
		return
	}
	slog.Info("Connected shared listing worker", "sessionId", sessionID, "backendAddress", backendAddress)
}
