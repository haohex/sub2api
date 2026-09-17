package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Codex turn-state probe settings are account-scoped. The raw state is kept in
// a separate managed cache key so the admin DTO can expose the configuration
// without ever returning the opaque upstream value.
const (
	CodexTurnStateProbeEnabledExtraKey = "codex_turn_state_probe_enabled"
	CodexTurnStateProbeProxyIDExtraKey = "codex_turn_state_probe_proxy_id"
	CodexTurnStateProbeFailureExtraKey = "codex_turn_state_probe_failures"
	CodexTurnStateProbeRetryExtraKey   = "codex_turn_state_probe_retry"
	// Kept as a migration key for older configurations. Model selection now
	// comes from credentials.model_mapping and this key is removed on update.
	CodexTurnStateProbeModelsExtraKey = "codex_turn_state_probe_models"
	CodexTurnStateProbeCacheExtraKey  = "codex_turn_state_probe_cache"

	codexTurnStateLength              = 292
	codexTurnStateTTL                 = time.Hour
	codexTurnStateRefreshBeforeExpiry = 5 * time.Minute
	codexTurnStateProbeMaxAttempts    = 50
	codexTurnStateProbeTimeout        = 15 * time.Second
	codexTurnStateProbeRefreshPeriod  = time.Minute
)

// CodexTurnStateProbeConfig is the validated account-level setting. The model
// set is derived from the account's model restriction mapping.
type CodexTurnStateProbeConfig struct {
	Enabled bool
	ProxyID int64
}

type codexTurnStateProbeFailure struct {
	Attempts int
	FailedAt time.Time
}

type codexTurnStateCacheEntry struct {
	State          string
	ObtainedAt     time.Time
	ExpiresAt      time.Time
	ProxyID        int64
	CredentialHash string
}

// IsCodexTurnStateProbeAccount reports whether this feature can use the
// account's own ChatGPT/Codex bearer token. CPR, API-key, and credential-shadow
// accounts are not eligible because sub2api does not hold their independent
// upstream ChatGPT token.
func IsCodexTurnStateProbeAccount(account *Account) bool {
	return account != nil && !account.IsCredentialShadow() && account.Platform == PlatformOpenAI && account.IsOpenAIOAuthLike()
}

// CodexTurnStateProbeConfigFromExtra parses and validates the user-controlled
// part of an account extra document. It intentionally ignores the managed
// cache key.
func CodexTurnStateProbeConfigFromExtra(extra map[string]any) (CodexTurnStateProbeConfig, error) {
	config := CodexTurnStateProbeConfig{}
	if extra == nil {
		return config, nil
	}

	enabled, present := extra[CodexTurnStateProbeEnabledExtraKey]
	if !present {
		return config, nil
	}
	value, ok := enabled.(bool)
	if !ok {
		return config, errors.New(CodexTurnStateProbeEnabledExtraKey + " must be a boolean")
	}
	config.Enabled = value
	if !value {
		return config, nil
	}

	proxyID, ok := positiveInt64Extra(extra[CodexTurnStateProbeProxyIDExtraKey])
	if !ok {
		return config, errors.New(CodexTurnStateProbeProxyIDExtraKey + " must be a positive integer")
	}
	config.ProxyID = proxyID
	return config, nil
}

// NormalizeCodexTurnStateProbeExtra removes malformed/inapplicable settings
// from ordinary account updates. Model selection is derived from the account's
// model restriction mapping, while cache/failure state is managed separately.
func NormalizeCodexTurnStateProbeExtra(platform, accountType string, extra map[string]any) (map[string]any, error) {
	if extra == nil {
		return nil, nil
	}
	if platform != PlatformOpenAI || (accountType != AccountTypeOAuth && accountType != AccountTypeSetupToken) {
		delete(extra, CodexTurnStateProbeEnabledExtraKey)
		delete(extra, CodexTurnStateProbeProxyIDExtraKey)
		delete(extra, CodexTurnStateProbeModelsExtraKey)
		delete(extra, CodexTurnStateProbeRetryExtraKey)
		return extra, nil
	}

	config, err := CodexTurnStateProbeConfigFromExtra(extra)
	if err != nil {
		return nil, err
	}
	if !config.Enabled {
		// An absent or false setting is the disabled state. Do not leave old
		// model/proxy selections around for a future accidental cache reuse.
		delete(extra, CodexTurnStateProbeProxyIDExtraKey)
		delete(extra, CodexTurnStateProbeModelsExtraKey)
		delete(extra, CodexTurnStateProbeRetryExtraKey)
		return extra, nil
	}
	extra[CodexTurnStateProbeProxyIDExtraKey] = config.ProxyID
	delete(extra, CodexTurnStateProbeModelsExtraKey)
	delete(extra, CodexTurnStateProbeRetryExtraKey)
	return extra, nil
}

