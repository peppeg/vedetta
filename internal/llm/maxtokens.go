package llm

import (
	"context"
	"log"
)

// Output budget. A server that gets no max_tokens applies its own default,
// which can be far below what the model can write (halogen: 8192 of 65536):
// a long reasoning then eats the room and the answer is cut. Vedetta never
// sets a ceiling of its own: it asks for the most the server allows. The only
// cuts are the repetition and stall watchdogs in the agent.

// WithMaxTokens sets the max_tokens sent with every request: n > 0 sends n,
// 0 asks the server's /models for the model's output cap (sent when found,
// omitted otherwise), n < 0 never sends the field. A client from NewClient
// sends no max_tokens until this is called.
func (c *Client) WithMaxTokens(n int) *Client {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	c.maxTokens = n
	c.omitMaxTokens = false
	return c
}

// selectMaxTokens returns the max_tokens to send, 0 to omit the field.
func (c *Client) selectMaxTokens(ctx context.Context) int {
	c.capMu.Lock()
	if c.omitMaxTokens || c.maxTokens < 0 {
		c.capMu.Unlock()
		return 0
	}
	if c.maxTokens > 0 {
		n := c.maxTokens
		c.capMu.Unlock()
		return n
	}
	c.capMu.Unlock()

	// What /models states is shared by every client for limitsTTL (context.go).
	lim, final, err := c.limits(ctx)
	if err != nil && !final {
		// Server down or busy: ask again on the next call, not settle on its default.
		log.Printf("[llm] model=%s output cap not known yet (%v): this call leaves max_tokens to the server", c.model, err)
		return 0
	}
	return lim.OutputCap
}

// rejectMaxTokens records that the server refused max_tokens: it is omitted
// from then on.
func (c *Client) rejectMaxTokens() {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	c.omitMaxTokens = true
}

// ModelOutputCap asks /models for the most tokens the configured model may
// write in one response. Only fields that state an output cap are read; a
// context length is not one (prompt and answer share it). 0 means not stated.
func (c *Client) ModelOutputCap(ctx context.Context) (int, error) {
	lim, _, err := c.limits(ctx)
	return lim.OutputCap, err
}

// parseOutputCap finds model in a /models answer and returns its output cap.
func parseOutputCap(body []byte, model string) int {
	return parseModelLimits(body, model).OutputCap
}
