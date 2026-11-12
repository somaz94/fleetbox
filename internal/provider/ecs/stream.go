package ecs

import (
	"context"
	"io"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
)

type fetchFunc func(ctx context.Context, token *string) ([]model.LogLine, *string, error)

// pollStream turns a paged log API into a LogStream. Without follow it serves
// the first page and ends; with follow it keeps polling from the forward token
// and waits between empty pages.
type pollStream struct {
	ctx      context.Context
	cancel   context.CancelFunc
	fetch    fetchFunc
	follow   bool
	interval time.Duration
	buf      []model.LogLine
	token    *string
	fetched  bool
}

func newPollStream(ctx context.Context, fetch fetchFunc, follow bool, interval time.Duration) *pollStream {
	ctx, cancel := context.WithCancel(ctx)
	return &pollStream{ctx: ctx, cancel: cancel, fetch: fetch, follow: follow, interval: interval}
}

func (s *pollStream) Recv() (model.LogLine, error) {
	for {
		if len(s.buf) > 0 {
			l := s.buf[0]
			s.buf = s.buf[1:]
			return l, nil
		}
		if s.fetched && !s.follow {
			return model.LogLine{}, io.EOF
		}
		if s.fetched {
			select {
			case <-s.ctx.Done():
				return model.LogLine{}, s.ctx.Err()
			case <-time.After(s.interval):
			}
		}
		lines, next, err := s.fetch(s.ctx, s.token)
		if err != nil {
			return model.LogLine{}, err
		}
		s.fetched = true
		s.buf, s.token = lines, next
	}
}

func (s *pollStream) Close() error {
	s.cancel()
	return nil
}
