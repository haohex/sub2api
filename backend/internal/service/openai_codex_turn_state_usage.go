package service

import "github.com/gin-gonic/gin"

type CodexStateUsageLengths struct {
	Sent, Returned *int
	SentState      *string
}

func OpenAICodexStateUsageLengths(c *gin.Context) CodexStateUsageLengths {
	var result CodexStateUsageLengths
	if c == nil {
		return result
	}
	if v, ok := c.Get("codex_state_sent_length"); ok {
		if n, ok := v.(int); ok {
			result.Sent = &n
		}
	}
	if v, ok := c.Get("codex_state_returned_length"); ok {
		if n, ok := v.(int); ok {
			result.Returned = &n
		}
	}
	if v, ok := c.Get("codex_state_sent_value"); ok {
		if state, ok := v.(string); ok && state != "" {
			result.SentState = &state
		}
	}
	return result
}
func resetCodexStateUsageLengths(c *gin.Context, sent int) {
	if c != nil {
		c.Set("codex_state_sent_length", sent)
		c.Set("codex_state_returned_length", 0)
	}
}

func setCodexStateUsageSentValue(c *gin.Context, state string) {
	if c != nil {
		if state == "" {
			c.Set("codex_state_sent_value", "")
			return
		}
		if len(state) > maxUsageCodexTurnStateLen {
			state = state[:maxUsageCodexTurnStateLen]
		}
		c.Set("codex_state_sent_value", state)
	}
}
