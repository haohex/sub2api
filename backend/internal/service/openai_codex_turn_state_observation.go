package service

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const codexTurnStateObservationLimit = 1 << 20

type codexTurnStateRequestKey struct{}

type codexTurnStateRequest struct {
	accountID int64
	model     string
	entry     codexTurnStateCacheEntry
}

// Call at the final request boundary, with the already mapped upstream model.
// The context snapshot identifies the exact candidate this attempt sent.
func applyConfiguredCodexTurnStateToRequest(account *Account, req *http.Request, model, token string) {
	if req == nil || req.Header == nil {
		return
	}
	state := configuredCodexTurnState(account, model, token, time.Now())
	if state == "" {
		return
	}
	req.Header.Set(openAICodexTurnStateHeader, state)
	candidate := codexTurnStateCandidateSent(account, model, state)
	if candidate != nil {
		*req = *req.WithContext(context.WithValue(req.Context(), codexTurnStateRequestKey{}, candidate))
	}
}

func codexTurnStateCandidateSent(account *Account, model, state string) *codexTurnStateRequest {
	if !IsCodexTurnStateProbeAccount(account) || state == "" {
		return nil
	}
	config, err := CodexTurnStateProbeConfigFromExtra(account.Extra)
	if err != nil || !config.Enabled {
		return nil
	}
	model = codexTurnStateModelKey(account, model)
	if !containsString(CodexTurnStateProbeModels(account), model) {
		return nil
	}
	entry, ok := codexTurnStateCacheFromExtra(account.Extra)[model]
	if !ok || entry.State != state || !validCodexTurnStateCacheMetadata(entry, config.ProxyID, time.Now()) {
		return nil
	}
	return &codexTurnStateRequest{accountID: account.ID, model: model, entry: entry}
}

type codexTurnStateObservation struct {
	once       sync.Once
	invalidate func(string)
}

func newCodexTurnStateObservation(ctx context.Context, repo AccountRepository, candidate *codexTurnStateRequest, wake func()) *codexTurnStateObservation {
	if repo == nil || candidate == nil {
		return nil
	}
	accountID := candidate.accountID
	return &codexTurnStateObservation{invalidate: func(reason string) {
		// The client may disconnect on a failure frame. Persist the observation with
		// a bounded, detached context so that cancellation cannot revive bad state.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		for range 3 {
			current, err := repo.GetByID(saveCtx, accountID)
			if err != nil {
				slog.Warn("codex_turn_state_observation_read_failed", "account_id", accountID, "error", err)
				return
			}
			config, err := CodexTurnStateProbeConfigFromExtra(current.Extra)
			if err != nil || !config.Enabled || !IsCodexTurnStateProbeAccount(current) {
				return
			}
			cache := codexTurnStateCacheFromExtra(current.Extra)
			entry, ok := cache[candidate.model]
			if !ok || entry != candidate.entry || entry.ProxyID != config.ProxyID {
				return
			}
			delete(cache, candidate.model)
			failures := codexTurnStateProbeFailuresFromExtra(current.Extra)
			delete(failures, candidate.model)
			saved, err := saveCodexTurnStateProbeRuntime(saveCtx, repo, current, codexTurnStateProbeRuntimeUpdates(cache, failures))
			if err != nil {
				slog.Warn("codex_turn_state_observation_save_failed", "account_id", accountID, "error", err)
				return
			}
			if saved {
				slog.Info("codex_turn_state_invalidated", "account_id", accountID, "model", candidate.model, "reason", reason)
				if wake != nil {
					wake()
				}
				return
			}
		}
		slog.Warn("codex_turn_state_observation_conflict", "account_id", accountID, "model", candidate.model)
	}}
}

func (o *codexTurnStateObservation) observeReason(reason string) {
	if o != nil && reason != "" {
		o.once.Do(func() { o.invalidate(reason) })
	}
}

func (o *codexTurnStateObservation) observeHeader(state string) {
	if len(state) == 312 {
		o.observeReason("turn_state_length_312")
	}
}

