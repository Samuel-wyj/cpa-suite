package plugin

// CPA Suite: xAI / SuperGrok quota fixes on top of upstream fetchXAIQuota.
//
// Upstream v1.3.20 returned only {"plan":"Paid"} for any OAuth token whose JWT
// carries tier >= 1 and never called the billing API, so SuperGrok accounts
// showed "no displayable quota". The hook in fetchAuthQuota now queries the
// same official endpoints for them. This file then handles the shapes the
// Grok CLI billing API is known to return (Grok CLI / TokenBar / Orca /
// QuotaKit observations):
//   - format=credits omits creditUsagePercent (proto default) right after a
//     weekly reset: with a self-contained USAGE_PERIOD_TYPE_WEEKLY period and
//     no positive on-demand cap that is a genuine 0% used, not "unknown";
//   - the plain /v1/billing ledger can be a stale calendar month; once a
//     weekly meter exists, a monthly row whose period already ended is dropped;
//   - subscriptionTier names the plan (SuperGrok, SuperGrok Heavy, ...).

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/messages"
)

func suiteFixXAIQuota(result *authQuotaResponse, weekly map[string]any, first int, now time.Time) {
	if first > len(result.Quota) {
		first = len(result.Quota)
	}
	weeklyRows := 0
	for _, row := range result.Quota[first:] {
		if row.Label == "Weekly limit" || strings.HasSuffix(row.Label, " usage") {
			weeklyRows++
		}
	}
	if weeklyRows == 0 && suiteXAIZeroWeekly(weekly) {
		result.Quota = append(result.Quota[:first:first], append([]quotaRow{{Label: "Weekly limit", LabelMessage: messages.New("Weekly limit"),
			RemainingPercent: remainingPercent(100), ResetAt: xaiResetAt(weekly)}}, result.Quota[first:]...)...)
		// Orca: the plain ledger may still describe last month; never let it
		// contradict a just-reset weekly meter.
		suiteDropEndedMonthly(result, first, now)
	}
	suiteHideEmptyMonthly(result, first)
	if tier := suiteXAITierLabel(firstString(weekly, "subscriptionTier", "subscription_tier")); tier != "" && (result.Plan == "" || result.Plan == "Paid") {
		result.Plan = tier
	}
}

func suiteDropEndedMonthly(result *authQuotaResponse, first int, now time.Time) {
	kept := result.Quota[:first]
	for _, row := range result.Quota[first:] {
		if row.Label == "Monthly allowance" && row.ResetAt != "" {
			if end, err := time.Parse(time.RFC3339, row.ResetAt); err == nil && !end.After(now) {
				continue
			}
		}
		kept = append(kept, row)
	}
	result.Quota = kept
}

// suiteXAIZeroWeekly: percent omitted, weekly period present and valid, no
// positive on-demand cap (a positive cap without a percent stays unknown).
func suiteXAIZeroWeekly(config map[string]any) bool {
	if config == nil {
		return false
	}
	if _, ok := floatValue(config, "creditUsagePercent", "credit_usage_percent"); ok {
		return false
	}
	if cap, ok := usdValue(config, "onDemandCap", "on_demand_cap"); ok && cap > 0 {
		return false
	}
	period := objectMap(config, "currentPeriod", "current_period")
	if !strings.EqualFold(firstString(period, "type"), "USAGE_PERIOD_TYPE_WEEKLY") {
		return false
	}
	start, errStart := time.Parse(time.RFC3339Nano, firstString(period, "start"))
	end, errEnd := time.Parse(time.RFC3339Nano, firstString(period, "end"))
	return errStart == nil && errEnd == nil && end.After(start)
}

var (
	suiteTierWords = regexp.MustCompile(`([a-z])([A-Z])`)
	suiteTierCaps  = regexp.MustCompile(`([A-Z])([A-Z][a-z])`)
)

