package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

// Dialer opens one stream to the Docker Engine API. Transports implement it.
type Dialer interface {
	DialContext(ctx context.Context) (net.Conn, error)
}

// engine is a minimal Docker Engine API client covering only the calls this
// adapter makes. Paths are unversioned so the daemon answers with its own
// current API version; daemons have raised their minimum version before.
type engine struct {
	http *http.Client
}

func newEngine(d Dialer) *engine {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx) },
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
	return &engine{http: &http.Client{Transport: tr}}
}

type containerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	Ports   []struct {
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

type containerJSON struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	Image        string `json:"Image"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"`
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
			Log    []struct {
				ExitCode int    `json:"ExitCode"`
				Output   string `json:"Output"`
			} `json:"Log"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image       string            `json:"Image"`
		Labels      map[string]string `json:"Labels"`
		Tty         bool              `json:"Tty"`
		Healthcheck *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"Config"`
}

type imageJSON struct {
	RepoDigests []string `json:"RepoDigests"`
	Created     string   `json:"Created"`
}

type eventMessage struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	TimeNano int64 `json:"timeNano"`
}

type apiError struct {
	Message string `json:"message"`
}

func (e *engine) do(ctx context.Context, method, path string, q url.Values) (*http.Response, error) {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %w", provider.ErrUnreachable, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	var ae apiError
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ae)
	err = fmt.Errorf("docker %s: %s: %s", path, resp.Status, ae.Message)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %w", provider.ErrNotFound, err)
	}
	return nil, err
}

func (e *engine) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := e.do(ctx, http.MethodGet, path, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker %s: decode: %w", path, err)
	}
	return nil
}

// restartContainer stops and starts a container, waiting for it like
// `docker restart` does.
func (e *engine) restartContainer(ctx context.Context, id string) error {
	resp, err := e.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/restart", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (e *engine) listContainers(ctx context.Context) ([]containerSummary, error) {
	var out []containerSummary
	return out, e.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &out)
}

func (e *engine) inspectContainer(ctx context.Context, id string) (containerJSON, error) {
	var out containerJSON
	return out, e.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &out)
}

func (e *engine) inspectImage(ctx context.Context, id string) (imageJSON, error) {
	var out imageJSON
	return out, e.getJSON(ctx, "/images/"+url.PathEscape(id)+"/json", nil, &out)
}

// events streams container events. With until set the daemon returns the
// stored events in range and ends the response.
func (e *engine) events(ctx context.Context, since, until time.Time, filters map[string][]string) (*json.Decoder, io.Closer, error) {
	f := map[string][]string{"type": {"container"}}
	for k, v := range filters {
		f[k] = v
	}
	fj, _ := json.Marshal(f)
	q := url.Values{"filters": {string(fj)}}
	if !since.IsZero() {
		q.Set("since", fmt.Sprint(since.Unix()))
	}
	if !until.IsZero() {
		q.Set("until", fmt.Sprint(until.Unix()))
	}
	resp, err := e.do(ctx, http.MethodGet, "/events", q)
	if err != nil {
		return nil, nil, err
	}
	return json.NewDecoder(resp.Body), resp.Body, nil
}

func (e *engine) logs(ctx context.Context, id string, opts provider.LogOptions, tail int) (io.ReadCloser, error) {
	q := url.Values{
		"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"},
		"tail": {fmt.Sprint(tail)}, "follow": {fmt.Sprint(opts.Follow)},
	}
	if !opts.Since.IsZero() {
		q.Set("since", fmt.Sprint(opts.Since.Unix()))
	}
	resp, err := e.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// logStream decodes the logs endpoint. A container without a TTY multiplexes
// stdout and stderr into frames with an 8-byte header (stream id, then the
// payload size big-endian); a frame need not end on a line boundary, so
// partial lines are kept per stream.
type logStream struct {
	body    io.ReadCloser
	r       *bufio.Reader
	tty     bool
	pending []model.LogLine
	partial map[string]string
	done    bool
}

func newLogStream(body io.ReadCloser, tty bool) *logStream {
	return &logStream{body: body, r: bufio.NewReader(body), tty: tty, partial: map[string]string{}}
}

func (s *logStream) Recv() (model.LogLine, error) {
	for len(s.pending) == 0 {
		if s.done {
			return model.LogLine{}, io.EOF
		}
		if err := s.fill(); err != nil {
			if !errors.Is(err, io.EOF) {
				return model.LogLine{}, err
			}
			s.done = true
			for _, stream := range []string{"stdout", "stderr"} {
				if p := s.partial[stream]; p != "" {
					s.pending = append(s.pending, parseLine(stream, p))
				}
			}
		}
	}
	l := s.pending[0]
	s.pending = s.pending[1:]
	return l, nil
}

func (s *logStream) fill() error {
	if s.tty {
		line, err := s.r.ReadString('\n')
		if line != "" {
			s.add("stdout", line)
		}
		return err
	}
	var hdr [8]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return io.EOF
		}
		return err
	}
	stream := "stdout"
	if hdr[0] == 2 {
		stream = "stderr"
	}
	payload := make([]byte, binary.BigEndian.Uint32(hdr[4:]))
	if _, err := io.ReadFull(s.r, payload); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return io.EOF
		}
		return err
	}
	s.add(stream, string(payload))
	return nil
}

func (s *logStream) add(stream, chunk string) {
	text := s.partial[stream] + chunk
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			break
		}
		s.pending = append(s.pending, parseLine(stream, text[:i]))
		text = text[i+1:]
	}
	s.partial[stream] = text
}

func (s *logStream) Close() error { return s.body.Close() }

// parseLine splits the RFC 3339 timestamp the daemon prefixes with timestamps=1.
func parseLine(stream, raw string) model.LogLine {
	raw = strings.TrimSuffix(raw, "\r")
	l := model.LogLine{Stream: stream, Text: raw}
	if ts, rest, ok := strings.Cut(raw, " "); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			t = t.UTC()
			l.Time, l.Text = &t, rest
		}
	}
	return l
}
