package dashboard

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/config"
)

const (
	jamLiveSnapshot    = "snapshot"
	jamLivePresence    = "presence"
	jamLiveFocus       = "focus"
	jamLiveEdit        = "edit"
	jamLiveEditApplied = "edit_applied"
	jamLiveConflict    = "conflict"
	jamLiveError       = "error"
)

type jamLiveMessage struct {
	Type           string                   `json:"type"`
	ClientID       string                   `json:"client_id,omitempty"`
	Section        string                   `json:"section,omitempty"`
	Content        string                   `json:"content,omitempty"`
	BaseRevisionID string                   `json:"base_revision_id,omitempty"`
	Reason         string                   `json:"reason,omitempty"`
	RevisionID     string                   `json:"revision_id,omitempty"`
	Expected       string                   `json:"expected_revision_id,omitempty"`
	Presence       []CampaignJamParticipant `json:"presence,omitempty"`
	Actor          *CampaignJamActor        `json:"actor,omitempty"`
	Jam            *CampaignJamState        `json:"jam,omitempty"`
	Error          string                   `json:"error,omitempty"`
}

type CampaignJamParticipant struct {
	ClientID  string           `json:"client_id"`
	Actor     CampaignJamActor `json:"actor"`
	Section   string           `json:"section,omitempty"`
	Connected string           `json:"connected_at"`
	Seen      string           `json:"seen_at"`
}

type jamLiveClient struct {
	id       string
	campaign string
	role     string
	section  string
	actor    CampaignJamActor
	conn     *websocket.Conn
	send     chan jamLiveMessage
	hub      *jamLiveHub
}

type jamLiveHub struct {
	mu      sync.Mutex
	clients map[string]*jamLiveClient
}

var (
	jamLiveHubsMu   sync.Mutex
	jamLiveHubs     = map[string]*jamLiveHub{}
	// SECURITY: same-origin CheckOrigin (shared with contribute_ws). The jam
	// socket authenticates via the dashboard session cookie, which browsers
	// also attach cross-site; allowing every Origin here let any web page a
	// logged-in operator visited read the full jam state and push live spec
	// edits with the victim's role (cross-site WebSocket hijacking).
	jamLiveUpgrader = websocket.Upgrader{CheckOrigin: wsSameOrigin}
)

func (s *Server) handleCampaignJamWebSocket(w http.ResponseWriter, r *http.Request) {
	if !config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleRead) {
		jsonError(w, "read access required", http.StatusForbidden)
		return
	}
	campaignID := campaignIDFromRequest(r)
	if campaignID == "" {
		jsonError(w, "campaign id required", http.StatusBadRequest)
		return
	}
	conn, err := jamLiveUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	actor := jamActorFromRequest(r, r.URL.Query().Get("agent"), r.URL.Query().Get("model"))
	client := &jamLiveClient{
		id:       jamID("client"),
		campaign: campaignID,
		role:     r.Header.Get("X-Hive-Role"),
		actor:    actor,
		conn:     conn,
		send:     make(chan jamLiveMessage, 16),
		hub:      jamHubForCampaign(campaignID),
	}
	client.hub.add(client)
	state, err := s.loadCampaignJam(campaignID)
	if err != nil {
		client.enqueue(jamLiveMessage{Type: jamLiveError, Error: err.Error()})
	} else {
		client.enqueue(jamLiveMessage{Type: jamLiveSnapshot, ClientID: client.id, Presence: client.hub.presence(), Jam: state})
	}
	client.hub.broadcast(jamLiveMessage{Type: jamLivePresence, Presence: client.hub.presence()})

	done := make(chan struct{})
	go client.writeLoop(done)
	client.readLoop(s)
	close(done)
	client.hub.remove(client.id)
	client.hub.broadcast(jamLiveMessage{Type: jamLivePresence, Presence: client.hub.presence()})
}

