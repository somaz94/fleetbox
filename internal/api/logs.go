package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

const maxTail = 5000

// heartbeatInterval keeps proxies from closing an idle followed stream.
var heartbeatInterval = 15 * time.Second

type recvResult struct {
	line model.LogLine
	err  error
}

// streamLogs serves streamInstanceLogs as server-sent events. Errors before
// the stream starts answer with the Error schema and a status; errors after it
// arrive as an `error` event, since the status line has already been sent.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) {
	ref := model.InstanceRef{
		Service: model.ServiceRef{Target: r.PathValue("target"), Scope: r.PathValue("scope"), Name: r.PathValue("service")},
		ID:      r.PathValue("instance"),
	}
	opts, err := logOptions(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorErrorCodeBadRequest, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, ErrorErrorCodeInternal, "streaming unsupported")
		return
	}
	ctx := r.Context()
	var stream provider.LogStream
	err = s.hide(ctx, ref.Service.Target)
	if err == nil {
		stream, err = s.core.Logs(ctx, ref, opts)
	}
	if err != nil {
		e, status := s.classify(err)
		writeError(w, status, e.Error.Code, e.Error.Message)
		return
	}
	defer stream.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx-style proxies buffer otherwise
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	results := make(chan recvResult)
	go func() {
		for {
			l, err := stream.Recv()
			select {
			case results <- recvResult{line: l, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var heartbeat <-chan time.Time
	if opts.Follow {
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		heartbeat = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat:
			io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		case res := <-results:
			switch {
			case errors.Is(res.err, io.EOF):
				io.WriteString(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			case res.err != nil:
				if ctx.Err() == nil {
					e, _ := s.classify(res.err)
					writeEvent(w, "error", e)
					flusher.Flush()
				}
				return
			}
			writeEvent(w, "line", toLogLine(res.line))
			flusher.Flush()
		}
	}
}

func logOptions(r *http.Request) (provider.LogOptions, error) {
	q := r.URL.Query()
	opts := provider.LogOptions{Container: q.Get("container")}
	if v := q.Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxTail {
			return opts, fmt.Errorf("tail must be an integer from 1 to %d", maxTail)
		}
		opts.Tail = n
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return opts, errors.New("since must be an RFC 3339 date-time")
		}
		opts.Since = t
	}
	if v := q.Get("follow"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return opts, errors.New("follow must be true or false")
		}
		opts.Follow = b
	}
	return opts, nil
}

func toLogLine(l model.LogLine) LogLine {
	out := LogLine{Text: l.Text, Time: l.Time}
	if l.Stream != "" {
		s := LogLineStream(l.Stream)
		out.Stream = &s
	}
	return out
}

// writeEvent writes one SSE event. JSON never contains a raw newline, so one
// data line always suffices.
func writeEvent(w io.Writer, event string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}