func (o *codexTurnStateObservation) observeEvent(payload []byte, eventType string) {
	if o == nil {
		return
	}
	if eventType == "" {
		eventType = gjson.GetBytes(payload, "type").String()
	}
	if eventType == "response.created" && gjson.GetBytes(payload, "response.model").String() == "gpt-5.6-luna" {
		o.observeReason("response_created_gpt_5_6_luna")
		return
	}
	// Inspect structured error fields only. A user/tool/model quoting this code
	// in ordinary output is not evidence of an upstream overload.
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type"} {
		if gjson.GetBytes(payload, path).String() == "server_is_overloaded" {
			o.observeReason("server_is_overloaded")
			return
		}
	}
	if eventType == "server_is_overloaded" || (eventType == "error" && gjson.GetBytes(payload, "code").String() == "server_is_overloaded") {
		o.observeReason("server_is_overloaded")
		return
	}
	if eventType == "response.metadata" {
		headers := gjson.GetBytes(payload, "headers")
		headers.ForEach(func(key, value gjson.Result) bool {
			if strings.EqualFold(key.String(), openAICodexTurnStateHeader) && value.Type == gjson.String {
				o.observeHeader(value.String())
			}
			return true
		})
	}
}

func observeCodexTurnStateHTTPResponse(req *http.Request, resp *http.Response, account *Account, repo AccountRepository, wake func()) {
	if req == nil || resp == nil || account == nil {
		return
	}
	candidate, _ := req.Context().Value(codexTurnStateRequestKey{}).(*codexTurnStateRequest)
	// Plugins may have replaced an outbound header. Do not blame a candidate
	// unless that candidate was still on the actual request.
	if candidate == nil || req.Header.Get(openAICodexTurnStateHeader) != candidate.entry.State {
		return
	}
	observer := newCodexTurnStateObservation(req.Context(), repo, candidate, wake)
	if observer == nil {
		return
	}
	observer.observeHeader(resp.Header.Get(openAICodexTurnStateHeader))
	if resp.Body != nil {
		resp.Body = &codexTurnStateObservedBody{ReadCloser: resp.Body, observer: observer, sse: strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")}
	}
}

// This wrapper observes bytes as the existing gateway reads them. It does not
// consume ahead, rewrite output, or wait for a complete stream. Large events
// are skipped with bounded memory; subsequent events are still observed.
type codexTurnStateObservedBody struct {
	io.ReadCloser
	observer  *codexTurnStateObservation
	sse       bool
	line      []byte
	data      []byte
	eventType string
	skipLine  bool
	skipEvent bool
}

func (r *codexTurnStateObservedBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if r.sse {
		r.feed(p[:n])
	} else if !r.skipEvent {
		if len(r.data)+n > codexTurnStateObservationLimit {
			r.data = nil
			r.skipEvent = true
		} else {
			r.data = append(r.data, p[:n]...)
		}
	}
	if err == io.EOF {
		if r.sse {
			if len(r.line) > 0 && !r.skipLine {
				r.processLine(r.line)
				r.line = nil
			}
			r.flushEvent()
		} else if !r.skipEvent {
			r.observer.observeEvent(r.data, "")
			r.data = nil
		}
	}
	return n, err
}

func (r *codexTurnStateObservedBody) feed(p []byte) {
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		part := p
		if end >= 0 {
			part = p[:end]
		}
		if !r.skipLine {
			if len(r.line)+len(part) > codexTurnStateObservationLimit {
				r.line = nil
				r.skipLine = true
				r.skipEvent = true
			} else {
				r.line = append(r.line, part...)
			}
		}
		if end < 0 {
			return
		}
		if !r.skipLine {
			r.processLine(r.line)
		}
		r.line = r.line[:0]
		r.skipLine = false
		p = p[end+1:]
	}
}

func (r *codexTurnStateObservedBody) processLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) == 0 {
		r.flushEvent()
		return
	}
	if bytes.HasPrefix(line, []byte("event:")) {
		r.eventType = strings.TrimSpace(string(line[6:]))
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) || r.skipEvent {
		return
	}
	value := bytes.TrimPrefix(line[5:], []byte{' '})
	if len(r.data)+len(value)+1 > codexTurnStateObservationLimit {
		r.data = nil
		r.skipEvent = true
		return
	}
	r.data = append(r.data, value...)
	r.data = append(r.data, '\n')
}

