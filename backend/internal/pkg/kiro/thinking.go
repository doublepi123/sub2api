package kiro

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const (
	thinkingStartTag = "<thinking>"
	thinkingEndTag   = "</thinking>"
	thinkingEndSeq   = "</thinking>\n\n"
)

// thinkingQuoteChars marks characters that wrap a "mentioned" thinking tag
// rather than a real block boundary. Mirrored from kiro.rs QUOTE_CHARS.
var thinkingQuoteChars = func() (out [256]bool) {
	for _, c := range []byte("`\"'\\#!@$%^&*()-_=+[]{};:<>,.?/") {
		out[c] = true
	}
	return out
}()

func generateThinkingPrefix(req apicompat.AnthropicRequest) string {
	if req.Thinking == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(req.Thinking.Type)) {
	case "enabled":
		return "<thinking_mode>enabled</thinking_mode><max_thinking_length>" +
			itoa(req.Thinking.BudgetTokens) + "</max_thinking_length>"
	case "adaptive":
		effort := "high"
		if req.OutputConfig != nil {
			if v := strings.TrimSpace(req.OutputConfig.Effort); v != "" {
				effort = v
			}
		}
		return "<thinking_mode>adaptive</thinking_mode><thinking_effort>" + effort + "</thinking_effort>"
	default:
		return ""
	}
}

