package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

var errCodexTurnStateMissing = errors.New("upstream did not return turn-state")

func codexTurnStateFromMetadata(payload []byte) string {
	if !codexTurnStateMetadataEvent(gjson.GetBytes(payload, "type").String()) {
		return ""
	}
	state := ""
	gjson.GetBytes(payload, "headers").ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.String(), openAICodexTurnStateHeader) && value.Type == gjson.String {
			state = value.String()
			return false
		}
		return true
	})
	return state
}

// Each attempt dials a fresh connection, never the business WS pool. Its entire
// dial/write/read lifetime shares one timeout and one slot of the model budget.
func (s *CodexTurnStateProbeService) probeWSOnce(parent context.Context, account *Account, proxy *Proxy, token, model string) (string, time.Time, error) {
	if account == nil || proxy == nil || s.wsDialer == nil {
		return "", time.Time{}, errors.New("ws probe dependencies are unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, codexTurnStateProbeTimeout)
	defer cancel()
	headers := make(http.Header)
	ensureCodexIdentityHeaders(headers)
	applyOpenAICodexProbeHeaders(headers)
	setOpenAIChatGPTAccountHeaders(headers, account)
	if customUA := strings.TrimSpace(account.GetOpenAIUserAgent()); customUA != "" {
		enforceCodexIdentityHeadersWithUA(headers, customUA)
	}
	account.ApplyHeaderOverrides(headers)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("OpenAI-Beta", openAIWSBetaV2Value)
	sessionID := uuid.NewString()
	headers.Set("session-id", sessionID)
	headers.Del(openAICodexTurnStateHeader)
	conn, _, _, err := s.wsDialer.Dial(ctx, strings.Replace(chatgptCodexURL, "https://", "wss://", 1), headers, proxy.URL())
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() {
		if force, ok := conn.(openAIWSForceCloser); ok {
			_ = force.CloseNow()
		} else {
			_ = conn.Close()
		}
	}()
	payload := createOpenAITestPayload(model, true)
	payload["type"] = "response.create"
	payload["client_metadata"] = map[string]string{"session_id": sessionID, "thread_id": sessionID, codexWSStreamRequestStartKey: fmt.Sprint(time.Now().UnixMilli())}
	if err := conn.WriteJSON(ctx, payload); err != nil {
		return "", time.Time{}, err
	}
	for {
		message, err := conn.ReadMessage(ctx)
		if err != nil {
			return "", time.Time{}, err
		}
		eventType := gjson.GetBytes(message, "type").String()
		if eventType == "response.created" && model != "gpt-5.6-luna" && gjson.GetBytes(message, "response.model").String() == "gpt-5.6-luna" {
			return "", time.Time{}, errors.New("probe returned a downgraded model")
		}
		if state := codexTurnStateFromMetadata(message); state != "" {
			expected := CodexTurnStateHealthyLength(account)
			if expected == 0 || len(state) != expected {
				return "", time.Time{}, fmt.Errorf("upstream state length %d, want %d", len(state), expected)
			}
			return state, time.Now().UTC(), nil
		}
		if eventType == "error" || isOpenAIWSTerminalEvent(eventType) {
			return "", time.Time{}, errCodexTurnStateMissing
		}
	}
}