func positiveInt64Extra(value any) (int64, bool) {
	var parsed int64
	switch value := value.(type) {
	case int:
		parsed = int64(value)
	case int64:
		parsed = value
	case int32:
		parsed = int64(value)
	case uint:
		parsed = int64(value)
	case uint64:
		if value > uint64(^uint64(0)>>1) {
			return 0, false
		}
		parsed = int64(value)
	case float64:
		if value != float64(int64(value)) {
			return 0, false
		}
		parsed = int64(value)
	case json.Number:
		valueInt, err := value.Int64()
		if err != nil {
			return 0, false
		}
		parsed = valueInt
	case string:
		valueInt, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, false
		}
		parsed = valueInt
	default:
		return 0, false
	}
	return parsed, parsed > 0
}

func normalizeCodexTurnStateModels(value any) []string {
	var raw []string
	switch value := value.(type) {
	case []string:
		raw = value
	case []any:
		for _, item := range value {
			if model, ok := item.(string); ok {
				raw = append(raw, model)
			}
		}
	case string:
		for _, model := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
			raw = append(raw, model)
		}
	}

	seen := make(map[string]struct{}, len(raw))
	models := make([]string, 0, len(raw))
	for _, model := range raw {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 200 {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return models
}

// CodexTurnStateProbeModels derives the concrete upstream model IDs from the
// same model restriction mapping used by account routing. An empty mapping
// means the account allows all models, so there is no finite probe set.
func CodexTurnStateProbeModels(account *Account) []string {
	if !IsCodexTurnStateProbeAccount(account) || account.IsOpenAIPassthroughEnabled() {
		return nil
	}
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		return nil
	}
	models := make([]string, 0, len(mapping))
	for _, mapped := range mapping {
		mapped = strings.TrimSpace(mapped)
		if mapped == "" || strings.Contains(mapped, "*") {
			continue
		}
		if account.TargetsChatGPTCodexUpstream() {
			mapped = normalizeOpenAIModelForUpstream(account, mapped)
		}
		models = append(models, mapped)
	}
	models = normalizeCodexTurnStateModels(models)
	sort.Strings(models)
	return models
}

// ApplyConfiguredCodexTurnState overwrites an outbound HTTP header when the
// account has a fresh, exact-model candidate. The function is deliberately
// exported so all OpenAI HTTP builders share the same final override.
func ApplyConfiguredCodexTurnState(account *Account, headers http.Header, model, token string) {
	if headers == nil {
		return
	}
	state := configuredCodexTurnState(account, model, token, time.Now())
	if state == "" {
		return
	}
	headers.Set(openAICodexTurnStateHeader, state)
}

func configuredCodexTurnState(account *Account, model, token string, now time.Time) string {
	if !IsCodexTurnStateProbeAccount(account) || strings.TrimSpace(token) == "" {
		return ""
	}
	config, err := CodexTurnStateProbeConfigFromExtra(account.Extra)
	if err != nil || !config.Enabled {
		return ""
	}
	configuredModels := CodexTurnStateProbeModels(account)
	modelKey := codexTurnStateModelKey(account, model)
	if modelKey == "" || !codexTurnStateConfigContainsModel(account, configuredModels, model, modelKey) {
		return ""
	}
	entry, ok := codexTurnStateCacheFromExtra(account.Extra)[modelKey]
	if !ok || !validCodexTurnStateCacheEntry(entry, config.ProxyID, token, now) {
		return ""
	}
	return entry.State
}

func validCodexTurnStateCacheEntry(entry codexTurnStateCacheEntry, proxyID int64, token string, now time.Time) bool {
	if !validCodexTurnStateCacheMetadata(entry, proxyID, now) || entry.CredentialHash == "" {
		return false
	}
	return entry.CredentialHash == codexTurnStateCredentialHash(token)
}

