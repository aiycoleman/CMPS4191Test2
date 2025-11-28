package ws

// Filename: internal/ws/handler.go

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Heartbeat and timeout settings
const (
	writeWait  = 5 * time.Second     // max time to complete a write
	pongWait   = 30 * time.Second    // if we don't get a pong in 30s, time out
	pingPeriod = (pongWait * 9) / 10 // send pings at ~90% of pongWait (e.g., 27s)
)

var messageCounter uint64

// Only allow pages served from this origin to connect
var allowedOrigins = []string{
	"http://localhost:4000",
}

func originAllowed(o string) bool {
	if o == "" {
		return false
	}
	for _, a := range allowedOrigins {
		if strings.EqualFold(o, a) {
			return true
		}
	}
	return false
}

// The upgrader object is used when we need to upgrade from HTTP to RFC 6455
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		ok := originAllowed(origin)
		if !ok {
			log.Printf("blocked cross-origin websocket: Origin=%q Path=%s", origin, r.URL.Path)
		}
		return ok
	},
	Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
	},
}

type CommandRequest struct {
	Command string  `json:"command"`
	A       float64 `json:"a"`
	B       float64 `json:"b"`
}

type CommandResponse struct {
	Result  float64 `json:"result,omitempty"`
	Command string  `json:"command"`
	Error   string  `json:"error,omitempty"`
}

// Attempt to upgrade from HTTP to RFC 6455
func HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Has to be an HTTP GET request
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Upgrade the connection from HTTP to RFC 6455
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("connection opened from %s", r.RemoteAddr)

	// Limit message size
	conn.SetReadLimit(1024 * 4)

	// PING / PONG SETUP

	// Idle timeout window starts now: must receive a pong within pongWait
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))

	// On each pong, extend the read deadline again
	conn.SetPongHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		log.Printf("pong from %s (data=%q)", r.RemoteAddr, appData)
		return nil
	})

	// Start a goroutine that sends pings every pingPeriod
	done := make(chan struct{})
	ticker := time.NewTicker(pingPeriod)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Send a ping; if this fails, the read loop will notice soon
				_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
					log.Printf("ping write error: %v", err)
					return
				}
				log.Printf("ping → %s", r.RemoteAddr)
			case <-done:
				return
			}
		}
	}()

	// Read/Echo loop
	for {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			// This error will be:
			//  - a timeout (no pong in time), or
			//  - a normal close, or
			//  - some other read error
			log.Printf("read error (timeout/close): %v", err)

			// Try to send a graceful close so the client can see 1000 instead of 1006
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "idle timeout"),
				time.Now().Add(writeWait),
			)

			break
		}

		// We successfully read a message; normal traffic also keeps the connection alive.
		// Note: the pong handler also updates the read deadline on pongs.

		// Echo back text messages
		if msgType == websocket.TextMessage {

			isJSON := len(payload) > 0 && payload[0] == '{'

			if isJSON {
				response, err := processCommand(payload) // Process JSON command

				// If processing failed
				if err != nil {
					log.Printf("json command error: %v", err)
					// Send back an error JSON response
					response, _ = json.Marshal(CommandResponse{
						Command: "error",
						Error:   fmt.Sprintf("processing error: %v", err.Error()),
					})
				}

				_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := conn.WriteMessage(websocket.TextMessage, response); err != nil {
					log.Printf("write error: %v", err)
					break
				}
				continue
			}

			var echoPayload []byte = payload

			// Part 1: Uppercase Echo
			if strings.HasPrefix(string(payload), "UPPER:") {
				text := strings.TrimPrefix(string(payload), "UPPER:")
				echoPayload = []byte(strings.ToUpper(text))
			}

			// Part 2: Reverse Echo (Use else if to prevent both from running)
			if strings.HasPrefix(string(payload), "REVERSE:") {
				text := strings.TrimPrefix(string(payload), "REVERSE:")
				echoPayload = []byte(reverseString(text))
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				log.Printf("write error: %v", err)
				break
			}

			// Part 3: BROADCAST COUNTER
			// Increment the counter atomically
			count := atomic.AddUint64(&messageCounter, 1)

			// Format the response with the counter
			newPayload := fmt.Sprintf("[Msg #%d] %s", count, echoPayload)
			echoPayload = []byte(newPayload) // Use the new, formatted payload
			// ---------------------------------

			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.TextMessage, echoPayload); err != nil {
				log.Printf("write error: %v", err)
				break
			}
		}
	}

	// Stop the ping goroutine
	close(done)

	log.Printf("connection closed from %s", r.RemoteAddr)
}

// TODO 2: Add a function to reverse a string
func reverseString(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < len(runes)/2; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

func processCommand(payload []byte) ([]byte, error) {
	var req CommandRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}

	res := CommandResponse{
		Command: req.Command,
	}

	switch req.Command {
	case "add":
		res.Result = req.A + req.B
	case "subtract":
		res.Result = req.A - req.B
	case "multiply":
		res.Result = req.A * req.B
	case "divide":
		if req.B == 0 {
			res.Error = "cannot divide by zero"
			res.Result = 0
		} else {
			res.Result = req.A / req.B
		}
	default:
		res.Error = fmt.Sprintf("unknown command: %s", req.Command)
		res.Result = 0
	}

	responseBytes, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response: %w", err)
	}
	return responseBytes, nil
}
