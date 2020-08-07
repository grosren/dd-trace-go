// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2020 Datadog, Inc.

package tracer

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

type traceWriter interface {
	// add adds a trace to the current payload being constructed by the handler.
	add([]*span)

	// flush causes the handler to send its current payload. The flush can happen asynchronously, but
	// the handler must be ready to accept new traces with add when flush returns.
	flush()

	// stop shuts down the traceWriter, ensuring all payloads are flushed.
	stop()
}

type agentTraceWriter struct {
	config *config

	payload *payload

	// climit limits the number of concurrent outgoing connections
	climit chan struct{}

	// wg waits for all goroutines to exit when stopping.
	wg sync.WaitGroup

	// prioritySampling refers to the tracer's priority sampler.
	prioritySampling *prioritySampler
}

var _ traceWriter = &agentTraceWriter{}

func newAgentTraceWriter(c *config, s *prioritySampler) *agentTraceWriter {
	return &agentTraceWriter{
		config:           c,
		payload:          newPayload(),
		climit:           make(chan struct{}, concurrentConnectionLimit),
		prioritySampling: s,
	}
}

func (h *agentTraceWriter) add(trace []*span) {
	if err := h.payload.push(trace); err != nil {
		h.config.statsd.Incr("datadog.tracer.traces_dropped", []string{"reason:encoding_error"}, 1)
		log.Error("error encoding msgpack: %v", err)
	}
	if h.payload.size() > payloadSizeLimit {
		h.config.statsd.Incr("datadog.tracer.flush_triggered", []string{"reason:size"}, 1)
		h.flush()
	}
}

func (h *agentTraceWriter) stop() {
	h.wg.Wait()
}

// flush will push any currently buffered traces to the server.
func (h *agentTraceWriter) flush() {
	if h.payload.itemCount() == 0 {
		return
	}
	h.wg.Add(1)
	h.climit <- struct{}{}
	go func(p *payload) {
		defer func(start time.Time) {
			<-h.climit
			h.wg.Done()
			h.config.statsd.Timing("datadog.tracer.flush_duration", time.Since(start), nil, 1)
		}(time.Now())
		size, count := p.size(), p.itemCount()
		log.Debug("Sending payload: size: %d traces: %d\n", size, count)
		rc, err := h.config.transport.send(p)
		if err != nil {
			h.config.statsd.Count("datadog.tracer.traces_dropped", int64(count), []string{"reason:send_failed"}, 1)
			log.Error("lost %d traces: %v", count, err)
		} else {
			h.config.statsd.Count("datadog.tracer.flush_bytes", int64(size), nil, 1)
			h.config.statsd.Count("datadog.tracer.flush_traces", int64(count), nil, 1)
			if err := h.prioritySampling.readRatesJSON(rc); err != nil {
				h.config.statsd.Incr("datadog.tracer.decode_error", nil, 1)
			}
		}
	}(h.payload)
	h.payload = newPayload()
}

type hexInt uint64

func (i hexInt) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatUint(uint64(i), 16) + `"`), nil
}

type jsonSpan struct {
	TraceID  hexInt             `json:"trace_id"`
	SpanID   hexInt             `json:"span_id"`
	ParentID hexInt             `json:"parent_id"`
	Name     string             `json:"name"`
	Resource string             `json:"resource"`
	Error    int32              `json:"error"`
	Meta     map[string]string  `json:"meta"`
	Metrics  map[string]float64 `json:"metrics"`
	Start    int64              `json:"start"`
	Duration int64              `json:"duration"`
	Service  string             `json:"service"`
}

type lambdaTraceWriter struct {
	config    *config
	payload   bytes.Buffer
	hasTraces bool
}

var _ traceWriter = &lambdaTraceWriter{}

func newLambdaTraceWriter(c *config) *lambdaTraceWriter {
	w := &lambdaTraceWriter{
		config: c,
	}
	w.resetPayload()
	return w
}

const payloadLimit = 256 * 1024 // log line limit for cloudwatch

func (h *lambdaTraceWriter) resetPayload() {
	h.payload.Reset()
	h.payload.WriteString(`{"traces": [`)
	h.hasTraces = false
}

func (h *lambdaTraceWriter) addSpan(s *span) error {
	js := jsonSpan{
		TraceID:  hexInt(s.TraceID),
		SpanID:   hexInt(s.SpanID),
		ParentID: hexInt(s.ParentID),
		Name:     s.Name,
		Resource: s.Resource,
		Error:    s.Error,
		Meta:     s.Meta,
		Metrics:  s.Metrics,
		Start:    s.Start,
		Duration: s.Duration,
		Service:  s.Service,
	}
	e := json.NewEncoder(&h.payload)
	err := e.Encode(js)
	if err != nil {
		return err
	}
	// cut trailing newline
	h.payload.Truncate(h.payload.Len() - 1)
	return nil
}

type encodingError struct {
	cause      error
	dropReason string
}

func (h *lambdaTraceWriter) addTrace(trace []*span) (int, *encodingError) {
	startLen := h.payload.Len()
	if !h.hasTraces {
		h.payload.WriteByte('[')
	} else {
		h.payload.WriteString(", [")
	}
	written := 0
	for i, s := range trace {
		l := h.payload.Len()
		if i != 0 {
			h.payload.WriteByte(',')
		}
		err := h.addSpan(s)
		if err != nil {
			h.payload.Truncate(startLen)
			return 0, &encodingError{cause: err, dropReason: "encoding_failed"}
		}
		if h.payload.Len() > payloadLimit-2 {
			if i == 0 && !h.hasTraces {
				// This is the first span of the first trace, and it's too big.
				h.payload.Truncate(startLen)
				return 0, &encodingError{cause: errors.New("span too large for payload"), dropReason: "trace_too_large"}
			}
			h.payload.Truncate(l)
			break
		}
		written++
	}
	h.payload.WriteString("]")
	h.hasTraces = true
	return written, nil
}

func (h *lambdaTraceWriter) add(trace []*span) {
	for len(trace) > 0 {
		written, err := h.addTrace(trace)
		if err != nil {
			log.Error("lost a trace: %s", err.cause)
			h.config.statsd.Count("datadog.tracer.traces_dropped", 1, []string{"reason:" + err.dropReason}, 1)
			return
		}
		trace = trace[written:]
		if len(trace) > 0 {
			h.flush()
		}
	}
}

func (h *lambdaTraceWriter) stop() {}

// flush will push any currently buffered traces to the server.
func (h *lambdaTraceWriter) flush() {
	if !h.hasTraces {
		return
	}
	h.payload.WriteString("]}")
	os.Stdout.Write(h.payload.Bytes())
	h.resetPayload()
}
