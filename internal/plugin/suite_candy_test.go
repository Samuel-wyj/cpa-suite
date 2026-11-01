package plugin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSuiteCandyTest(t *testing.T) {
	app, _, _, state := watchOnlyApp(t, "modules:\n  candy_test: on\n")
	db, err := sql.Open("sqlite3", "file:"+state+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sent map[string]any
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case hostAuthList:
			return json.RawMessage(`{"files":[{"id":"codex-a.json","auth_index":"cx-1","name":"codex-a.json","type":"codex","provider":"codex"},{"id":"g.json","auth_index":"gm-1","name":"g.json","type":"gemini"}]}`), nil
		case hostModelExecute:
			sent = payload.(map[string]any)
			// The host publishes the nested request's usage (no API key) while it runs.
			usage := map[string]any{"Provider": "codex", "Model": "gpt-5", "ResponseModel": "gpt-5-2025-08-07", "AuthIndex": "cx-1", "APIKey": "",
				"Generate": true, "RequestedAt": time.Now().UTC(), "Latency": int64(2500 * time.Millisecond),
				"Detail": map[string]any{"InputTokens": 14100, "OutputTokens": 900, "ReasoningTokens": 777}}
			if _, err := app.HandleMethod(MethodUsageHandle, mustMarshal(t, usage)); err != nil {
				t.Error(err)
			}
			body := `{"model":"gpt-5-2025-08-07","output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"最少需要取出 21 个糖果。"}]}],"usage":{"input_tokens":14000,"output_tokens":800,"output_tokens_details":{"reasoning_tokens":700}}}`
			return mustJSONRaw(t, map[string]any{"StatusCode": 200, "Body": []byte(body)}), nil
		}
		return json.RawMessage(`{}`), nil
	})
	callOK(t, app, http.MethodPost, routeSuiteCandyRun, nil, map[string]any{"auth_index": "cx-1"}, http.StatusBadRequest, nil) // unconfirmed
	callOK(t, app, http.MethodPost, routeSuiteCandyRun, nil, map[string]any{"auth_index": "gm-1", "confirm": true}, http.StatusNotFound, nil)
	callOK(t, app, http.MethodPost, routeSuiteCandyRun, nil, map[string]any{"auth_index": "cx-1", "confirm": true, "effort": "high"}, http.StatusAccepted, nil)
	if st := callManagement(t, app, http.MethodPost, routeSuiteCandyRun, nil, map[string]any{"auth_index": "cx-1", "confirm": true}).StatusCode; st != http.StatusConflict && st != http.StatusTooManyRequests {
		t.Fatalf("second run status %d", st)
	}
	type acct struct {
		AuthIndex   string     `json:"auth_index"`
		Running     bool       `json:"running"`
		NextAllowed *time.Time `json:"next_allowed_at"`
		Runs        []struct {
			Status, Model string
			ResponseModel string `json:"response_model"`
			Passed        bool
			Reasoning     int64 `json:"reasoning_tokens"`
			Input         int64 `json:"input_tokens"`
			Latency       int64 `json:"latency_ms"`
		} `json:"runs"`
	}
	var got struct {
		Mode     string `json:"mode"`
		Accounts []acct `json:"accounts"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		callOK(t, app, http.MethodGet, routeSuiteCandy, nil, nil, http.StatusOK, &got)
		if len(got.Accounts) == 1 && len(got.Accounts[0].Runs) == 1 && got.Accounts[0].Runs[0].Status != "running" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].AuthIndex != "cx-1" {
		t.Fatalf("only the Codex account is eligible: %+v", got)
	}
	r := got.Accounts[0].Runs
	if len(r) != 1 || r[0].Status != "done" || !r[0].Passed || r[0].Reasoning != 777 || r[0].Input != 14100 || r[0].ResponseModel != "gpt-5-2025-08-07" || r[0].Latency != 2500 || r[0].Model != "gpt-5.5" {
		t.Fatalf("run %+v", r)
	}
	if got.Accounts[0].NextAllowed == nil || got.Accounts[0].Running {
		t.Fatalf("rate limit %+v", got.Accounts[0])
	}
	callOK(t, app, http.MethodPost, routeSuiteCandyRun, nil, map[string]any{"auth_index": "cx-1", "confirm": true}, http.StatusTooManyRequests, nil)
	// The request went to that exact credential with the Codex CLI context.
	body := string(sent["body"].([]byte))
	if sent["auth_id"] != "codex-a.json" || sent["forced_provider"] != "codex" || sent["stream"] != false || !strings.Contains(body, "五角星形") || len(body) < 40000 || !strings.Contains(body, `"effort":"high"`) {
		t.Fatalf("request %v len=%d", sent["auth_id"], len(body))
	}
	// Not billed: upstream billing wrote no request event for the nested request.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("request_events = %d %v (candy usage must not be billed)", n, err)
	}
	// …and the passive monitor did not observe it.
	if s, _ := app.suite.MonitorSamples(time.Time{}); len(s) != 0 {
		t.Fatalf("monitor observed the candy test: %+v", s)
	}
	if !candyStandalone21("答案：21。") || candyStandalone21("121 个") || candyStandalone21("210") {
		t.Fatal("standalone 21")
	}
}

func TestSuiteCandyOffByDefault(t *testing.T) {
	app, _, _, _ := watchOnlyApp(t, "")
	var got struct {
		Mode string `json:"mode"`
	}
	callOK(t, app, http.MethodGet, routeSuiteCandy, nil, nil, http.StatusOK, &got)
	if got.Mode != "off" {
		t.Fatalf("mode %q", got.Mode)
	}
	callOK(t, app, http.MethodPost, routeSuiteCandyRun, nil, map[string]any{"auth_index": "x", "confirm": true}, http.StatusConflict, nil)
}
