package plugin

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"sync"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/sqlite"
	"cpa-key-billing/internal/suite"
)

type App struct {
	store                 *billing.Store
	hostCaller            HostCaller
	admissionsMu          sync.Mutex
	admissions            map[string]*requestAdmission
	routingMu             sync.Mutex
	credentials           map[string]credentialView
	credentialsByRawID    map[string]string
	credentialRefsByIndex map[string]string
	scheduler             subsetScheduler
	pending               map[string]pendingRouteLog
	pendingSequence       uint64
	routeOrder            routeOrderState   // 
	suite                 *suite.Controller // CPA Suite hook
}

func (a *App) SetHostCaller(caller HostCaller) {
	a.hostCaller = caller
}

func NewApp() *App {
	return newApp(billing.NewStore(openRepository, nil))
}

func newApp(store *billing.Store) *App {
	return &App{
		store:                 store,
		admissions:            make(map[string]*requestAdmission),
		credentials:           make(map[string]credentialView),
		credentialsByRawID:    make(map[string]string),
		credentialRefsByIndex: make(map[string]string),
		pending:               make(map[string]pendingRouteLog),
		suite:                 newSuiteController(), // CPA Suite hook
	}
}

func openRepository(path string) (billing.Repository, error) {
	return sqlite.Open(path)
}

// HandleMethod dispatches one host RPC call. A panic anywhere below is
// converted into an error envelope: the host fuses a panicking plugin, and
// taking the whole proxy down over a billing bug is not an acceptable trade.
func (a *App) HandleMethod(method string, request []byte) (response []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = nil
			err = fmt.Errorf("Plugin call %s panicked: %v", method, recovered)
			if a != nil && a.store != nil {
				a.store.AddPluginLog(billing.PluginLogError, "%v", err)
			}
		}
	}()
	return a.handleMethod(method, request)
}

func (a *App) handleMethod(method string, request []byte) ([]byte, error) {
	if response, handled, err := a.suiteGate(method, request); handled { // CPA Suite hook
		return response, err
	}
	switch method {
	case MethodPluginRegister, MethodPluginReconfigure:
		if errConfigure := a.configure(request); errConfigure != nil {
			a.store.AddPluginLog(billing.PluginLogError, "Failed to apply plugin configuration: %v", errConfigure)
			return nil, errConfigure
		}
		return OKEnvelope(registration())
	case MethodRequestInterceptBefore:
		return a.interceptBeforeAuth(request)
	case MethodRequestInterceptAfter:
		return a.interceptAfterAuth(request)
	case MethodRequestComplete:
		return a.completeRequest(request)
	case MethodSchedulerPick:
		return a.pickCredential(request)
	case MethodUsageHandle:
		return a.handleUsage(request)
	case MethodManagementRegister:
		return OKEnvelope(managementRegistration())
	case MethodManagementHandle:
		return a.handleManagement(request)
	default:
		return ErrorEnvelope("unknown_method", "Unsupported plugin method: "+method, http.StatusNotFound), nil
	}
}

func (a *App) Shutdown() {
	if a == nil || a.store == nil {
		return
	}
	a.store.Close()
	a.suite.Close() // CPA Suite hook
}

func (a *App) configure(raw []byte) error {
	var req LifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return fmt.Errorf("Parse plugin lifecycle request: %w", errUnmarshal)
		}
	}
	prepared, errPrepare := a.suitePrepareConfig(req.ConfigYAML) // CPA Suite hook
	if errPrepare != nil {
		return errPrepare
	}
	req.ConfigYAML = prepared
	cfg, errDecode := billing.DecodeConfig(req.ConfigYAML)
	if errDecode != nil {
		return errDecode
	}
	if errConfigure := func() error {
		a.routingMu.Lock()
		defer a.routingMu.Unlock()
		previous := a.store.ConfigCredentials()
		if err := a.store.Configure(cfg); err != nil {
			return err
		}
		if loaded := a.store.ConfigCredentials(); !maps.Equal(previous, loaded) {
			a.replaceSyncedCredentials(previous, loaded, nil)
		}
		return nil
	}(); errConfigure != nil {
		return errConfigure
	}
	// Refresh records its result; a download failure does not disable custom prices.
	_, _ = a.store.EnsureReferencePrices()
	return nil
}

func registration() Registration {
	return Registration{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			Name:             PluginName,
			Version:          Version,
			Author:           PluginName,
			GitHubRepository: GitHubRepository,
			Logo:             pluginLogo,
			ConfigFields: append([]ConfigField{
				{
					Name:        "debug",
					Type:        "boolean",
					Description: "Record debug logs, including routing and reference price matching",
				},
				{
					Name:        "codex_fast_mode_billing",
					Type:        "boolean",
					Description: "Bill Codex priority requests at 2.5 times the standard cost",
				},
				{
					Name:        "mask_api_key_view_emails",
					Type:        "boolean",
					Description: "Mask email addresses in API key account responses",
				},
				{
					Name:        "allow_api_key_quota_reset",
					Type:        "boolean",
					Description: "Allow API key users to reset Codex and Claude auth file quotas using upstream resets",
				},
				{
					Name:        "concurrency_stale_minutes",
					Type:        "integer",
					Default:     billing.DefaultConcurrencyStaleMinutes,
					Description: "Reclaim concurrency slots with no request activity for this many minutes (0 disables)",
				},
				{
					Name:        "state_file",
					Type:        "string",
					Description: "Billing database file path",
				},
			}, suiteConfigFields()...), // CPA Suite hook
		},
		Capabilities: Capabilities{
			RequestInterceptor:     true,
			RequestLifecyclePlugin: true,
			UsagePlugin:            true,
			ManagementAPI:          true,
			Scheduler:              true,
			// see every priority tier so a rule's own order
			// can fall through to lower CPA tiers.
			SchedulerAcrossPriorities: true,
			// no request body needed in either phase.
			RequestInterceptorOmitBodyBefore: true,
			RequestInterceptorOmitBodyAfter:  true,
		},
	}
}