func validCodexTurnStateCacheMetadata(entry codexTurnStateCacheEntry, proxyID int64, now time.Time) bool {
	if len(entry.State) != codexTurnStateLength || entry.ProxyID != proxyID {
		return false
	}
	if entry.ObtainedAt.IsZero() || entry.ExpiresAt.IsZero() || !now.Before(entry.ExpiresAt) {
		return false
	}
	if entry.ExpiresAt.Sub(entry.ObtainedAt) > codexTurnStateTTL || entry.ExpiresAt.Before(entry.ObtainedAt) {
		return false
	}
	return true
}

func codexTurnStateCacheNeedsRefresh(entry codexTurnStateCacheEntry, now time.Time) bool {
	return !now.Before(entry.ExpiresAt.Add(-codexTurnStateRefreshBeforeExpiry))
}

func codexTurnStateModelKey(account *Account, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if account != nil {
		if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
			model = mapped
		}
		if account.TargetsChatGPTCodexUpstream() {
			model = normalizeOpenAIModelForUpstream(account, model)
		}
	}
	return strings.TrimSpace(model)
}

func codexTurnStateCredentialHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func codexTurnStateCacheFromExtra(extra map[string]any) map[string]codexTurnStateCacheEntry {
	result := make(map[string]codexTurnStateCacheEntry)
	if extra == nil {
		return result
	}
	rawCache, ok := extra[CodexTurnStateProbeCacheExtraKey]
	if !ok {
		return result
	}
	cache, ok := rawCache.(map[string]any)
	if !ok {
		if typed, typedOK := rawCache.(map[string]codexTurnStateCacheEntry); typedOK {
			for model, entry := range typed {
				result[model] = entry
			}
		}
		return result
	}
	for model, rawEntry := range cache {
		entry, ok := decodeCodexTurnStateCacheEntry(rawEntry)
		if ok {
			result[model] = entry
		}
	}
	return result
}

func decodeCodexTurnStateCacheEntry(raw any) (codexTurnStateCacheEntry, bool) {
	if typed, ok := raw.(codexTurnStateCacheEntry); ok {
		return typed, true
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return codexTurnStateCacheEntry{}, false
	}
	state, ok := values["state"].(string)
	if !ok || state == "" {
		return codexTurnStateCacheEntry{}, false
	}
	obtainedAt, ok := parseCodexTurnStateTime(values["obtained_at"])
	if !ok {
		return codexTurnStateCacheEntry{}, false
	}
	expiresAt, ok := parseCodexTurnStateTime(values["expires_at"])
	if !ok {
		return codexTurnStateCacheEntry{}, false
	}
	proxyID, ok := positiveInt64Extra(values["proxy_id"])
	if !ok {
		return codexTurnStateCacheEntry{}, false
	}
	credentialHash, ok := values["credential_hash"].(string)
	if !ok || strings.TrimSpace(credentialHash) == "" {
		return codexTurnStateCacheEntry{}, false
	}
	return codexTurnStateCacheEntry{
		State:          state,
		ObtainedAt:     obtainedAt,
		ExpiresAt:      expiresAt,
		ProxyID:        proxyID,
		CredentialHash: credentialHash,
	}, true
}

func parseCodexTurnStateTime(value any) (time.Time, bool) {
	switch value := value.(type) {
	case time.Time:
		return value, !value.IsZero()
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
		return parsed, err == nil && !parsed.IsZero()
	default:
		return time.Time{}, false
	}
}

func codexTurnStateCacheToExtra(cache map[string]codexTurnStateCacheEntry) map[string]any {
	if len(cache) == 0 {
		return nil
	}
	out := make(map[string]any, len(cache))
	for model, entry := range cache {
		out[model] = map[string]any{
			"state":           entry.State,
			"obtained_at":     entry.ObtainedAt.UTC().Format(time.RFC3339Nano),
			"expires_at":      entry.ExpiresAt.UTC().Format(time.RFC3339Nano),
			"proxy_id":        entry.ProxyID,
			"credential_hash": entry.CredentialHash,
		}
	}
	return out
}