func suiteXAITierLabel(tier string) string {
	tier = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(tier, "SUBSCRIPTION_TIER_"), "TIER_"))
	if tier == "" || len(tier) > 40 {
		return ""
	}
	if strings.ToUpper(tier) == tier {
		parts := strings.Split(strings.ToLower(tier), "_")
		for i, p := range parts {
			if p != "" {
				parts[i] = strings.ToUpper(p[:1]) + p[1:]
			}
		}
		tier = strings.Join(parts, " ")
	}
	tier = suiteTierCaps.ReplaceAllString(suiteTierWords.ReplaceAllString(tier, "$1 $2"), "$1 $2")
	tier = strings.NewReplacer("Super Grok", "SuperGrok", "X Premium Plus", "X Premium+").Replace(tier)
	return tier
}

// ---- Read-only diagnostic: raw xAI billing responses, secrets redacted ----

const routeSuiteXAIDiag = "/suite/diag/xai-billing"

var (
	suiteDiagMu   sync.Mutex
	suiteDiagLast = map[string]time.Time{}
)

var (
	suiteDiagSecretKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|api_?key|cookie|session|authorization|email|e_mail|phone|user_?id|userid|account_?id|^sub$|^id$|_id$|Id$)`)
	suiteDiagEmail     = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	suiteDiagJWT       = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}(\.[A-Za-z0-9_-]*)?`)
)

func suiteDiagRedact(value any, c *authQuotaClient) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if suiteDiagSecretKey.MatchString(key) {
				if _, nested := item.(map[string]any); !nested {
					out[key] = "[redacted]"
					continue
				}
			}
			out[key] = suiteDiagRedact(item, c)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = suiteDiagRedact(v[i], c)
		}
		return out
	case string:
		return suiteDiagRedactText(v, c)
	}
	return value
}

func suiteDiagRedactText(s string, c *authQuotaClient) string {
	s = c.redact(s)
	s = suiteDiagJWT.ReplaceAllString(s, "[redacted-jwt]")
	return suiteDiagEmail.ReplaceAllString(s, "[redacted-email]")
}

type suiteDiagCall struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
	JSON   any    `json:"json,omitempty"`
	Text   string `json:"text,omitempty"`
}

