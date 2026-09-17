package config

import (
	"fmt"
	"strconv"
	"strings"
)

// parsePinModel parses PIN_MODEL: semicolon/newline separated slot entries,
// each exactly "<slot-index>:<model>", e.g.
// "0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash". Slot indexes address
// AUTH_TOKENS positions; slots without an entry are unpinned and serve any
// model. Empty input yields nil (feature off).
//
// Strict single-pin: a slot takes exactly one model. Comma lists, extra
// colons and duplicate slots are an error — a silently-ignored pin would
// route quota to the wrong account.
func parsePinModel(value string) (map[int]string, error) {
	pins := make(map[int]string)
	entries := strings.FieldsFunc(value, func(r rune) bool {
		return r == ';' || r == '\n' || r == '\r'
	})
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		parts := strings.Split(e, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid PIN_MODEL entry %q (want <slot>:<model>)", e)
		}
		idx, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || idx < 0 {
			return nil, fmt.Errorf("invalid PIN_MODEL slot %q (want non-negative index)", strings.TrimSpace(parts[0]))
		}
		model := strings.TrimSpace(parts[1])
		if model == "" {
			return nil, fmt.Errorf("invalid PIN_MODEL entry %q (no model listed)", e)
		}
		if strings.Contains(model, ",") {
			return nil, fmt.Errorf("invalid PIN_MODEL entry %q (one model per slot, no comma lists)", e)
		}
		if _, dup := pins[idx]; dup {
			return nil, fmt.Errorf("invalid PIN_MODEL entry %q (slot %d pinned twice)", e, idx)
		}
		pins[idx] = model
	}
	if len(pins) == 0 {
		return nil, nil
	}
	return pins, nil
}
