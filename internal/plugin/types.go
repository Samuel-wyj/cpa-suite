// Package plugin implements the CLIProxyAPI plugin RPC contract.
package plugin

import (
	"cpa-key-billing/internal/suite"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 7
)

// Plugin identity. PluginID doubles as the dynamic library file name, the
// `plugins.configs` key, and the Management/resource route segment.
const (
	// CPA Suite: renamed fork of cpa-key-billing (docs/UPSTREAM_PATCHES.md #1).
	PluginID   = "cpa-suite"
	PluginName = "cpa-suite"
	Version    = suite.Version

	MenuLabel       = "CPA Suite 控制台"
	MenuDescription = "Manage downstream API key routing, billing, concurrency limits, subscription quotas, and usage"

	GitHubRepository = "https://github.com/haowang02/cpa-plugin-key-billing"
)

const (
	MethodPluginRegister    = "plugin.register"
	MethodPluginReconfigure = "plugin.reconfigure"

	MethodRequestInterceptBefore = "request.intercept_before"
	MethodRequestInterceptAfter  = "request.intercept_after"
	MethodRequestComplete        = "request.complete"
	MethodSchedulerPick          = "scheduler.pick"

	MethodUsageHandle = "usage.handle"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"
)

const (
	// MetadataCallerScope is sha256("cli-proxy-api:caller-scope:v1\x00"+apiKey)
	// in hex. It is the only downstream-key identifier available at interception
	// time, and it covers keys presented via query string as well as headers.
	MetadataCallerScope    = "caller_scope"
	MetadataGenerate       = "generate"
	MetadataSource         = "source"
	MetadataRequestPath    = "request_path"
	MetadataRequestedModel = "requested_model"
	MetadataSelectedAuth   = "selected_auth_id"
	MetadataSelectedIndex  = "selected_auth_index"
	// SourcePluginHostModelCallback is the MetadataSource value for nested
	// plugin-initiated executions, which bypass client model and quota checks.
	SourcePluginHostModelCallback = "plugin_host_model_callback"
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type LifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type Registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

type Metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []ConfigField `json:"ConfigFields"`
}

type ConfigField struct {
	Name string `json:"Name"`
	Type string `json:"Type"`
	// Default is shown by CPA v8.0.29+ management (pluginapi.ConfigField.Default);
	// older hosts ignore the unknown key.
	Default     any    `json:"Default,omitempty"`
	Description string `json:"Description"`
}

type Capabilities struct {
	RequestInterceptor     bool `json:"request_interceptor"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	UsagePlugin            bool `json:"usage_plugin"`
	ManagementAPI          bool `json:"management_api"`
	Scheduler              bool `json:"scheduler"`
	// SchedulerAcrossPriorities asks CPA v8.0.28+ for available candidates
	// of every priority tier, not only the highest one.
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities,omitempty"`
	// RequestInterceptorOmitBody* (schema v7, CPA v8.0.44+) stop the host from
	// cloning the request body for the interceptor; the suite reads only
	// Model, RequestedModel, Headers and Metadata.
	RequestInterceptorOmitBodyBefore bool `json:"request_interceptor_omit_body_before,omitempty"`
	RequestInterceptorOmitBodyAfter  bool `json:"request_interceptor_omit_body_after,omitempty"`
}

type SchedulerPickRequest struct {
	Provider  string   `json:"Provider"`
	Providers []string `json:"Providers"`
	Model     string   `json:"Model"`
	Options   struct {
		Headers  map[string][]string `json:"Headers"`
		Metadata map[string]any      `json:"Metadata"`
	} `json:"Options"`
	Candidates []SchedulerAuthCandidate `json:"Candidates"`
}

type SchedulerAuthCandidate struct {
	ID         string            `json:"ID"`
	Provider   string            `json:"Provider"`
	Priority   int               `json:"Priority"`
	Status     string            `json:"Status"`
	Attributes map[string]string `json:"Attributes"`
}

type SchedulerPickResponse struct {
	AuthID  string `json:"AuthID,omitempty"`
	Handled bool   `json:"Handled"`
}

type RequestCompletion struct {
	RequestID  string `json:"RequestID"`
	Outcome    string `json:"Outcome"`
	StatusCode int    `json:"StatusCode"`
	Error      string `json:"Error"`
}

type RequestInterceptRequest struct {
	RequestID      string `json:"RequestID"`
	SourceFormat   string `json:"SourceFormat"`
	Model          string `json:"Model"`
	RequestedModel string `json:"RequestedModel"`
	// Headers are the inbound request headers (0.7: transport detection).
	Headers  http.Header    `json:"Headers,omitempty"`
	Metadata map[string]any `json:"Metadata"`
}

type RequestInterceptResponse struct {
	Terminate       bool        `json:"Terminate,omitempty"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
}

type UsageRecord struct {
	Provider            string        `json:"Provider"`
	ExecutorType        string        `json:"ExecutorType"`
	Model               string        `json:"Model"`
	Alias               string        `json:"Alias"`
	APIKey              string        `json:"APIKey"`
	AuthIndex           string        `json:"AuthIndex"`
	AuthType            string        `json:"AuthType"`
	Source              string        `json:"Source"`
	ReasoningEffort     string        `json:"ReasoningEffort"`
	ServiceTier         string        `json:"ServiceTier"`
	ResponseServiceTier string        `json:"ResponseServiceTier"`
	ResponseModel       string        `json:"ResponseModel"`
	ResponseHeaders     http.Header   `json:"ResponseHeaders"`
	Generate            bool          `json:"Generate"`
	RequestedAt         time.Time     `json:"RequestedAt"`
	Latency             time.Duration `json:"Latency"`
	TTFT                time.Duration `json:"TTFT"`
	Failed              bool          `json:"Failed"`
	Failure             UsageFailure  `json:"Failure"`
	Detail              UsageDetail   `json:"Detail"`
}

type UsageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type UsageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

type ManagementRegistrationResponse struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

type ManagementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type ResourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type ManagementRequest struct {
	Method         string      `json:"Method"`
	Path           string      `json:"Path"`
	Headers        http.Header `json:"Headers"`
	Query          url.Values  `json:"Query"`
	Body           []byte      `json:"Body"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type ManagementResponse struct {
	StatusCode int         `json:"StatusCode,omitempty"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}
