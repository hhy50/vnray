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

// UsersJSON 返回只读用户响应；历史按采样代次和窗口缓存，最新列表不访问历史存储。
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
		since := time.Now().Add(-query.Window)
		ids := make([]uint64, 0, len(p.users.Users))
		for _, u := range p.users.Users {
			ids = append(ids, u.ID)
		}
		rows, err := s.opts.History.Read(ctx, since, p.users.SampledAt, query.Points, ids)
		if err != nil {
			return nil, err
		}
		current := make(map[uint64]User, len(p.users.Users))
		for _, u := range p.users.Users {
			current[u.ID] = u
		}
		response.History = make([]Snapshot, 0, len(rows))
		for _, sample := range rows {
			filtered := sample.Users[:0]
			for _, u := range sample.Users {
				if latest, ok := current[u.ID]; ok {
					u.Name, u.Protocol = latest.Name, latest.Protocol
					filtered = append(filtered, u)
				}
			}
			sample.Users = filtered
			response.History = append(response.History, sample)
		}
		// 去重期间曲线仍延伸到最新采样，但不为此写历史存储。
		history := response.History
		if !p.users.SampledAt.Before(since) && (len(history) == 0 || !history[len(history)-1].SampledAt.Equal(p.users.SampledAt)) {
			history = append(history, p.users.Snapshot)
		}
		if len(history) > query.Points {
			selected := make([]Snapshot, query.Points)
			for i := range selected {
				selected[i] = history[i*(len(history)-1)/(len(selected)-1)]
			}
			history = selected
		}
		response.History = history
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
