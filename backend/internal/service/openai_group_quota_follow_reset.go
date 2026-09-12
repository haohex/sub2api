package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const openAIGroupQuotaFollowResetPollInterval = 5 * time.Second

type GroupQuotaFollowResetResult struct {
	EventID               int64
	GroupID               int64
	UserIDs               []int64
	AffectedSubscriptions int
	Skipped               bool
}

type OpenAIGroupQuotaFollowResetRepository interface {
	ObserveWeeklyReset(ctx context.Context, accountID int64, resetAt, observedAt time.Time) (int, error)
	ProcessNextPending(ctx context.Context) (*GroupQuotaFollowResetResult, error)
}

// OpenAIGroupQuotaFollowResetService processes only locally persisted events.
// It never queries upstream quota endpoints; observations come from successful
// gateway traffic through ObserveOpenAIWeeklyResetAt.
type OpenAIGroupQuotaFollowResetService struct {
	repo         OpenAIGroupQuotaFollowResetRepository
	billingCache *BillingCacheService
	ctx          context.Context
	cancel       context.CancelFunc
	wake         chan struct{}
	start        sync.Once
	stop         sync.Once
	wg           sync.WaitGroup
	observedMu   sync.Mutex
	observed     map[int64]time.Time
}

func NewOpenAIGroupQuotaFollowResetService(repo OpenAIGroupQuotaFollowResetRepository, billingCache *BillingCacheService) *OpenAIGroupQuotaFollowResetService {
	ctx, cancel := context.WithCancel(context.Background())
	return &OpenAIGroupQuotaFollowResetService{
		repo: repo, billingCache: billingCache, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1),
	}
}

func (s *OpenAIGroupQuotaFollowResetService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.start.Do(func() {
		setOpenAIGroupQuotaFollowResetObserver(s)
		s.wg.Add(1)
		go s.run()
		s.Notify()
	})
}

func (s *OpenAIGroupQuotaFollowResetService) Stop() {
	if s == nil {
		return
	}
	s.stop.Do(func() {
		clearOpenAIGroupQuotaFollowResetObserver(s)
		s.cancel()
		s.wg.Wait()
	})
}

func (s *OpenAIGroupQuotaFollowResetService) Notify() {
	if s == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *OpenAIGroupQuotaFollowResetService) Observe(ctx context.Context, accountID int64, resetAt time.Time) {
	if s == nil || s.repo == nil || accountID <= 0 || resetAt.IsZero() {
		return
	}
	resetAt = resetAt.UTC()
	// A successful response commonly carries the same absolute weekly reset
	// timestamp for the whole window. Avoid taking the account row lock again
	// until a newer timestamp arrives. The database remains authoritative after
	// process restarts and for observations from other instances.
	s.observedMu.Lock()
	if previous, ok := s.observed[accountID]; ok && !resetAt.After(previous) {
		s.observedMu.Unlock()
		return
	}
	s.observedMu.Unlock()
	// The upstream observation remains valid if the downstream client has
	// disconnected. Persist it synchronously before billing, with a bounded wait.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	created, err := s.repo.ObserveWeeklyReset(ctx, accountID, resetAt, time.Now().UTC())
	if err != nil {
		slog.Warn("openai_group_quota_follow_observe_failed", "account_id", accountID, "reset_at", resetAt, "error", err)
		return
	}
	s.observedMu.Lock()
	if s.observed == nil {
		s.observed = make(map[int64]time.Time)
	}
	if previous, ok := s.observed[accountID]; !ok || resetAt.After(previous) {
		s.observed[accountID] = resetAt
	}
	s.observedMu.Unlock()
	if created > 0 {
		s.Notify()
	}
}

func (s *OpenAIGroupQuotaFollowResetService) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(openAIGroupQuotaFollowResetPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
			s.processPending()
		case <-ticker.C:
			s.processPending()
		}
	}
}

func (s *OpenAIGroupQuotaFollowResetService) processPending() {
	for {
		ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		result, err := s.repo.ProcessNextPending(ctx)
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Warn("openai_group_quota_follow_process_failed", "error", err)
			}
			return
		}
		if result == nil {
			return
		}
		for _, userID := range result.UserIDs {
			if s.billingCache == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.billingCache.InvalidateSubscriptionAndNotify(ctx, userID, result.GroupID)
			cancel()
			if err != nil {
				slog.Warn("openai_group_quota_follow_cache_invalidate_failed", "event_id", result.EventID, "group_id", result.GroupID, "user_id", userID, "error", err)
			}
		}
		if result.Skipped {
			slog.Info("openai_group_quota_follow_skipped", "event_id", result.EventID, "group_id", result.GroupID)
			continue
		}
		slog.Info("openai_group_quota_follow_completed", "event_id", result.EventID, "group_id", result.GroupID, "affected_subscriptions", result.AffectedSubscriptions)
	}
}

var openAIGroupQuotaFollowResetObserver struct {
	sync.RWMutex
	service *OpenAIGroupQuotaFollowResetService
}

func setOpenAIGroupQuotaFollowResetObserver(service *OpenAIGroupQuotaFollowResetService) {
	openAIGroupQuotaFollowResetObserver.Lock()
	openAIGroupQuotaFollowResetObserver.service = service
	openAIGroupQuotaFollowResetObserver.Unlock()
}

func clearOpenAIGroupQuotaFollowResetObserver(service *OpenAIGroupQuotaFollowResetService) {
	openAIGroupQuotaFollowResetObserver.Lock()
	if openAIGroupQuotaFollowResetObserver.service == service {
		openAIGroupQuotaFollowResetObserver.service = nil
	}
	openAIGroupQuotaFollowResetObserver.Unlock()
}

func ObserveOpenAIWeeklyResetAt(ctx context.Context, accountID int64, resetAt time.Time) {
	openAIGroupQuotaFollowResetObserver.RLock()
	service := openAIGroupQuotaFollowResetObserver.service
	openAIGroupQuotaFollowResetObserver.RUnlock()
	if service != nil {
		service.Observe(ctx, accountID, resetAt)
	}
}