func jamHubForCampaign(campaign string) *jamLiveHub {
	jamLiveHubsMu.Lock()
	defer jamLiveHubsMu.Unlock()
	hub := jamLiveHubs[campaign]
	if hub == nil {
		hub = &jamLiveHub{clients: map[string]*jamLiveClient{}}
		jamLiveHubs[campaign] = hub
	}
	return hub
}

func (h *jamLiveHub) add(c *jamLiveClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c.id] = c
}

func (h *jamLiveHub) remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, id)
}

func (h *jamLiveHub) broadcast(msg jamLiveMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, client := range h.clients {
		client.enqueueLocked(msg)
	}
}

func (h *jamLiveHub) presence() []CampaignJamParticipant {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.presenceLocked()
}

func (h *jamLiveHub) presenceLocked() []CampaignJamParticipant {
	out := make([]CampaignJamParticipant, 0, len(h.clients))
	for _, client := range h.clients {
		out = append(out, CampaignJamParticipant{ClientID: client.id, Actor: client.actor, Section: client.section, Connected: client.connectedAt(), Seen: jamNow()})
	}
	return out
}

func (c *jamLiveClient) connectedAt() string {
	return c.id[len("client-") : len("client-")+23]
}

func (c *jamLiveClient) enqueue(msg jamLiveMessage) {
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	c.enqueueLocked(msg)
}

func (c *jamLiveClient) enqueueLocked(msg jamLiveMessage) {
	select {
	case c.send <- msg:
	default:
	}
}

func (c *jamLiveClient) writeLoop(done <-chan struct{}) {
	for {
		select {
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.conn.WriteJSON(msg); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

func (c *jamLiveClient) readLoop(s *Server) {
	defer c.conn.Close()
	for {
		var msg jamLiveMessage
		if err := c.conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Type {
		case jamLiveFocus:
			c.setFocus(msg.Section)
		case jamLiveEdit:
			c.applyEdit(s, msg)
		default:
			c.enqueue(jamLiveMessage{Type: jamLiveError, Error: "unsupported jam live message"})
		}
	}
}

func (c *jamLiveClient) setFocus(section string) {
	c.hub.mu.Lock()
	if live := c.hub.clients[c.id]; live != nil {
		live.actor = c.actor
		live.section = section
	}
	presence := make([]CampaignJamParticipant, 0, len(c.hub.clients))
	for _, client := range c.hub.clients {
		presence = append(presence, CampaignJamParticipant{ClientID: client.id, Actor: client.actor, Section: client.section, Connected: client.connectedAt(), Seen: jamNow()})
	}
	for _, client := range c.hub.clients {
		client.enqueueLocked(jamLiveMessage{Type: jamLivePresence, Presence: presence})
	}
	c.hub.mu.Unlock()
}

func (c *jamLiveClient) applyEdit(s *Server, msg jamLiveMessage) {
	if !config.RoleAtLeast(c.role, config.RoleReadWrite) {
		c.enqueue(jamLiveMessage{Type: jamLiveError, Error: "read-write access required"})
		return
	}
	var revisionID string
	state, err := s.mutateCampaignJam(c.campaign, func(state *CampaignJamState) error {
		if msg.BaseRevisionID != state.SpecRevisionID {
			return errJamLiveConflict
		}
		rev := recordJamRevision(state, msg.Content, firstRunNonEmpty(msg.Reason, "live co-edit"), c.actor, nil)
		revisionID = rev.ID
		return nil
	})
	if errors.Is(err, errJamLiveConflict) {
		current, _ := s.loadCampaignJam(c.campaign)
		expected := ""
		if current != nil {
			expected = current.SpecRevisionID
		}
		c.enqueue(jamLiveMessage{Type: jamLiveConflict, Expected: expected, Jam: current})
		return
	}
	if err != nil {
		c.enqueue(jamLiveMessage{Type: jamLiveError, Error: err.Error()})
		return
	}
	c.hub.broadcast(jamLiveMessage{Type: jamLiveEditApplied, RevisionID: revisionID, Jam: state, Actor: &c.actor})
}

var errJamLiveConflict = errors.New("jam live edit conflict")
