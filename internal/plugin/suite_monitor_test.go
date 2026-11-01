package plugin

import (
	"net/http"
	"testing"
	"time"
)

func TestSuiteMonitorObserveAndAlerts(t *testing.T) {
	app, _, _, _ := watchOnlyApp(t, "")
	at := time.Now().UTC().Add(-time.Minute)
	usage := func(m map[string]any) {
		t.Helper()
		base := map[string]any{"Provider": "xai", "Model": "grok-4", "ResponseModel": "grok-4", "AuthIndex": "acc1", "Generate": true, "APIKey": "sk-client",
			"Stream": true, "RequestedAt": at, "Detail": map[string]any{"OutputTokens": 10, "ReasoningTokens": 20}}
		for k, v := range m {
			base[k] = v
		}
		if _, err := app.HandleMethod(MethodUsageHandle, mustMarshal(t, base)); err != nil {
			t.Fatal(err)
		}
	}
	usage(nil)
	usage(map[string]any{"Provider": "codex", "Model": "gpt-5", "Failed": true, "Failure": map[string]any{"StatusCode": 408, "Body": "stream disconnected before response.completed"}})
	usage(map[string]any{"APIKey": ""})         // plugin/host-internal requests (no client key) excluded
	usage(map[string]any{"Generate": false})    // non-generation excluded
	usage(map[string]any{"AuthIndex": ""})      // unknown account excluded
	usage(map[string]any{"Provider": "gemini"}) // not monitored
	samples, err := app.suite.MonitorSamples(time.Time{})
	if err != nil || len(samples) != 2 {
		t.Fatalf("samples = %+v %v", samples, err)
	}
	for _, s := range samples {
		if s.Provider == "codex" && (s.ErrKind != "truncated" || s.Status != 408) || s.Provider == "xai" && (!s.Stream || s.Reasoning != 20) {
			t.Fatalf("sample %+v", s)
		}
	}
	var mon struct {
		Mode     string `json:"mode"`
		Accounts []struct {
			AuthIndex string `json:"auth_index"`
			Verdict   string `json:"verdict"`
		} `json:"accounts"`
	}
	callOK(t, app, http.MethodGet, routeSuiteMonitor, nil, nil, http.StatusOK, &mon)
	if len(mon.Accounts) != 1 || mon.Accounts[0].AuthIndex != "acc1" || mon.Accounts[0].Verdict != "insufficient" {
		t.Fatalf("monitor = %+v", mon)
	}
	var alerts struct {
		Mode   string           `json:"mode"`
		Alerts []map[string]any `json:"alerts"`
		Badge  int              `json:"badge"`
	}
	callOK(t, app, http.MethodGet, routeSuiteAlerts, nil, nil, http.StatusOK, &alerts)
	if alerts.Mode != "on" || alerts.Badge != 0 {
		t.Fatalf("alerts = %+v", alerts)
	}
	callOK(t, app, http.MethodPost, routeSuiteAlertsMute, nil, map[string]any{"source": "monitor", "object": "acc1", "minutes": 60}, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routeSuiteAlertsMute, nil, map[string]any{"source": "nope", "object": "acc1"}, http.StatusBadRequest, nil)
	callOK(t, app, http.MethodPost, routeSuiteAlertsAck, nil, map[string]any{"id": "x|y|z"}, http.StatusNotFound, nil)
	callOK(t, app, http.MethodPost, routeSuiteAlertsUnmute, nil, map[string]any{"source": "monitor", "object": "acc1"}, http.StatusOK, nil)
}
