package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type codexStateSchedule struct {
	Next      time.Time
	Remaining int
	Attempt   int
	Manual    bool
	Mode      string
}

func codexStateMode(entry codexTurnStateCacheEntry, expected int, token string, now time.Time) string {
	if !validCodexTurnStateCacheEntry(entry, expected, token, now) {
		return "continuous"
	}
	remaining := codexStateEffectiveExpiry(entry, now).Sub(now)
	if remaining <= 10*time.Minute {
		return "continuous"
	}
	if remaining <= 30*time.Minute {
		return "periodic"
	}
	return "idle"
}
func (s *CodexTurnStateProbeService) reserveStateAccount(id int64) bool {
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	if s.active == nil {
		s.active = map[int64]bool{}
	}
	if s.active[id] || len(s.active) >= 4 {
		return false
	}
	s.active[id] = true
	return true
}
func (s *CodexTurnStateProbeService) releaseStateAccount(id int64) {
	s.scheduleMu.Lock()
	delete(s.active, id)
	s.scheduleMu.Unlock()
	s.Wake()
}

func (s *CodexTurnStateProbeService) dispatchStateProbes(ctx context.Context) {
	if s.accountRepo == nil || s.proxyRepo == nil || ctx.Err() != nil {
		return
	}
	now := time.Now()
	if now.Sub(s.lastCleanup) > time.Hour {
		if repo, ok := s.accountRepo.(CodexStateEventRepository); ok {
			if repo.CleanupCodexStateEvents(ctx, now.Add(-7*24*time.Hour)) == nil {
				s.lastCleanup = now
			}
		}
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return
	}
	global, err := s.GetGlobalConfig(ctx)
	if err != nil {
		return
	}
	type task struct {
		account Account
		model   string
		due     time.Time
	}
	tasks := []task{}
	for _, account := range accounts {
		if !codexTurnStateProbeEnabled(&account) || CodexTurnStateHealthyLength(&account) == 0 {
			s.pauseStateAccount(account.ID)
			continue
		}
		for _, model := range CodexTurnStateProbeModels(&account) {
			key := codexTurnStatePoolKey{account.ID, model}
			entry := codexTurnStateCacheFromExtra(account.Extra)[model]
			mode := codexStateMode(entry, CodexTurnStateHealthyLength(&account), account.GetOpenAIAccessToken(), now)
			queued := s.takeManualRefresh(account.ID, model)
			s.scheduleMu.Lock()
			if s.schedules == nil {
				s.schedules = map[codexTurnStatePoolKey]*codexStateSchedule{}
			}
			state := s.schedules[key]
			if state == nil {
				state = &codexStateSchedule{Next: now}
				s.schedules[key] = state
			}
			previous := state.Mode
			if queued {
				state.Manual = true
				state.Remaining = 25
				state.Attempt = 0
				state.Next = now
			}
			if mode == "idle" && state.Manual {
				mode = "manual"
			}
			if !global.Available || account.Status != StatusActive {
				mode = "paused"
			}
			if mode == "continuous" && previous != "continuous" {
				state.Next = now
			}
			if previous == "paused" && mode != "paused" {
				state.Next = now
			}
			state.Mode = mode
			if mode == "idle" {
				state.Next = codexStateEffectiveExpiry(entry, now).Add(-30 * time.Minute)
			}
			if mode != "idle" && mode != "paused" && !state.Next.After(now) && !s.active[account.ID] {
				tasks = append(tasks, task{account, model, state.Next})
			}
			s.scheduleMu.Unlock()
			if previous != "" && previous != mode && (previous == "paused" || mode == "paused") {
				recordCodexStateEvent(s.accountRepo, CodexStateEvent{AccountID: account.ID, Model: model, Kind: mode, Reason: mode})
			}
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].due.Before(tasks[j].due) })
	for _, task := range tasks {
		if !s.reserveStateAccount(task.account.ID) {
			continue
		}
		s.wg.Add(1)
		go func(id int64, model string) {
			defer s.wg.Done()
			defer s.releaseStateAccount(id)
			s.executeStateAttempt(ctx, id, model)
		}(task.account.ID, task.model)
	}
}

