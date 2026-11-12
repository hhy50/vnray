package inbound

import (
	"context"
	"time"

	"github.com/hhy50/vnray/internal/concurrency"
	"github.com/hhy50/vnray/internal/plugin"
)

const userQueueTimeout = 5 * time.Second

func acquireUserGate(ctx context.Context, gate *concurrency.Gate) error {
	queueCtx, cancel := context.WithTimeout(ctx, userQueueTimeout)
	defer cancel()
	return gate.AcquireContext(queueCtx)
}

var (
	_ plugin.UserSyncer            = (*V2RayPlugin)(nil)
	_ plugin.UserSyncer            = (*SSPlugin)(nil)
	_ plugin.TLSSetter             = (*V2RayPlugin)(nil)
	_ plugin.TrafficConsumer       = (*V2RayPlugin)(nil)
	_ plugin.TrafficConsumer       = (*SSPlugin)(nil)
	_ plugin.TrafficConsumer       = (*VNRayUserPlugin)(nil)
	_ plugin.VLESSTLSConsumer      = (*V2RayPlugin)(nil)
	_ plugin.SessionConfigConsumer = (*VNRayUserPlugin)(nil)
	_ plugin.DrainGateBinder       = (*V2RayPlugin)(nil)
	_ plugin.DrainGateBinder       = (*SSPlugin)(nil)
	_ plugin.DrainGateBinder       = (*VNRayUserPlugin)(nil)
)
