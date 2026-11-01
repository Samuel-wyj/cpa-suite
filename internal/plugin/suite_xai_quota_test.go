package plugin

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// SuperGrok token: JWT with tier=1 (what upstream calls "Paid").
func superGrokToken() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"tier":1,"sub":"user-123","email":"owner@example.com","exp":1999999999}`))
	return "eyJhbGciOiJub25lIn0." + payload + ".sig-secret-part"
}

func xaiHost(t *testing.T, responses map[string]string, calls *[]string) HostCaller {
	return func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case hostAuthList:
			return json.RawMessage(`{"files":[{"auth_index":"xai-9","name":"owner@example.com.json","type":"xai","email":"owner@example.com"}]}`), nil
		case hostAuthGet:
			return mustJSONRaw(t, map[string]any{"auth_index": "xai-9", "json": map[string]any{"access_token": superGrokToken(), "user_id": "user-123"}}), nil
		case hostHTTPDo:
			r := payload.(hostHTTPRequest)
			*calls = append(*calls, r.Method+" "+r.URL)
			body, ok := responses[r.URL]
			if !ok {
				return mustJSONRaw(t, hostHTTPResponse{StatusCode: 404, Body: []byte(`{}`)}), nil
			}
			return mustJSONRaw(t, hostHTTPResponse{StatusCode: 200, Body: []byte(body)}), nil
		}
		t.Fatalf("unexpected host method %s", method)
		return nil, nil
	}
}

const (
	xaiCredits = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	xaiMonthly = "https://cli-chat-proxy.grok.com/v1/billing"
)

func TestSuiteSuperGrokPaidAccountIsQueried(t *testing.T) {
	var calls []string
	app := newConfiguredApp(t)
	app.SetHostCaller(xaiHost(t, map[string]string{
		xaiCredits: `{"config":{"creditUsagePercent":45,"subscriptionTier":"SuperGrok","currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-08T00:00:00Z","end":"2026-10-15T00:00:00Z"},"productUsage":[{"product":"Grok Build","usagePercent":45}]}}`,
		xaiMonthly: `{"config":{}}`,
	}, &calls))
	result, err := app.fetchAuthQuota("", hostAuthFile{AuthIndex: "xai-9", Type: "xai"}, "xai")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("billing must be queried for SuperGrok, calls=%v", calls)
	}
	if result.Plan != "SuperGrok" || len(result.Quota) != 2 || result.Quota[0].Label != "Weekly limit" || *result.Quota[0].RemainingPercent != 55 || result.Quota[0].ResetAt != "2026-10-15T00:00:00Z" {
		t.Fatalf("result = %+v", result)
	}
}

func TestSuiteSuperGrokBillingUnavailableKeepsPaid(t *testing.T) {
	var calls []string
	app := newConfiguredApp(t)
	app.SetHostCaller(xaiHost(t, map[string]string{}, &calls))
	result, err := app.fetchAuthQuota("", hostAuthFile{AuthIndex: "xai-9", Type: "xai"}, "xai")
	if err != nil || result.Plan != "Paid" || len(result.Quota) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSuiteXAIWeeklyResetOmitsPercent(t *testing.T) {
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, config string
		want         int
	}{
		{"fresh weekly reset = 0% used", `{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-11T00:49:20.291511+00:00","end":"2026-10-18T00:49:20.291511+00:00"},"onDemandUsed":{"val":0},"onDemandCap":{"val":0}}`, 1},
		{"positive on-demand cap stays unknown", `{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-11T00:00:00Z","end":"2026-10-18T00:00:00Z"},"onDemandCap":{"val":500}}`, 0},
		{"non-weekly period stays unknown", `{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","start":"2026-10-01T00:00:00Z","end":"2026-11-01T00:00:00Z"}}`, 0},
		{"malformed period stays unknown", `{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-10-18T00:00:00Z"}}`, 0},
	} {
		var config map[string]any
		_ = json.Unmarshal([]byte(tc.config), &config)
		result := authQuotaResponse{Quota: []quotaRow{}}
		suiteFixXAIQuota(&result, config, 0, now)
		if len(result.Quota) != tc.want {
			t.Fatalf("%s: %+v", tc.name, result.Quota)
		}
		if tc.want == 1 && (*result.Quota[0].RemainingPercent != 100 || !strings.HasPrefix(result.Quota[0].ResetAt, "2026-10-18")) {
			t.Fatalf("%s: %+v", tc.name, result.Quota[0])
		}
	}
}

func TestSuiteXAIStaleMonthlyDroppedWhenWeeklyExists(t *testing.T) {
	var calls []string
	app := newConfiguredApp(t)
	app.SetHostCaller(xaiHost(t, map[string]string{
		xaiCredits: `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-11T00:00:00Z","end":"2099-10-18T00:00:00Z"},"onDemandCap":{"val":0}}}`,
		xaiMonthly: `{"config":{"monthlyLimit":{"val":2272},"used":{"val":3406},"billingPeriodStart":"2026-09-01T00:00:00+00:00","billingPeriodEnd":"2026-10-01T00:00:00+00:00"}}`,
	}, &calls))
	result, err := app.fetchAuthQuota("", hostAuthFile{AuthIndex: "xai-9", Type: "xai"}, "xai")
	if err != nil || len(result.Quota) != 1 || result.Quota[0].Label != "Weekly limit" || *result.Quota[0].RemainingPercent != 100 {
		t.Fatalf("result=%+v err=%v", result.Quota, err)
	}
}

