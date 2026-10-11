package proxy

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// sseReader wraps an io.Reader to parse SSE events and trigger hooks
type sseReader struct {
	flow     *Flow
	proxy    *Proxy
	reader   *bufio.Reader
	buffer   []byte      // data accumulated for the current event
	started  bool
	ended    bool
	mu       sync.Mutex
}

// newSSEReader creates a new SSE reader wrapper
func newSSEReader(f *Flow, r io.Reader) io.Reader {
	return &sseReader{
		flow:   f,
		proxy:  f.ConnContext.proxy,
		reader: bufio.NewReader(r),
		buffer: make([]byte, 0, 1024),
	}
}

// Read implements io.Reader, parsing SSE events as data is read
func (sr *sseReader) Read(p []byte) (n int, err error) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if sr.ended {
		return 0, io.EOF
	}

	// Read data from the underlying reader
	n, err = sr.reader.Read(p)
	if n > 0 {
		// Append the new data to the buffer
		sr.buffer = append(sr.buffer, p[:n]...)

		// Parse all complete events in the buffer
		sr.parseEvents()
	}

	if err != nil {
		// Stream ended, trigger SSEEnd
		if sr.started {
			sr.flushEvent() // handle the last possibly incomplete event
			sr.triggerEnd()
		}
		return n, err
	}

	if !sr.started {
		sr.started = true
	}

	return n, nil
}

// parseEvents parses SSE events from the buffer
func (sr *sseReader) parseEvents() {
	// Look for the event separator \n\n in the buffer
	for {
		// Find the end position of the next event
		eventEnd := sr.findEventEnd()
		if eventEnd == -1 {
			// No complete event found, wait for more data
			break
		}

		// Extract one complete event
		eventData := string(sr.buffer[:eventEnd])
		if len(eventData) > 0 {
			// Parse and trigger the event
			sr.parseAndFlushEvent(eventData)
		}

		// Remove the processed data (including \n\n)
		sr.buffer = sr.buffer[eventEnd+2:]
	}
}

// findEventEnd finds the position of \n\n in the buffer
func (sr *sseReader) findEventEnd() int {
	for i := 0; i < len(sr.buffer)-1; i++ {
		if sr.buffer[i] == '\n' && sr.buffer[i+1] == '\n' {
			return i
		}
	}
	return -1
}

// parseAndFlushEvent parses and triggers one event
func (sr *sseReader) parseAndFlushEvent(eventData string) {
	event := sr.parseEvent(eventData)

	// Only process events that have a data field
	if event.Data != "" {
		// Save to the Flow
		sr.flow.SSE.addEvent(event)

		// Trigger the SSEMessage hook (access the latest event via f.SSE.Events)
		for _, addon := range sr.proxy.Addons {
			addon.SSEMessage(sr.flow)
		}
	}
}

// flushEvent parses and triggers the event in the current buffer
func (sr *sseReader) flushEvent() {
	if len(sr.buffer) == 0 {
		return
	}

	// Parse the event
	event := sr.parseEvent(string(sr.buffer))

	// Only process events that have a data field
	if event.Data != "" {
		// Save to the Flow
		sr.flow.SSE.addEvent(event)

		// Trigger the SSEMessage hook (access the latest event via f.SSE.Events)
		for _, addon := range sr.proxy.Addons {
			addon.SSEMessage(sr.flow)
		}
	}

	// Clear the buffer
	sr.buffer = sr.buffer[:0]
}

// parseEvent parses the SSE event text
func (sr *sseReader) parseEvent(text string) *SSEEvent {
	event := &SSEEvent{
		Event: "message",
		Data:  "",
		Raw:   text,
		Time:  time.Now(),
	}

	// Parse line by line
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Skip comment lines
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse the field: value format
		if idx := strings.Index(line, ":"); idx != -1 {
			field := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])

			switch field {
			case "data":
				// Multiple data lines are joined with a newline
				if event.Data != "" {
					event.Data += "\n"
				}
				event.Data += value
			case "event":
				event.Event = value
			case "id":
				event.ID = value
			case "retry":
				if retry, err := strconv.Atoi(value); err == nil {
					event.Retry = retry
				}
			}
		}
	}

	return event
}

// triggerEnd triggers the SSE end hook
func (sr *sseReader) triggerEnd() {
	if sr.ended {
		return
	}

	sr.ended = true

	for _, addon := range sr.proxy.Addons {
		addon.SSEEnd(sr.flow)
	}

	log.Debugf("SSE stream ended for %s", sr.flow.Request.URL.String())
}