func codexTurnStateProbeFailuresFromExtra(extra map[string]any) map[string]codexTurnStateProbeFailure {
	result := make(map[string]codexTurnStateProbeFailure)
	if extra == nil {
		return result
	}
	raw, ok := extra[CodexTurnStateProbeFailureExtraKey].(map[string]any)
	if !ok {
		return result
	}
	for model, rawFailure := range raw {
		values, ok := rawFailure.(map[string]any)
		if !ok {
			continue
		}
		attempts, ok := positiveInt64Extra(values["attempts"])
		if !ok || attempts > int64(codexTurnStateProbeMaxAttempts) {
			continue
		}
		failedAt, ok := parseCodexTurnStateTime(values["failed_at"])
		if !ok {
			continue
		}
		result[model] = codexTurnStateProbeFailure{Attempts: int(attempts), FailedAt: failedAt}
	}
	return result
}

func codexTurnStateProbeFailuresToExtra(failures map[string]codexTurnStateProbeFailure) map[string]any {
	if len(failures) == 0 {
		return nil
	}
	result := make(map[string]any, len(failures))
	for model, failure := range failures {
		result[model] = map[string]any{
			"attempts":  failure.Attempts,
			"failed_at": failure.FailedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	return result
}

func resetCodexTurnStateProbeFailures(extra map[string]any) {
	if extra == nil {
		return
	}
	failures := codexTurnStateProbeFailuresFromExtra(extra)
	cache := codexTurnStateCacheFromExtra(extra)
	for model := range failures {
		delete(cache, model)
	}
	if encoded := codexTurnStateCacheToExtra(cache); encoded == nil {
		delete(extra, CodexTurnStateProbeCacheExtraKey)
	} else {
		extra[CodexTurnStateProbeCacheExtraKey] = encoded
	}
	delete(extra, CodexTurnStateProbeFailureExtraKey)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func codexTurnStateConfigContainsModel(account *Account, configured []string, requested, modelKey string) bool {
	requested = strings.TrimSpace(requested)
	if containsString(configured, requested) || containsString(configured, modelKey) {
		return true
	}
	for _, model := range configured {
		if codexTurnStateModelKey(account, model) == modelKey {
			return true
		}
	}
	return false
}

// CodexTurnStateProbeService refreshes configured account/model candidates in
// the background. It never reads the SSE body: response headers are enough.
type CodexTurnStateProbeService struct {
	accountRepo   AccountRepository
	proxyRepo     ProxyRepository
	tokenProvider *OpenAITokenProvider
	httpUpstream  HTTPUpstream

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	accountMu sync.Map // account id -> *sync.Mutex
}

func NewCodexTurnStateProbeService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	tokenProvider *OpenAITokenProvider,
	httpUpstream HTTPUpstream,
) *CodexTurnStateProbeService {
	ctx, cancel := context.WithCancel(context.Background())
	return &CodexTurnStateProbeService{
		accountRepo:   accountRepo,
		proxyRepo:     proxyRepo,
		tokenProvider: tokenProvider,
		httpUpstream:  httpUpstream,
		stopCh:        make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}
}

func (s *CodexTurnStateProbeService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.run()
	})
}

func (s *CodexTurnStateProbeService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *CodexTurnStateProbeService) run() {
	defer s.wg.Done()
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// The first scan is immediate so enabling the setting does not wait for a
	// full scheduler interval after process startup.
	s.refreshOnce(ctx)
	ticker := time.NewTicker(codexTurnStateProbeRefreshPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.refreshOnce(ctx)
		case <-s.stopCh:
			return
		}
	}
}

func (s *CodexTurnStateProbeService) refreshOnce(ctx context.Context) {
	if s == nil || s.accountRepo == nil || s.proxyRepo == nil || s.httpUpstream == nil {
		return
	}
	accounts, err := s.accountRepo.ListActive(ctx)
	if err != nil {
		slog.Warn("codex_turn_state_probe_list_accounts_failed", "error", err)
		return
	}
	for i := range accounts {
		account := &accounts[i]
		if !IsCodexTurnStateProbeAccount(account) {
			continue
		}
		config, configErr := CodexTurnStateProbeConfigFromExtra(account.Extra)
		if configErr != nil || !config.Enabled {
			continue
		}
		if err := s.refreshAccount(ctx, account, config); err != nil {
			slog.Warn("codex_turn_state_probe_refresh_failed", "account_id", account.ID, "error", err)
		}
	}
}