func TestSuiteXAITierLabel(t *testing.T) {
	for in, want := range map[string]string{"SuperGrok": "SuperGrok", "SuperGrokHeavy": "SuperGrok Heavy", "XPremiumPlus": "X Premium+", "SUBSCRIPTION_TIER_SUPER_GROK_HEAVY": "SuperGrok Heavy", "": ""} {
		if got := suiteXAITierLabel(in); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestSuiteXAIDiagRedactsAndParses(t *testing.T) {
	var calls []string
	app := newConfiguredApp(t)
	app.SetHostCaller(xaiHost(t, map[string]string{
		xaiCredits: `{"config":{"creditUsagePercent":30,"userId":"user-123","email":"owner@example.com","note":"bearer ` + superGrokToken() + `","currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-08T00:00:00Z","end":"2026-10-15T00:00:00Z"}}}`,
		xaiMonthly: `not json owner@example.com`,
	}, &calls))
	response := app.suiteXAIDiag(ManagementRequest{Query: url.Values{}})
	body := string(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", response.StatusCode, body)
	}
	for _, secret := range []string{"owner@example.com", "user-123", "sig-secret-part", superGrokToken()} {
		if strings.Contains(body, secret) {
			t.Fatalf("secret %q leaked: %s", secret, body)
		}
	}
	if len(calls) != 2 || !strings.Contains(body, `"paid_tier_detected":true`) || !strings.Contains(body, `"remaining_percent":70`) || !strings.Contains(body, `"tier":1`) {
		t.Fatalf("body = %s calls=%v", body, calls)
	}
	if again := app.suiteXAIDiag(ManagementRequest{Query: url.Values{}}); again.StatusCode != http.StatusTooManyRequests || len(calls) != 2 {
		t.Fatalf("diagnostic must be rate limited: %d calls=%v", again.StatusCode, calls)
	}
}

// Exact shapes returned by the user's SuperGrok account on 2026-10-12
// (diag_xai_billing.sh, redacted). creditUsagePercent is on a 0–100 scale:
// 1.0 means 1 % used (public reports: 45 → 55 % left; "~1 % emits 1").
const (
	realSuperGrokCredits = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-12T06:01:34.275124+00:00","end":"2026-10-19T06:01:34.275124+00:00"},"creditUsagePercent":1.0,"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"productUsage":[{"product":"GrokBuild","usagePercent":1.0}],"isUnifiedBillingUser":true,"prepaidBalance":{"val":0},"topUpMethod":"TOP_UP_METHOD_SAVED_PAYMENT_METHOD","billingPeriodStart":"2026-10-12T06:01:34.275124+00:00","billingPeriodEnd":"2026-10-19T06:01:34.275124+00:00"}}`
	realSuperGrokMonthly = `{"config":{"monthlyLimit":{"val":0},"used":{"val":0},"onDemandCap":{"val":0},"billingPeriodStart":"2026-10-01T00:00:00+00:00","billingPeriodEnd":"2026-11-01T00:00:00+00:00","history":[{"totalUsed":{"val":0}},{"totalUsed":{"val":0}},{"totalUsed":{"val":0}}]}}`
)

func TestSuiteRealSuperGrokShapes(t *testing.T) {
	var calls []string
	app := newConfiguredApp(t)
	app.SetHostCaller(xaiHost(t, map[string]string{xaiCredits: realSuperGrokCredits, xaiMonthly: realSuperGrokMonthly}, &calls))
	result, err := app.fetchAuthQuota("", hostAuthFile{AuthIndex: "f738b204b32ffad5", Type: "xai"}, "xai")
	if err != nil {
		t.Fatal(err)
	}
	labels := []string{}
	for _, row := range result.Quota {
		labels = append(labels, row.Label)
	}
	// Weekly 99 % left, GrokBuild as a sub-row, no 0/0 monthly row, no prepaid / on-demand rows.
	if len(result.Quota) != 2 || labels[0] != "Weekly limit" || labels[1] != "GrokBuild usage" {
		t.Fatalf("labels %v", labels)
	}
	weekly := result.Quota[0]
	if *weekly.RemainingPercent != 99 || weekly.ResetAt != "2026-10-19T06:01:34Z" {
		t.Fatalf("weekly %+v (reset = 2026-10-19 14:01 UTC+8)", weekly)
	}
	if result.CreditBalance != "" || result.Plan != "Paid" {
		t.Fatalf("plan %q balance %q", result.Plan, result.CreditBalance)
	}
	reset, _ := time.Parse(time.RFC3339, weekly.ResetAt)
	if got := reset.In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02 15:04"); got != "2026-10-19 14:01" {
		t.Fatalf("reset %s", got)
	}
}

func TestSuiteMonthlyRowWithRealLimitKept(t *testing.T) {
	result := authQuotaResponse{Quota: []quotaRow{{Label: "Monthly allowance", Limit: floatPointer(10), Used: floatPointer(0)}, {Label: "Monthly allowance", Limit: floatPointer(0), Used: floatPointer(0)}}}
	suiteHideEmptyMonthly(&result, 0)
	if len(result.Quota) != 1 || *result.Quota[0].Limit != 10 {
		t.Fatalf("%+v", result.Quota)
	}
}
