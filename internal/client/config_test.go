package client

import (
	"fmt"
	"sync"
	"testing"

	"github.com/hhy50/vnray/internal/protocol"
)

func TestConfigPushOrdering(t *testing.T) {
	var applied []string
	c := &Client{onConfig: func(cfg protocol.PushConfig) { applied = append(applied, cfg.ClientConfig.CustomHosts[0].Domain) }}
	push := func(id uint32) {
		frame, err := protocol.NewPushConfigFrame(protocol.PushConfig{ClientConfig: protocol.ClientConfig{CustomHosts: []protocol.CustomHost{{Domain: fmt.Sprint(id), IP: "127.0.0.1"}}}})
		if err != nil {
			t.Error(err)
			return
		}
		c.handleConfigPush(id, frame)
	}
	push(4)
	push(2)
	if len(applied) != 1 || applied[0] != "4" {
		t.Fatalf("stale config applied: %v", applied)
	}
	var wg sync.WaitGroup
	for id := uint32(6); id <= 100; id += 2 {
		wg.Add(1)
		go func() { defer wg.Done(); push(id) }()
	}
	wg.Wait()
	if applied[len(applied)-1] != "100" {
		t.Fatalf("latest config lost: %v", applied)
	}
}
