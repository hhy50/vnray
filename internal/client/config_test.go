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

func TestConfigReportAndPushOrdering(t *testing.T) {
	var revisions []uint64
	c := &Client{onConfig: func(cfg protocol.PushConfig) { revisions = append(revisions, cfg.Revision) }}
	c.applyConfig(0, protocol.PushConfig{Revision: 1})
	c.applyConfig(2, protocol.PushConfig{Revision: 3})
	c.applyConfig(0, protocol.PushConfig{Revision: 2})
	c.applyConfig(0, protocol.PushConfig{Revision: 4})
	c.applyConfig(100, protocol.PushConfig{Revision: 3})
	c.applyConfig(102, protocol.PushConfig{})
	if fmt.Sprint(revisions) != "[1 3 4]" {
		t.Fatalf("stale report or push applied: %v", revisions)
	}
	var wg sync.WaitGroup
	for revision := uint64(5); revision <= 100; revision++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.applyConfig(0, protocol.PushConfig{Revision: revision}) }()
	}
	wg.Wait()
	if revisions[len(revisions)-1] != 100 {
		t.Fatalf("latest report or push lost: %v", revisions)
	}
}
