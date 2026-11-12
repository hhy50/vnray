package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func (s *Service) latest() (*publication, error) {
	settings := s.settings.Load()
	if !settings.Enabled {
		return nil, ErrDisabled
	}
	p := s.current.Load()
	if p == nil || !p.host.Current() || time.Since(p.host.SampledAt) > 3*settings.Interval {
		return nil, ErrWarming
	}
	return p, nil
}

// HostJSON 返回预编码的只读主机快照；调用方不得修改返回的切片。
func (s *Service) HostJSON() ([]byte, error) {
	p, err := s.latest()
	if err != nil {
		return nil, err
	}
	return p.hostJSON, nil
}

// UsersJSON 返回只读用户响应；历史按采样代次和窗口缓存，最新列表不访问数据库。
func (s *Service) UsersJSON(ctx context.Context, query Query) ([]byte, error) {
	p, err := s.latest()
	if err != nil {
		return nil, err
	}
	if query.Points == 0 {
		return p.usersJSON, nil
	}
	if query.Points < 2 || query.Points > 120 || query.Window <= 0 || query.Window > 7*24*time.Hour {
		return nil, errors.New("invalid monitoring query")
	}
	query.Window = min(query.Window, s.settings.Load().Retention)
	ctx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	select {
	case p.queryGate <- struct{}{}:
		defer func() { <-p.queryGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if cached, ok := p.queries[query]; ok && time.Now().Before(cached.expires) {
		return cached.data, nil
	}
	response := p.users
	if s.opts.History != nil {
		rows, err := s.opts.History.Read(ctx, time.Now().Add(-query.Window), p.users.SampledAt, query.Points)
		if err != nil {
			return nil, err
		}
		current := make(map[uint64]bool, len(p.users.Users))
		for _, u := range p.users.Users {
			current[u.ID] = true
		}
		response.History = make([]Snapshot, 0, len(rows))
		for _, row := range rows {
			var sample Snapshot
			if err := json.Unmarshal(row, &sample); err != nil {
				return nil, err
			}
			filtered := sample.Users[:0]
			for _, u := range sample.Users {
				if current[u.ID] {
					filtered = append(filtered, u)
				}
			}
			sample.Users = filtered
			response.History = append(response.History, sample)
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	var cachedBytes int
	for _, cached := range p.queries {
		cachedBytes += len(cached.data)
	}
	if len(p.queries) < 8 && cachedBytes+len(encoded) <= 8<<20 {
		expires := p.host.SampledAt.Add(s.settings.Load().Interval)
		if len(response.History) > 0 {
			expires = minTime(expires, response.History[0].SampledAt.Add(query.Window))
		}
		p.queries[query] = cachedQuery{data: encoded, expires: expires}
	}
	return encoded, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