func (s *CodexTurnStateProbeService) executeStateAttempt(ctx context.Context, id int64, model string) {
	key := codexTurnStatePoolKey{id, model}
	s.scheduleMu.Lock()
	state := s.schedules[key]
	if state == nil {
		s.scheduleMu.Unlock()
		return
	}
	state.Attempt++
	attempt := state.Attempt
	if state.Remaining <= 0 {
		state.Remaining = 25
	}
	manual := state.Manual
	s.scheduleMu.Unlock()
	source := "automatic"
	if manual {
		source = "manual_probe"
	}
	s.setProbeRuntime(id, model, "running", attempt, "")
	outcome := "probe_unavailable"
	replaced := false
	var replacedExpiry time.Time
	defer func() {
		s.scheduleMu.Lock()
		defer s.scheduleMu.Unlock()
		if replaced {
			left := time.Until(replacedExpiry)
			state.Mode = "idle"
			if left <= 10*time.Minute {
				state.Mode = "continuous"
			} else if left <= 30*time.Minute {
				state.Mode = "periodic"
			}
		}
		advanceCodexStateSchedule(state, replaced, time.Now())
		s.setProbeRuntime(id, model, "idle", attempt, outcome)
	}()
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return
	}
	if !codexTurnStateProbeEnabled(account) || account.Status != StatusActive || !containsString(CodexTurnStateProbeModels(account), model) {
		return
	}
	global, err := s.GetGlobalConfig(ctx)
	if err != nil || !global.Available {
		return
	}
	proxy, err := s.proxyRepo.GetByID(ctx, global.ProxyID)
	if err != nil || proxy == nil {
		return
	}
	token, err := s.probeToken(ctx, account)
	if err != nil {
		recordCodexStateEvent(s.accountRepo, CodexStateEvent{AccountID: id, Model: model, Kind: "probe_result", Reason: "credentials_unavailable", Source: source})
		return
	}
	event := CodexStateEvent{AccountID: id, Model: model, Source: source, Attempt: attempt, Kind: "probe_started", ProxyID: proxy.ID, ProxyName: proxy.Name, ProxyAddress: fmt.Sprintf("%s://%s:%d", proxy.Protocol, proxy.Host, proxy.Port)}
	recordCodexStateEvent(s.accountRepo, event)
	start := time.Now()
	result := s.acquireState(ctx, account, proxy, token, model, "")
	event.Kind = "probe_result"
	event.HTTPStatus = result.status
	event.ReturnedLength = result.returnedLength
	event.DurationMS = time.Since(start).Milliseconds()
	if result.err != nil {
		outcome = result.err.Error()
	} else {
		replaced, outcome = s.replaceState(ctx, account, model, token, result.state, result.obtained, proxy.ID, source)
		_, expires, _ := codexStateTimes(result.state, result.obtained, time.Now())
		event.ExpiresAt = &expires
		replacedExpiry = expires
	}
	event.Reason = outcome
	s.referenceIP(ctx, proxy, &event)
	recordCodexStateEvent(s.accountRepo, event)
}

func (s *CodexTurnStateProbeService) replaceState(ctx context.Context, expected *Account, model, token, state string, obtained time.Time, proxyID int64, source string) (bool, string) {
	_, expires, err := codexStateTimes(state, obtained, time.Now())
	if err != nil {
		return false, err.Error()
	}
	for range 3 {
		current, err := s.accountRepo.GetByID(ctx, expected.ID)
		if err != nil {
			return false, "account_unavailable"
		}
		if !codexTurnStateProbeEnabled(current) || current.Status != StatusActive || codexTurnStateOwnerIdentity(current) != codexTurnStateOwnerIdentity(expected) || !containsString(CodexTurnStateProbeModels(current), model) || CodexTurnStateHealthyLength(current) != len(state) {
			return false, "account_changed"
		}
		cache := codexTurnStateCacheFromExtra(current.Extra)
		old := cache[model]
		if old.State == state {
			return false, "same_state"
		}
		if validCodexTurnStateCacheEntry(old, CodexTurnStateHealthyLength(current), token, time.Now()) && !expires.After(codexStateEffectiveExpiry(old, time.Now())) {
			return false, "not_newer"
		}
		cache[model] = codexTurnStateCacheEntry{State: state, ObtainedAt: obtained, ExpiresAt: expires, ProxyID: proxyID, CredentialHash: codexTurnStateCredentialHash(token), Source: source}
		failures := codexTurnStateProbeFailuresFromExtra(current.Extra)
		delete(failures, model)
		saved, err := saveCodexTurnStateProbeRuntime(ctx, s.accountRepo, current, codexTurnStateProbeRuntimeUpdates(cache, failures))
		if err != nil {
			return false, "save_failed"
		}
		if saved {
			return true, "replaced"
		}
	}
	return false, "concurrent_update"
}

