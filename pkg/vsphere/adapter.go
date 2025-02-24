/*
Copyright 2020 VMware, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package vsphere

import (
	"context"
	"fmt"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/jpillora/backoff"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/event"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
	"go.uber.org/zap"
	"knative.dev/eventing/pkg/adapter/v2"
	"knative.dev/pkg/kvstore"
	"knative.dev/pkg/logging"

	kubeclient "knative.dev/pkg/client/injection/kube/client"
)

const (
	// signal unstable event API for converting vSphere events to CE
	eventTypeFormat = "com.vmware.vsphere.%s.v0"
	// extended attribute to filter on vSphere API version/class
	ceVSphereAPIKey     = "vsphereapiversion"
	ceVSphereEventClass = "eventclass"
	// read up to max events per iteration
	maxEventsBatch = 100
)

type envConfig struct {
	adapter.EnvConfig

	// KVConfigMap is the name of the configmap to use as our kvstore.
	KVConfigMap string `envconfig:"VSPHERE_KVSTORE_CONFIGMAP" required:"true"`

	// CheckpointConfig configures the checkpoint behavior of this controller
	CheckpointConfig string `envconfig:"VSPHERE_CHECKPOINT_CONFIG" default:"{}"`

	// PayloadEncoding configures the encoding format for the cloud event payload
	PayloadEncoding string `envconfig:"VSPHERE_PAYLOAD_ENCODING" default:"application/xml"`
}

func NewEnvConfig() adapter.EnvConfigAccessor {
	return &envConfig{}
}

// vAdapter implements the vSphereSource adapter to trigger a Sink.
type vAdapter struct {
	Logger          *zap.SugaredLogger
	Namespace       string
	Source          string
	VClient         *govmomi.Client
	VAPIVersion     string
	CEClient        cloudevents.Client
	KVStore         kvstore.Interface
	CpConfig        CheckpointConfig
	PayloadEncoding string
}

func NewAdapter(ctx context.Context, processed adapter.EnvConfigAccessor, ceClient cloudevents.Client) adapter.Adapter {
	env := processed.(*envConfig)
	logger := logging.FromContext(ctx)

	vClient, err := NewSOAPClient(ctx)
	if err != nil {
		logger.Fatalf("unable to create vSphere client: %v", err)
	}

	source := vClient.URL().Host
	if source == "" {
		logger.Fatal("unable to determine vSphere client source: empty host")
	}

	// setup checkpointing
	store := kvstore.NewConfigMapKVStore(ctx, env.KVConfigMap, env.Namespace, kubeclient.Get(ctx).CoreV1())
	if err = store.Init(ctx); err != nil {
		logger.Fatalf("could not initialize kv store: %v", err)
	}

	cpconf, err := newCheckpointConfig(env.CheckpointConfig)
	if err != nil {
		logger.Fatalf("could not not read checkpoint config: %v", err)
	}

	logger.Infow("configuring checkpointing", zap.String("ReplayWindow", cpconf.MaxAge.String()),
		zap.String("Period", cpconf.Period.String()))

	if cpconf.MaxAge == time.Duration(0) {
		logger.Warn("disabling event replay: maxAge set to 0s")
	}

	return &vAdapter{
		Logger:          logger,
		Namespace:       env.Namespace,
		Source:          source,
		VClient:         vClient,
		VAPIVersion:     vClient.ServiceContent.About.ApiVersion,
		CEClient:        ceClient,
		KVStore:         store,
		CpConfig:        *cpconf,
		PayloadEncoding: env.PayloadEncoding,
	}
}

// Start implements adapter.Adapter
func (a *vAdapter) Start(ctx context.Context) error {
	defer func() {
		// using fresh ctx to avoid canceled error during logout
		_ = a.VClient.Logout(context.Background()) // best effort, ignoring error
	}()

	return a.run(ctx)
}

// run will start reading events from vCenter and send them to the configured
// sink. The internal vCenter event (history) collector will attempt to replay
// events starting at the current vCenter time or retrieved from a previous
// checkpoint with additional validation logic to avoid unbounded event replay.
// A checkpoint will be created periodically to track the position in the
// vCenter event stream. This allows to implement at-least-once semantics.
func (a *vAdapter) run(ctx context.Context) error {
	var cp checkpoint
	if err := a.KVStore.Get(ctx, checkpointKey, &cp); err != nil {
		logging.FromContext(ctx).Warnw("could not retrieve checkpoint configuration", zap.Error(err))
	}
	// begin of event stream defaults to current vCenter time (UTC)
	vcTime, err := methods.GetCurrentTime(ctx, a.VClient)
	if err != nil {
		return fmt.Errorf("get current time from vCenter: %w", err)
	}

	begin := getBeginFromCheckpoint(ctx, *vcTime, cp, a.CpConfig.MaxAge)
	coll, err := newHistoryCollector(ctx, a.VClient.Client, begin)
	if err != nil {
		return fmt.Errorf("create event collector: %w", err)
	}

	return a.readEvents(ctx, coll)
}

// readEvents polls vCenter for new events starting at the configured begin time
// in the provided event history collector. A checkpoint will be periodically
// created and stored in Kubernetes to track successfully processed events
// (ACK-ed by sink).
func (a *vAdapter) readEvents(ctx context.Context, c *event.HistoryCollector) error {
	logger := logging.FromContext(ctx)

	var (
		lastEvent              types.BaseEvent
		lastCheckpointEventKey int32
	)

	bOff := backoff.Backoff{
		Factor: 2,
		Jitter: false,
		Min:    time.Second,
		Max:    5 * time.Second,
	}

	cpTicker := time.NewTicker(a.CpConfig.Period)
	defer cpTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		// checkpoints
		case <-cpTicker.C:
			// avoid unnecessary K8s API calls
			skip := lastEvent == nil || lastCheckpointEventKey == lastEvent.GetEvent().Key
			if !skip {
				var current checkpoint
				if err := a.KVStore.Get(ctx, checkpointKey, &current); err != nil {
					return fmt.Errorf("retrieve current checkpoint: %w", err)
				}

				logger.Debugw("creating checkpoint", zap.Any("checkpoint", current))
				if err := a.KVStore.Save(ctx); err != nil {
					return fmt.Errorf("save checkpoint: %w", err)
				}
				lastCheckpointEventKey = lastEvent.GetEvent().Key
			} else {
				logger.Debug("skipping checkpoint: no new events since last checkpoint")
			}

		// poll vCenter events
		default:
			events, err := c.ReadNextEvents(ctx, maxEventsBatch)
			if err != nil {
				return fmt.Errorf("read events from vcenter: %w", err)
			}

			if len(events) == 0 {
				delay := bOff.Duration()
				logger.Debugw("backing off retrieving events: no new events received", zap.Duration("backoffSeconds", delay))
				time.Sleep(delay)
				continue
			}

			logger.Debugf("got %d events", len(events))

			n, err := a.sendEvents(ctx, events)
			if err != nil {
				// TODO: return and fail instead?
				logger.Errorf("send events: success %d (total %d): %v", n, len(events), err)

				// 	special case: all events failed so skipping checkpoint
				if n == 0 {
					continue
				}
			}

			if n == 0 && err == nil {
				panic("we should never get here")
			}

			// last successfully sent event from batch
			lastEvent = events[n-1]
			cp := checkpoint{
				VCenter:               a.Source,
				LastEventKey:          lastEvent.GetEvent().Key,
				LastEventType:         getEventDetails(lastEvent).Type,
				LastEventKeyTimestamp: lastEvent.GetEvent().CreatedTime,
				CreatedTimestamp:      time.Now().UTC(),
			}
			if err = a.KVStore.Set(ctx, checkpointKey, cp); err != nil {
				return fmt.Errorf("set checkpoint: %w", err)
			}

			bOff.Reset()
		}
	}
}

// sendEvents converts all events to cloud events and sends them to the
// configured sink. It returns the number of successfully processed events,
// which might 0, partial or all events. sendEvents returns when all events are
// processed or on the first error.
func (a *vAdapter) sendEvents(ctx context.Context, baseEvents []types.BaseEvent) (int, error) {
	var success int

	for _, be := range baseEvents {
		ev := cloudevents.NewEvent(cloudevents.VersionV1)
		ev.SetSource(a.Source)

		details := getEventDetails(be)

		// CE envelop
		ev.SetID(fmt.Sprintf("%d", be.GetEvent().Key))
		ev.SetType(fmt.Sprintf(eventTypeFormat, details.Type))
		ev.SetTime(be.GetEvent().CreatedTime)
		ev.SetExtension(ceVSphereEventClass, details.Class)
		ev.SetExtension(ceVSphereAPIKey, a.VAPIVersion)

		if err := ev.SetData(a.PayloadEncoding, be); err != nil {
			return success, fmt.Errorf("set data on event: %w", err)
		}

		// TODO: better partial batch failure handling here?
		logging.FromContext(ctx).Debugw("sending event",
			zap.String("ID", ev.ID()),
			zap.String("type", ev.Type()),
			zap.Any("data", be),
		)

		result := a.CEClient.Send(ctx, ev)
		if !cloudevents.IsACK(result) {
			logging.FromContext(ctx).Errorw("failed to send cloudevent", zap.Error(result))
			return success, result
		}
		success++
	}

	return success, nil
}

// getBeginFromCheckpoint returns the valid begin time to start replaying
// vCenter events. If the checkpoint is empty the current vCenter time (UTC) is
// used. If the last checkpoint event timestamp is larger than maxAge, replay
// will start at maxAge.
func getBeginFromCheckpoint(ctx context.Context, vcTime time.Time, cp checkpoint, maxAge time.Duration) time.Time {
	begin := vcTime
	logger := logging.FromContext(ctx)

	cpTime := cp.LastEventKeyTimestamp
	if !cpTime.IsZero() {
		// valid checkpoint
		logger.Info("found existing checkpoint")
		maxTime := begin.Add(maxAge * -1)
		if maxTime.Unix() > cpTime.Unix() {
			logger.Warnw("potential data loss: last event timestamp in checkpoint is older than configured maximum",
				zap.String("maxHistory", maxAge.String()), zap.String("checkpointTimestamp",
					cp.LastEventKeyTimestamp.String()))
			begin = maxTime
			logger.Warnw("setting begin of event stream", zap.String("beginTimestamp", begin.String()))
		} else {
			begin = cpTime
			logger.Infow("setting begin of event stream", zap.String("beginTimestamp", begin.String()),
				zap.Int32("eventKey", cp.LastEventKey))
		}
	} else {
		// 	empty checkpoint
		logger.Info("no valid checkpoint found")
		logger.Infow("setting begin of event stream", zap.String("beginTimestamp", begin.String()))
	}
	return begin
}