func hasThinkingTags(content string) bool {
	return strings.Contains(content, "<thinking_mode>") || strings.Contains(content, "<max_thinking_length>")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

type thinkingEventKind int

const (
	thinkingEventText thinkingEventKind = iota
	thinkingEventStart
	thinkingEventDelta
	thinkingEventStop
)

type thinkingEvent struct {
	kind thinkingEventKind
	text string
}

// thinkingParser splits Kiro's in-band <thinking> tags out of streamed text.
// The state machine matches kiro.rs process_content_with_thinking.
type thinkingParser struct {
	buf            string
	inBlock        bool
	extracted      bool
	stripLeadingNL bool
}

func (p *thinkingParser) push(chunk string) []thinkingEvent {
	if chunk == "" {
		return nil
	}
	p.buf += chunk
	return p.drain(false)
}

// flushBoundary handles tool_use / stream-end where </thinking> may lack \n\n.
func (p *thinkingParser) flushBoundary() []thinkingEvent {
	return p.drain(true)
}

func (p *thinkingParser) drain(boundary bool) []thinkingEvent {
	var events []thinkingEvent
	for {
		switch {
		case !p.inBlock && !p.extracted:
			if start, ok := findRealThinkingStartTag(p.buf); ok {
				before := p.buf[:start]
				if strings.TrimSpace(before) != "" {
					events = append(events, thinkingEvent{kind: thinkingEventText, text: before})
				}
				p.inBlock = true
				p.stripLeadingNL = true
				p.buf = p.buf[start+len(thinkingStartTag):]
				events = append(events, thinkingEvent{kind: thinkingEventStart})
				continue
			}
			if boundary {
				if p.buf != "" {
					events = append(events, thinkingEvent{kind: thinkingEventText, text: p.buf})
					p.buf = ""
				}
				return events
			}
			target := saturatingSub(len(p.buf), len(thinkingStartTag))
			safe := findCharBoundary(p.buf, target)
			if safe > 0 {
				safeContent := p.buf[:safe]
				if strings.TrimSpace(safeContent) != "" {
					events = append(events, thinkingEvent{kind: thinkingEventText, text: safeContent})
					p.buf = p.buf[safe:]
				}
			}
			return events

		case p.inBlock:
			if p.stripLeadingNL {
				if strings.HasPrefix(p.buf, "\n") {
					p.buf = p.buf[1:]
					p.stripLeadingNL = false
				} else if p.buf != "" {
					p.stripLeadingNL = false
				}
			}
			if end, ok := findRealThinkingEndTag(p.buf); ok {
				events = append(events, p.closeThinking(p.buf[:end], true)...)
				p.buf = p.buf[end+len(thinkingEndSeq):]
				continue
			}
			if boundary {
				if end, ok := findRealThinkingEndTagAtBufferEnd(p.buf); ok {
					events = append(events, p.closeThinking(p.buf[:end], false)...)
					remaining := strings.TrimLeftFunc(p.buf[end+len(thinkingEndTag):], unicode.IsSpace)
					p.buf = remaining
					if remaining != "" {
						events = append(events, thinkingEvent{kind: thinkingEventText, text: remaining})
						p.buf = ""
					}
					continue
				}
				if p.buf != "" {
					events = append(events, thinkingEvent{kind: thinkingEventDelta, text: p.buf})
					p.buf = ""
				}
				events = append(events, thinkingEvent{kind: thinkingEventDelta}, thinkingEvent{kind: thinkingEventStop})
				p.inBlock = false
				p.extracted = true
				return events
			}
			target := saturatingSub(len(p.buf), len(thinkingEndSeq))
			safe := findCharBoundary(p.buf, target)
			if safe > 0 {
				events = append(events, thinkingEvent{kind: thinkingEventDelta, text: p.buf[:safe]})
				p.buf = p.buf[safe:]
			}
			return events

		default:
			if p.buf != "" {
				events = append(events, thinkingEvent{kind: thinkingEventText, text: p.buf})
				p.buf = ""
			}
			return events
		}
	}
}

func (p *thinkingParser) closeThinking(content string, _ bool) []thinkingEvent {
	p.inBlock = false
	p.extracted = true
	events := make([]thinkingEvent, 0, 3)
	if content != "" {
		events = append(events, thinkingEvent{kind: thinkingEventDelta, text: content})
	}
	events = append(events, thinkingEvent{kind: thinkingEventDelta}, thinkingEvent{kind: thinkingEventStop})
	return events
}

func saturatingSub(a, b int) int {
	if a < b {
		return 0
	}
	return a - b
}

func findCharBoundary(s string, target int) int {
	if target >= len(s) {
		return len(s)
	}
	if target <= 0 {
		return 0
	}
	for target > 0 && !utf8.RuneStart(s[target]) {
		target--
	}
	return target
}

func isThinkingQuoteChar(buf string, pos int) bool {
	if pos < 0 || pos >= len(buf) {
		return false
	}
	return thinkingQuoteChars[buf[pos]]
}

func findTaggedPosition(buffer, tag string, accept func(after string) bool) (int, bool) {
	searchStart := 0
	for {
		rel := strings.Index(buffer[searchStart:], tag)
		if rel < 0 {
			return 0, false
		}
		pos := searchStart + rel
		if isThinkingQuoteChar(buffer, pos-1) || isThinkingQuoteChar(buffer, pos+len(tag)) {
			searchStart = pos + 1
			continue
		}
		after := buffer[pos+len(tag):]
		if !accept(after) {
			searchStart = pos + 1
			continue
		}
		return pos, true
	}
}

func findRealThinkingStartTag(buffer string) (int, bool) {
	return findTaggedPosition(buffer, thinkingStartTag, func(string) bool { return true })
}

func findRealThinkingEndTag(buffer string) (int, bool) {
	return findTaggedPosition(buffer, thinkingEndTag, func(after string) bool {
		if len(after) < 2 {
			return false
		}
		return strings.HasPrefix(after, "\n\n")
	})
}

func findRealThinkingEndTagAtBufferEnd(buffer string) (int, bool) {
	return findTaggedPosition(buffer, thinkingEndTag, func(after string) bool {
		return strings.TrimSpace(after) == ""
	})
}

func wrapAssistantThinking(thinking, text string, hasToolUse bool) string {
	thinking = strings.TrimSpace(thinking)
	if thinking != "" {
		if strings.TrimSpace(text) != "" {
			return "<thinking>" + thinking + "</thinking>\n\n" + text
		}
		return "<thinking>" + thinking + "</thinking>"
	}
	if strings.TrimSpace(text) == "" && hasToolUse {
		return " "
	}
	return text
}