func (s *CodexTurnStateProbeService) ImportState(ctx context.Context, id int64, model, state string) (string, error) {
	state = strings.TrimSpace(state)
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	if !codexTurnStateProbeEnabled(account) || account.Status != StatusActive || !containsString(CodexTurnStateProbeModels(account), model) || CodexTurnStateHealthyLength(account) == 0 {
		return "", errors.New("account_not_eligible")
	}
	if !s.reserveStateAccount(id) {
		return "", errors.New("account_busy")
	}
	defer s.releaseStateAccount(id)
	event := CodexStateEvent{AccountID: id, Model: model, Kind: "import_result", Source: "manual_import", SentLength: len(state)}
	defer func() { recordCodexStateEvent(s.accountRepo, event) }()
	reject := func(reason string) (string, error) { event.Reason = reason; return "", errors.New(reason) }
	now := time.Now().UTC()
	if len(state) != CodexTurnStateHealthyLength(account) {
		return reject("unexpected_state_length")
	}
	_, _, err = codexStateTimes(state, now, now)
	if err != nil {
		return reject(err.Error())
	}
	global, err := s.GetGlobalConfig(ctx)
	if err != nil || !global.Available {
		return reject("proxy_unavailable")
	}
	proxy, err := s.proxyRepo.GetByID(ctx, global.ProxyID)
	if err != nil || proxy == nil {
		return reject("proxy_unavailable")
	}
	token, err := s.probeToken(ctx, account)
	if err != nil {
		return reject("credentials_unavailable")
	}
	event.Attempt = 1
	_, expires, _ := codexStateTimes(state, now, now)
	event.ExpiresAt = &expires
	event.ProxyID = proxy.ID
	event.ProxyName = proxy.Name
	event.ProxyAddress = fmt.Sprintf("%s://%s:%d", proxy.Protocol, proxy.Host, proxy.Port)
	started := event
	started.Kind = "import_started"
	recordCodexStateEvent(s.accountRepo, started)
	start := time.Now()
	result := s.acquireState(ctx, account, proxy, token, model, state)
	event.HTTPStatus = result.status
	event.ReturnedLength = result.returnedLength
	event.DurationMS = time.Since(start).Milliseconds()
	if result.err != nil {
		event.Reason = result.err.Error()
	} else {
		_, event.Reason = s.replaceState(ctx, account, model, token, state, now, proxy.ID, "manual_import")
	}
	s.referenceIP(ctx, proxy, &event)
	if event.Reason != "replaced" {
		return "", errors.New(event.Reason)
	}
	s.scheduleMu.Lock()
	delete(s.schedules, codexTurnStatePoolKey{id, model})
	s.scheduleMu.Unlock()
	s.setProbeRuntime(id, model, "idle", 0, "")
	return event.Reason, nil
}

func advanceCodexStateSchedule(state *codexStateSchedule, replaced bool, now time.Time) {
	state.Next = now.Add(time.Second)
	if replaced {
		state.Manual = false
		state.Remaining = 0
		state.Attempt = 0
		switch state.Mode {
		case "periodic":
			state.Next = now.Add(5 * time.Minute)
		case "idle":
			state.Next = now
		}
		return
	}
	if state.Mode != "continuous" {
		state.Remaining--
		if state.Remaining <= 0 {
			state.Next = now.Add(5 * time.Minute)
			state.Manual = false
			state.Attempt = 0
		}
	}
}

func (s *CodexTurnStateProbeService) pauseStateAccount(id int64) {
	events := []CodexStateEvent{}
	s.scheduleMu.Lock()
	for key, state := range s.schedules {
		if key.accountID == id && state.Mode != "paused" {
			state.Mode = "paused"
			events = append(events, CodexStateEvent{AccountID: id, Model: key.model, Kind: "paused", Reason: "account_or_plan_unavailable"})
		}
	}
	s.scheduleMu.Unlock()
	for _, event := range events {
		recordCodexStateEvent(s.accountRepo, event)
	}
}