// suiteXAIDiag performs exactly the two GETs upstream's quota query makes
// (both read-only billing views), at most once per account per 30 s, and
// returns them redacted together with how this build parses them.
func (a *App) suiteXAIDiag(req ManagementRequest) ManagementResponse {
	index := strings.TrimSpace(req.Query.Get("auth_index"))
	files, err := a.listHostAuthFiles()
	if err != nil {
		return JSONResponse(http.StatusBadGateway, map[string]any{"error": err.Error()})
	}
	var file *hostAuthFile
	xai := []map[string]string{}
	for i := range files {
		if authCategory(files[i].Type) != "xai" || strings.EqualFold(strings.TrimSpace(files[i].AccountType), "api_key") {
			continue
		}
		xai = append(xai, map[string]string{"auth_index": files[i].AuthIndex, "name": suiteDiagEmail.ReplaceAllString(files[i].Name, "[redacted-email]")})
		if files[i].AuthIndex == index || (index == "" && file == nil) {
			file = &files[i]
		}
	}
	if file == nil {
		return JSONResponse(http.StatusNotFound, map[string]any{"error": "no xAI OAuth auth file found (pass ?auth_index=)", "xai_auth_files": xai})
	}
	suiteDiagMu.Lock()
	if last, ok := suiteDiagLast[file.AuthIndex]; ok && time.Since(last) < 30*time.Second {
		suiteDiagMu.Unlock()
		return JSONResponse(http.StatusTooManyRequests, map[string]any{"error": "rate limited: one diagnostic per account every 30 seconds"})
	}
	suiteDiagLast[file.AuthIndex] = time.Now()
	suiteDiagMu.Unlock()

	client, err := a.openAuthQuotaClient(req.HostCallbackID, *file)
	if err != nil {
		return JSONResponse(http.StatusBadGateway, map[string]any{"error": err.Error()})
	}
	defer client.close()
	claims := map[string]any{}
	if parts := strings.Split(credentialToken(client.credential), "."); len(parts) >= 2 {
		if payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1]); errDecode == nil {
			var all map[string]any
			if json.Unmarshal(payload, &all) == nil {
				names := make([]string, 0, len(all))
				for key, value := range all {
					names = append(names, key)
					lower := strings.ToLower(key)
					if lower == "tier" || strings.HasSuffix(lower, "/tier") || strings.HasSuffix(lower, ":tier") || lower == "exp" || lower == "iat" || lower == "iss" || lower == "aud" {
						claims[key] = value
					}
				}
				sort.Strings(names)
				claims["_claim_names"] = names
			}
		}
	}
	recorded := map[string]hostHTTPResponse{}
	calls := []suiteDiagCall{}
	headers := suiteXAIHeaders(client.credential)
	for _, endpoint := range []string{"https://cli-chat-proxy.grok.com/v1/billing?format=credits", "https://cli-chat-proxy.grok.com/v1/billing"} {
		h := headers.Clone()
		h.Set("Authorization", "Bearer "+client.token)
		response, errCall := client.do(hostHTTPRequest{HostCallbackID: req.HostCallbackID, Method: http.MethodGet, URL: endpoint, Headers: h})
		call := suiteDiagCall{URL: endpoint, Status: response.StatusCode}
		if errCall != nil {
			call.Error = suiteDiagRedactText(errCall.Error(), client)
		} else {
			recorded[endpoint] = response
			var decoded any
			if json.Unmarshal(response.Body, &decoded) == nil {
				call.JSON = suiteDiagRedact(decoded, client)
			} else {
				text := string(response.Body)
				if len(text) > 2000 {
					text = text[:2000] + "…"
				}
				call.Text = suiteDiagRedactText(text, client)
			}
		}
		calls = append(calls, call)
	}
	// Parse the recorded bodies offline with this build's parser (no extra calls).
	replay := &authQuotaClient{credential: client.credential, token: client.token, hostCaller: func(_ string, payload any) (json.RawMessage, error) {
		r, ok := recorded[payload.(hostHTTPRequest).URL]
		if !ok {
			return nil, fmt.Errorf("not recorded")
		}
		return json.Marshal(r)
	}}
	parsed := authQuotaResponse{FetchedAt: time.Now().UTC(), Quota: []quotaRow{}}
	if paidXAICredential(client.credential) {
		parsed.Plan = "Paid"
	}
	parseErr := ""
	if errParse := replay.fetchXAIQuota(&parsed); errParse != nil {
		parseErr = suiteDiagRedactText(errParse.Error(), client)
	}
	return JSONResponse(http.StatusOK, map[string]any{
		"version": Version, "auth_index": file.AuthIndex, "auth_type": file.Type,
		"paid_tier_detected": paidXAICredential(client.credential), "jwt": claims,
		"requests": calls, "parsed": parsed, "parse_error": parseErr, "xai_auth_files": xai,
		"note": "Read-only: the same two GET billing views the quota button calls. Tokens, emails, user/account ids are redacted.",
	})
}

func suiteXAIHeaders(credential map[string]any) http.Header {
	headers := http.Header{
		"X-Xai-Token-Auth":      {"xai-grok-cli"},
		"X-Grok-Client-Version": {xaiGrokClientVersion},
		"User-Agent":            {"grok-pager/" + xaiGrokClientVersion + " grok-shell/" + xaiGrokClientVersion},
		"Accept":                {"*/*"},
	}
	if userID := credentialString(credential, "user_id", "userId", "xai_user_id", "xaiUserId", "sub", "subject"); userID != "" {
		headers.Set("X-Userid", userID)
	}
	return headers
}

// suiteHideEmptyMonthly drops the plain-ledger "Monthly allowance" row when it
// reports monthlyLimit 0 and used 0: unified-billing (SuperGrok) accounts have
// no monthly cap there, and 0/0 would read as "exhausted".
func suiteHideEmptyMonthly(result *authQuotaResponse, first int) {
	kept := result.Quota[:first]
	for _, row := range result.Quota[first:] {
		if row.Label == "Monthly allowance" && row.Limit != nil && *row.Limit == 0 && (row.Used == nil || *row.Used == 0) {
			continue
		}
		kept = append(kept, row)
	}
	result.Quota = kept
}
