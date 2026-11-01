// wsclient: drives CPA's /v1/responses websocket like Codex (non-duplex).
// usage: wsclient URL KEY MODE   MODE = turns | dropmid | idle
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	url, key, mode := os.Args[1], os.Args[2], os.Args[3]
	h := http.Header{"Authorization": []string{"Bearer " + key}}
	conn, resp, err := websocket.DefaultDialer.Dial(url, h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		fmt.Println("dial error", code, err)
		os.Exit(1)
	}
	defer conn.Close()
	send := func(input string) {
		msg, _ := json.Marshal(map[string]any{"type": "response.create", "model": "gpt-5.5", "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": input}}}}})
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			fmt.Println("write", err)
			os.Exit(1)
		}
	}
	readTurn := func() string {
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return "read error: " + err.Error()
			}
			var ev struct{ Type string }
			json.Unmarshal(data, &ev)
			if ev.Type == "response.completed" || ev.Type == "error" || ev.Type == "response.failed" {
				if ev.Type != "response.completed" {
					return ev.Type + " " + strings.TrimSpace(string(data))
				}
				return ev.Type
			}
		}
	}
	switch mode {
	case "turns":
		for i := 0; i < 5; i++ {
			send("hi")
			fmt.Println("turn", i, readTurn())
		}
	case "idle":
		send("hi")
		fmt.Println("turn", readTurn())
		time.Sleep(1500 * time.Millisecond) // socket stays open, idle
		fmt.Println("idle done")
	case "dropmid":
		send("E2E HOLD CONCURRENCY SLOT")
		time.Sleep(400 * time.Millisecond)
		conn.UnderlyingConn().Close() // abrupt client disconnect mid-turn
		fmt.Println("dropped")
	}
}