func (r *codexTurnStateObservedBody) flushEvent() {
	if !r.skipEvent && len(r.data) > 0 {
		r.observer.observeEvent(r.data, r.eventType)
	}
	r.data = r.data[:0]
	r.eventType = ""
	r.skipEvent = false
}

func (s *OpenAIGatewayService) wakeCodexTurnStateProbe() {
	if s != nil && s.codexTurnStateProbe != nil {
		s.codexTurnStateProbe.Wake()
	}
}

func codexTurnStateProbeEnabled(account *Account) bool {
	if !IsCodexTurnStateProbeAccount(account) {
		return false
	}
	config, err := CodexTurnStateProbeConfigFromExtra(account.Extra)
	return err == nil && config.Enabled
}

// A WS connection can outlive several renewals. Refresh just the candidate
// settings at each send boundary, keeping the connection's identity/profile.
func (s *OpenAIGatewayService) prepareCodexTurnStateWSFrame(ctx context.Context, c *gin.Context, account *Account, payload []byte, turnState, token string, headers http.Header) ([]byte, *codexTurnStateObservation) {
	candidateAccount := account
	if codexTurnStateProbeEnabled(account) && s.accountRepo != nil {
		fresh, err := s.accountRepo.GetByID(ctx, account.ID)
		if err != nil {
			slog.Warn("codex_turn_state_ws_refresh_failed", "account_id", account.ID, "error", err)
		} else if fresh != nil {
			copyAccount := *account
			copyAccount.Extra = make(map[string]any, len(account.Extra))
			for key, value := range account.Extra {
				copyAccount.Extra[key] = value
			}
			for _, key := range []string{CodexTurnStateProbeEnabledExtraKey, CodexTurnStateProbeProxyIDExtraKey, CodexTurnStateProbeCacheExtraKey} {
				delete(copyAccount.Extra, key)
				if value, ok := fresh.Extra[key]; ok {
					copyAccount.Extra[key] = value
				}
			}
			candidateAccount = &copyAccount
		}
	}
	payload = applyCodexWSFrameWireProfile(c, candidateAccount, payload, turnState, token)
	model := gjson.GetBytes(payload, "model").String()
	state := gjson.GetBytes(payload, "client_metadata."+openAICodexTurnStateHeader).String()
	if state == "" {
		state = headers.Get(openAICodexTurnStateHeader)
	}
	candidate := codexTurnStateCandidateSent(candidateAccount, model, state)
	return payload, newCodexTurnStateObservation(ctx, s.accountRepo, candidate, s.wakeCodexTurnStateProbe)
}

// Retries within one forwarding call can retain the original Account snapshot.
// Revalidate a managed header immediately before transport so a state observed
// as invalid on the previous attempt is not sent again from that old snapshot.
func refreshCodexTurnStateHTTPRequest(req *http.Request, repo AccountRepository) {
	if req == nil || repo == nil {
		return
	}
	previous, _ := req.Context().Value(codexTurnStateRequestKey{}).(*codexTurnStateRequest)
	if previous == nil || req.Header.Get(openAICodexTurnStateHeader) != previous.entry.State {
		return
	}
	current, err := repo.GetByID(req.Context(), previous.accountID)
	if err != nil {
		slog.Warn("codex_turn_state_http_refresh_failed", "account_id", previous.accountID, "error", err)
		return
	}
	entry := codexTurnStateCacheFromExtra(current.Extra)[previous.model]
	next := codexTurnStateCandidateSent(current, previous.model, entry.State)
	if next != nil && next.entry.CredentialHash != previous.entry.CredentialHash {
		next = nil
	}
	if next == nil {
		req.Header.Del(openAICodexTurnStateHeader)
	} else {
		req.Header.Set(openAICodexTurnStateHeader, next.entry.State)
	}
	*req = *req.WithContext(context.WithValue(req.Context(), codexTurnStateRequestKey{}, next))
}
