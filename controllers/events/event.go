package events

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/api/trace"
	"go.opentelemetry.io/otel/label"
	tracesdk "go.opentelemetry.io/otel/sdk/export/trace"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// bundle up two object references to track who did what to whom
type actionReference struct {
	actor  objectReference // this is what did something (may be blank if not known)
	object objectReference // this is what it was done to
}

func (p actionReference) String() string {
	return fmt.Sprintf("actor: %s, object: %s", p.actor, p.object) // TODO improve this
}

// Get the object relating to an event, after applying some heuristics
// or a blank struct if this can't be done
func objectFromEvent(ctx context.Context, client client.Client, event *corev1.Event) (runtime.Object, actionReference, error) {
	if event.InvolvedObject.Name == "" {
		return nil, actionReference{}, fmt.Errorf("No involved object")
	}

	objRef := refFromObjRef(event.InvolvedObject)
	ret := actionReference{
		object: objRef,
	}

	switch {
	case event.Source.Component == "deployment-controller" && event.InvolvedObject.Kind == "Deployment":
		// if we have a message like "Scaled down replica set foobar-7ff854f459 to 0"; extract the ReplicaSet name
		marker := "replica set "
		pos := strings.Index(event.Message, marker)
		if pos == -1 {
			break
		}
		pos += len(marker)
		end := strings.IndexByte(event.Message[pos:], ' ')
		if end == -1 {
			break
		}
		ret.actor = ret.object
		ret.object.Kind = "replicaset"
		ret.object.Namespace = lc(ret.object.Namespace)
		ret.object.Name = lc(event.Message[pos : pos+end])
	}

	involved, err := getObject(ctx, client, event.InvolvedObject.APIVersion, objRef.Kind, objRef.Namespace, objRef.Name)
	return involved, ret, err
}

func (r *EventWatcher) eventToSpan(event *corev1.Event, remoteContext trace.SpanContext) *tracesdk.SpanData {
	// resource says which component the span is seen as coming from
	res := r.getResource(eventSource(event))

	attrs := []label.KeyValue{
		label.String("type", event.Type),
		label.String("kind", event.InvolvedObject.Kind),
		label.String("namespace", event.InvolvedObject.Namespace),
		label.String("name", event.InvolvedObject.Name),
		label.String("message", event.Message),
		label.String("eventID", event.Namespace+"/"+event.Name),
	}

	return &tracesdk.SpanData{
		SpanContext: trace.SpanContext{
			TraceID: remoteContext.TraceID,
			SpanID:  eventToSpanID(event),
		},
		ParentSpanID:    remoteContext.SpanID,
		SpanKind:        trace.SpanKindInternal,
		Name:            fmt.Sprintf("%s.%s", event.InvolvedObject.Kind, event.Reason),
		StartTime:       eventTime(event),
		EndTime:         eventTime(event),
		Attributes:      attrs,
		HasRemoteParent: true,
		Resource:        res,
		//InstrumentationLibrary instrumentation.Library
	}
}

// generate a spanID from an event.  The first time this event is issued has a span ID that can be derived from the event UID
func eventToSpanID(event *corev1.Event) trace.SpanID {
	f := fnv.New64a()
	f.Write([]byte(event.UID))
	if event.Count > 0 {
		fmt.Fprint(f, event.Count)
	}
	var h trace.SpanID
	_ = f.Sum(h[:0])
	return h
}

// Some events have just an EventTime; if LastTimestamp is present we prefer that.
func eventTime(event *corev1.Event) time.Time {
	if !event.LastTimestamp.Time.IsZero() {
		return event.LastTimestamp.Time
	}
	return event.EventTime.Time
}

func eventSource(event *corev1.Event) source {
	if event.Source.Component != "" {
		return source{
			name:     event.Source.Component,
			instance: event.Source.Host,
		}
	}
	return source{
		name:     event.ReportingController,
		instance: event.ReportingInstance,
	}
}