func (s *CodexTurnStateProbeService) refreshAccount(ctx context.Context, account *Account, config CodexTurnStateProbeConfig) error {
	if account == nil || account.ID <= 0 {
		return errors.New("invalid account")
	}
	lock := s.accountLock(account.ID)
	lock.Lock()
	defer lock.Unlock()

	now := time.Now()
	models := CodexTurnStateProbeModels(account)
	oldCache := codexTurnStateCacheFromExtra(account.Extra)
	oldFailures := codexTurnStateProbeFailuresFromExtra(account.Extra)
	cache := make(map[string]codexTurnStateCacheEntry, len(oldCache))
	failures := make(map[string]codexTurnStateProbeFailure, len(oldFailures))
	modelSet := make(map[string]struct{}, len(models))
	for _, model := range models {
		modelSet[model] = struct{}{}
	}
	for model, failure := range oldFailures {
		if model != "" && failure.Attempts <= codexTurnStateProbeMaxAttempts {
			if _, ok := modelSet[model]; !ok {
				continue
			}
			failures[model] = failure
		}
	}
	for model, entry := range oldCache {
		modelKey := codexTurnStateModelKey(account, model)
		if modelKey == "" || !codexTurnStateConfigContainsModel(account, models, model, modelKey) || !validCodexTurnStateCacheMetadata(entry, config.ProxyID, now) {
			continue
		}
		cache[modelKey] = entry
	}
	persistCache := func() error {
		if reflect.DeepEqual(oldCache, cache) {
			return nil
		}
		encoded := codexTurnStateCacheToExtra(cache)
		var cacheUpdate any
		if encoded != nil {
			cacheUpdate = encoded
		}
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{CodexTurnStateProbeCacheExtraKey: cacheUpdate}); err != nil {
			return err
		}
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		if encoded == nil {
			delete(account.Extra, CodexTurnStateProbeCacheExtraKey)
		} else {
			account.Extra[CodexTurnStateProbeCacheExtraKey] = encoded
		}
		return nil
	}
	persistFailures := func() error {
		if reflect.DeepEqual(oldFailures, failures) {
			return nil
		}
		encoded := codexTurnStateProbeFailuresToExtra(failures)
		var failureUpdate any
		if encoded != nil {
			failureUpdate = encoded
		}
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{CodexTurnStateProbeFailureExtraKey: failureUpdate}); err != nil {
			return err
		}
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		if encoded == nil {
			delete(account.Extra, CodexTurnStateProbeFailureExtraKey)
		} else {
			account.Extra[CodexTurnStateProbeFailureExtraKey] = encoded
		}
		return nil
	}
	if len(models) == 0 {
		if err := persistCache(); err != nil {
			return err
		}
		return persistFailures()
	}

	proxy, err := s.proxyRepo.GetByID(ctx, config.ProxyID)
	if err != nil {
		if persistErr := persistCache(); persistErr != nil {
			slog.Warn("codex_turn_state_probe_cache_cleanup_failed", "account_id", account.ID, "error", persistErr)
		}
		return err
	}
	if proxy == nil || !proxy.IsActive() || proxy.IsExpired(now) {
		if persistErr := persistCache(); persistErr != nil {
			slog.Warn("codex_turn_state_probe_cache_cleanup_failed", "account_id", account.ID, "error", persistErr)
		}
		return fmt.Errorf("probe proxy %d is unavailable", config.ProxyID)
	}
	token, err := s.probeToken(ctx, account)
	if err != nil {
		// Keep structurally valid, unexpired entries when the token provider is
		// temporarily unavailable. The request path still verifies the hash
		// against the actual token, and the next scan can refresh the cache.
		if persistErr := persistCache(); persistErr != nil {
			slog.Warn("codex_turn_state_probe_cache_cleanup_failed", "account_id", account.ID, "error", persistErr)
		}
		return err
	}
	for modelKey, entry := range cache {
		if !validCodexTurnStateCacheEntry(entry, config.ProxyID, token, now) {
			delete(cache, modelKey)
		}
	}
	for _, model := range models {
		modelKey := codexTurnStateModelKey(account, model)
		if modelKey == "" {
			continue
		}
		failure := failures[modelKey]
		remainingAttempts := codexTurnStateProbeMaxAttempts - failure.Attempts
		if remainingAttempts <= 0 {
			continue
		}
		if entry, ok := cache[modelKey]; ok && validCodexTurnStateCacheEntry(entry, config.ProxyID, token, now) &&
			!codexTurnStateCacheNeedsRefresh(entry, now) {
			continue
		}
		state, obtainedAt, probeErr := s.probeModel(ctx, account, proxy, token, modelKey, remainingAttempts)
		if probeErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures[modelKey] = codexTurnStateProbeFailure{
				Attempts: failure.Attempts + remainingAttempts,
				FailedAt: time.Now().UTC(),
			}
			slog.Warn("codex_turn_state_probe_model_failed", "account_id", account.ID, "model", modelKey, "error", probeErr)
			continue
		}
		delete(failures, modelKey)
		cache[modelKey] = codexTurnStateCacheEntry{
			State:          state,
			ObtainedAt:     obtainedAt,
			ExpiresAt:      obtainedAt.Add(codexTurnStateTTL),
			ProxyID:        config.ProxyID,
			CredentialHash: codexTurnStateCredentialHash(token),
		}
	}

	if err := persistCache(); err != nil {
		return err
	}
	return persistFailures()
}

