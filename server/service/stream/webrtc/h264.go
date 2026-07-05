package webrtc

import (
	"NanoKVM-Server/config"
	"NanoKVM-Server/service/iceservers"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/pion/dtls/v3"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

var (
	upgrader = websocket.Upgrader{
		WriteBufferSize: 256 * 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	globalManager *WebRTCManager
	managerOnce   sync.Once
)

func getManager() *WebRTCManager {
	managerOnce.Do(func() {
		globalManager = NewWebRTCManager()
	})
	return globalManager
}

func Connect(c *gin.Context) {
	// create WebSocket connection
	wsConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Errorf("failed to create h264 websocket: %s", err)
		return
	}
	defer func() {
		_ = wsConn.Close()
		log.Debugf("h264 websocket disconnected: %s", c.ClientIP())
	}()
	log.Debugf("h264 websocket connected: %s", c.ClientIP())

	var zeroTime time.Time
	_ = wsConn.SetReadDeadline(zeroTime)

	// create video connection
	iceServers := createICEServers()

	mediaEngine, err := createMediaEngine()
	if err != nil {
		log.Errorf("failed to create h264 media engine: %s", err)
		return
	}

	videoConn, err := createPeerConnection(iceServers, mediaEngine)
	if err != nil {
		log.Errorf("failed to create h264 video peer connection: %s", err)
		return
	}
	defer func() {
		_ = videoConn.Close()
		log.Debugf("h264 video peer disconnected: %s", c.ClientIP())
	}()

	// create client
	client := NewClient(wsConn, videoConn)
	if err := client.AddTrack(); err != nil {
		log.Errorf("failed to add track: %s", err)
		return
	}

	manager := getManager()
	manager.AddClient(wsConn, client)
	defer manager.RemoveClient(wsConn)

	// handle signaling
	signalingHandler := NewSignalingHandler(client)
	signalingHandler.RegisterCallbacks()
	if err := sendICEServers(client, iceServers); err != nil {
		log.Errorf("failed to send ICE servers: %s", err)
		return
	}

	// read and wait
	for {
		message, err := client.ReadMessage()
		if err != nil {
			return
		}
		if message != nil {
			if err := signalingHandler.HandleMessage(message); err != nil {
				log.Errorf("failed to handle signaling message: %s", err)
			}
		}
	}
}

func createICEServers() []webrtc.ICEServer {
	var iceServers []webrtc.ICEServer

	// Venue servers FIRST: the deduplicated STUN/TURN union of every mesh this
	// KVM is on (fleet first), published by the mesh bridge. A remote viewer
	// reaching this web UI through AllMyStuff's sites proxy shares a mesh with
	// us, so these are the relays it can actually reach — offer them ahead of
	// the locally-configured (often LAN-only) STUN/TURN.
	for _, s := range iceservers.Get() {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}

	conf := config.GetInstance()

	if conf.Stun != "" && conf.Stun != "disable" {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs: []string{"stun:" + conf.Stun},
		})
	}

	if conf.Turn.TurnAddr != "" && conf.Turn.TurnUser != "" && conf.Turn.TurnCred != "" {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       []string{"turn:" + conf.Turn.TurnAddr},
			Username:   conf.Turn.TurnUser,
			Credential: conf.Turn.TurnCred,
		})
	}

	return dedupICEServers(iceServers)
}

// dedupICEServers collapses entries that share the same URL set, keeping the
// first occurrence — so a venue TURN and a locally-configured TURN at the same
// URL don't double up (the venue entry, prepended first, wins its credentials).
func dedupICEServers(servers []webrtc.ICEServer) []webrtc.ICEServer {
	seen := make(map[string]bool, len(servers))
	out := make([]webrtc.ICEServer, 0, len(servers))
	for _, s := range servers {
		urls := make([]string, len(s.URLs))
		copy(urls, s.URLs)
		sort.Strings(urls)
		key := strings.Join(urls, ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

type clientICEServer struct {
	URLs       []string    `json:"urls"`
	Username   string      `json:"username,omitempty"`
	Credential interface{} `json:"credential,omitempty"`
}

func sendICEServers(client *Client, iceServers []webrtc.ICEServer) error {
	clientServers := make([]clientICEServer, 0, len(iceServers))
	for _, server := range iceServers {
		clientServers = append(clientServers, clientICEServer{
			URLs:       server.URLs,
			Username:   server.Username,
			Credential: server.Credential,
		})
	}

	data, err := json.Marshal(clientServers)
	if err != nil {
		return err
	}

	return client.WriteMessage("ice-servers", string(data))
}

func createMediaEngine() (*webrtc.MediaEngine, error) {
	mediaEngine := &webrtc.MediaEngine{}

	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		log.Errorf("failed to register default codecs: %s", err)
		return nil, err
	}

	if err := mediaEngine.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"},
		webrtc.RTPCodecTypeVideo,
	); err != nil {
		log.Errorf("failed to register header extension: %s", err)
		return nil, err
	}

	return mediaEngine, nil
}

func createPeerConnection(iceServers []webrtc.ICEServer, mediaEngine *webrtc.MediaEngine) (*webrtc.PeerConnection, error) {
	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetSRTPProtectionProfiles(
		dtls.SRTP_AEAD_AES_128_GCM,
		dtls.SRTP_AES128_CM_HMAC_SHA1_80,
	)

	apiOptions := []func(api *webrtc.API){
		webrtc.WithSettingEngine(settingEngine),
	}
	if mediaEngine != nil {
		apiOptions = append(apiOptions, webrtc.WithMediaEngine(mediaEngine))
	}

	api := webrtc.NewAPI(apiOptions...)

	return api.NewPeerConnection(webrtc.Configuration{
		ICEServers:   iceServers,
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	})
}
