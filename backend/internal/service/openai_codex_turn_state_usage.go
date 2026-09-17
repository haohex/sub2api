package service

import "github.com/gin-gonic/gin"

type CodexStateUsageLengths struct{ Sent, Returned *int }

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
	return result
}
func resetCodexStateUsageLengths(c *gin.Context, sent int) {
	if c != nil {
		c.Set("codex_state_sent_length", sent)
		c.Set("codex_state_returned_length", 0)
	}
}