func (s *CodexTurnStateProbeService) accountLock(accountID int64) *sync.Mutex {
	value, _ := s.accountMu.LoadOrStore(accountID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (s *CodexTurnStateProbeService) probeToken(ctx context.Context, account *Account) (string, error) {
	if account.Type == AccountTypeOAuth {
		if s.tokenProvider == nil {
			return "", errors.New("OpenAI token provider is unavailable")
		}
		return s.tokenProvider.GetAccessToken(ctx, account)
	}
	token := strings.TrimSpace(account.GetOpenAIAccessToken())
	if token == "" {
		return "", errors.New("account has no access token")
	}
	return token, nil
}

func (s *CodexTurnStateProbeService) probeModel(
	ctx context.Context,
	account *Account,
	proxy *Proxy,
	token string,
	model string,
	maxAttempts int,
) (string, time.Time, error) {
	if maxAttempts <= 0 {
		return "", time.Time{}, errors.New("probe attempt budget is exhausted")
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", time.Time{}, err
		}
		state, obtainedAt, err := s.probeOnce(ctx, account, proxy, token, model)
		if err == nil {
			slog.Info("codex_turn_state_probe_succeeded", "account_id", account.ID, "model", model, "attempt", attempt, "state_length", len(state), "expires_at", obtainedAt.Add(codexTurnStateTTL).UTC().Format(time.RFC3339))
			return state, obtainedAt, nil
		}
		lastErr = err
		slog.Debug("codex_turn_state_probe_attempt_failed", "account_id", account.ID, "model", model, "attempt", attempt, "max_attempts", maxAttempts, "error", err)
	}
	return "", time.Time{}, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

func (s *CodexTurnStateProbeService) probeOnce(
	parentCtx context.Context,
	account *Account,
	proxy *Proxy,
	token string,
	model string,
) (string, time.Time, error) {
	if account == nil || proxy == nil {
		return "", time.Time{}, errors.New("probe account or proxy is nil")
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentCtx, codexTurnStateProbeTimeout)
	defer cancel()
	payload := createOpenAITestPayload(model, true)
	body, err := json.Marshal(payload)
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, strings.NewReader(string(body)))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	ensureCodexIdentityHeaders(req.Header)
	applyOpenAICodexProbeHeaders(req.Header)
	setOpenAIChatGPTAccountHeaders(req.Header, account)
	if customUA := strings.TrimSpace(account.GetOpenAIUserAgent()); customUA != "" {
		enforceCodexIdentityHeadersWithUA(req.Header, customUA)
	}
	account.ApplyHeaderOverrides(req.Header)
	// Candidate acquisition is intentionally unseeded. A manually configured
	// header override must not turn this request into a validation of an older
	// or unrelated state value. Keep the probe's auth and SSE negotiation
	// authoritative as well.
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Del(openAICodexTurnStateHeader)
	req.Close = true

	resp, err := s.httpUpstream.DoWithTLS(req, proxy.URL(), account.ID, account.Concurrency, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	if resp == nil {
		return "", time.Time{}, errors.New("probe returned a nil response")
	}
	// The probe deliberately never reads the SSE body. Closing it immediately
	// after headers arrive cancels the upstream stream and prevents completion
	// tokens from being consumed.
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	state := resp.Header.Get(openAICodexTurnStateHeader)
	if len(state) != codexTurnStateLength {
		return "", time.Time{}, fmt.Errorf("upstream state length %d, want %d", len(state), codexTurnStateLength)
	}
	return state, time.Now().UTC(), nil
}
