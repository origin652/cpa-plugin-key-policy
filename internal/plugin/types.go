package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
)

const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 1

	MethodPluginRegister    = "plugin.register"
	MethodPluginReconfigure = "plugin.reconfigure"

	MethodFrontendAuthIdentifier   = "frontend_auth.identifier"
	MethodFrontendAuthAuthenticate = "frontend_auth.authenticate"

	MethodModelRoute = "model.route"

	MethodResponseInterceptAfter = "response.intercept_after"

	// MethodSchedulerPick is the host->plugin call that asks this plugin to
	// choose an auth candidate among those available for a routed provider,
	// before the host's built-in scheduler runs. We use it to honor a ModelRule's
	// Group: when a downstream key pinned a tier (e.g. codex "team"), we filter
	// candidates by their plan_type attribute so the request only ever lands on
	// an auth file of that tier. Returning Handled=false falls back to the host
	// scheduler, so providers without a tier concept behave as before.
	MethodSchedulerPick = "scheduler.pick"

	// MethodUsageHandle is the host->plugin call that delivers a finalized
	// usage record (tokens already parsed by CPA) after a request completes.
	// This is the billing entry point that ALSO fires for streaming responses
	// (unlike response.intercept_after, which the host never invokes on the
	// streaming path — it only invokes response.intercept_stream_chunk).
	MethodUsageHandle = "usage.handle"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"
)

const (
	PluginID   = "cpa-key-policy"
	PluginName = "cpa-key-policy"
	Version    = "0.2.2"
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
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
	Logo             string        `json:"Logo,omitempty"`
	ConfigFields     []ConfigField `json:"ConfigFields"`
}

type ConfigField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

type Capabilities struct {
	FrontendAuthProvider          bool `json:"frontend_auth_provider"`
	FrontendAuthProviderExclusive bool `json:"frontend_auth_provider_exclusive,omitempty"`
	ModelRouter                   bool `json:"model_router"`
	Scheduler                     bool `json:"scheduler,omitempty"`
	ResponseInterceptor           bool `json:"response_interceptor"`
	UsagePlugin                   bool `json:"usage_plugin"`
	ManagementAPI                 bool `json:"management_api"`
}

type IdentifierResponse struct {
	Identifier string `json:"identifier"`
}

type FrontendAuthRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`
}

type FrontendAuthResponse struct {
	Authenticated bool              `json:"Authenticated"`
	Principal     string            `json:"Principal,omitempty"`
	Metadata      map[string]string `json:"Metadata,omitempty"`
}

type ModelRouteRequest struct {
	SourceFormat       string         `json:"SourceFormat"`
	RequestedModel     string         `json:"RequestedModel"`
	Stream             bool           `json:"Stream"`
	Headers            http.Header    `json:"Headers"`
	Query              url.Values     `json:"Query"`
	Body               []byte         `json:"Body"`
	Metadata           map[string]any `json:"Metadata"`
	AvailableProviders []string       `json:"AvailableProviders"`
}

type ModelRouteResponse struct {
	Handled     bool   `json:"Handled"`
	TargetKind  string `json:"TargetKind,omitempty"`
	Target      string `json:"Target,omitempty"`
	TargetModel string `json:"TargetModel,omitempty"`
	Reason      string `json:"Reason,omitempty"`
}

// SchedulerPickRequest is the payload of the host->plugin scheduler.pick call.
// It mirrors pluginapi.SchedulerPickRequest. The plugin only needs Provider,
// Model, Options.Metadata (carrying the group we stamped at authenticate time)
// and Candidates[].Attributes (codex plan_type etc.).
type SchedulerPickRequest struct {
	Provider   string                   `json:"Provider,omitempty"`
	Providers  []string                 `json:"Providers,omitempty"`
	Model      string                   `json:"Model"`
	Stream     bool                     `json:"Stream,omitempty"`
	Options    SchedulerPickOptions     `json:"Options"`
	Candidates []SchedulerAuthCandidate `json:"Candidates"`
}

type SchedulerPickOptions struct {
	Headers  map[string][]string `json:"Headers,omitempty"`
	Metadata map[string]any      `json:"Metadata,omitempty"`
}

// SchedulerAuthCandidate describes one selectable auth record.
type SchedulerAuthCandidate struct {
	ID         string            `json:"ID"`
	Provider   string            `json:"Provider"`
	Priority   int               `json:"Priority,omitempty"`
	Status     string            `json:"Status,omitempty"`
	Attributes map[string]string `json:"Attributes,omitempty"`
	Metadata   map[string]any    `json:"Metadata,omitempty"`
}

type SchedulerPickResponse struct {
	// AuthID picks a specific candidate; leave empty and set Handled=false to
	// defer to the host scheduler.
	AuthID string `json:"AuthID,omitempty"`
	// Handled reports whether the plugin made a scheduling decision.
	Handled bool `json:"Handled"`
}

type ResponseInterceptRequest struct {
	SourceFormat    string         `json:"SourceFormat"`
	Model           string         `json:"Model"`
	RequestedModel  string         `json:"RequestedModel"`
	Stream          bool           `json:"Stream"`
	RequestHeaders  http.Header    `json:"RequestHeaders"`
	ResponseHeaders http.Header    `json:"ResponseHeaders"`
	OriginalRequest []byte         `json:"OriginalRequest"`
	RequestBody     []byte         `json:"RequestBody"`
	Body            []byte         `json:"Body"`
	StatusCode      int            `json:"StatusCode"`
	Metadata        map[string]any `json:"Metadata"`
}

// UsageHandleRequest is the payload of the host->plugin usage.handle call.
// CPA parses the token counts itself (from the upstream response, including
// the final usage frame of a stream) and delivers them here after a request
// completes — both streaming and non-streaming. This is the reliable billing
// entry point: the host never invokes response.intercept_after on streaming
// responses, so the plugin cannot rely on that alone to bill streams.
type UsageHandleRequest struct {
	// Model is the resolved upstream model id.
	Model string `json:"Model"`
	// Alias is the client-requested model name (what the caller passed in the
	// request body's "model" field), when one was used. This is what we match
	// against our ModelRule aliases to price the request.
	Alias string `json:"Alias"`
	// APIKey is the client's downstream key (the cpa_... value), when available.
	// We hash it to find the owning key config — same lookup path as auth.
	APIKey string      `json:"APIKey"`
	Failed bool        `json:"Failed"`
	Detail UsageDetail `json:"Detail"`
}

// UsageDetail mirrors CPA's usage token breakdown. Only the fields we bill on.
type UsageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

// UsageHandleResponse is empty: usage.handle is a fire-and-forget notification.
type UsageHandleResponse struct{}

type ResponseInterceptResponse struct {
	Headers      http.Header `json:"Headers,omitempty"`
	Body         []byte      `json:"Body,omitempty"`
	ClearHeaders []string    `json:"ClearHeaders,omitempty"`
}

type ManagementRegistrationRequest struct {
	BasePath         string `json:"BasePath"`
	ResourceBasePath string `json:"ResourceBasePath"`
}

type ManagementRegistrationResponse struct {
	Routes    []ManagementRoute `json:"Routes"`
	Resources []ResourceRoute   `json:"Resources,omitempty"`
}

type ManagementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

// ResourceRoute declares a browser-navigable, unauthenticated GET resource that
// CPA serves under /v0/resource/plugins/<pluginID><Path>.
type ResourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type ManagementRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`
}

type ManagementResponse struct {
	StatusCode int         `json:"StatusCode,omitempty"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

func OKEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

func ErrorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(Envelope{
		OK: false,
		Error: &EnvelopeError{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
		},
	})
	return raw
}
