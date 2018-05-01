package internal

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/honeycombio/beeline-go"
	"github.com/honeycombio/beeline-go/timer"
	libhoney "github.com/honeycombio/libhoney-go"
	uuid "github.com/satori/go.uuid"
)

type ResponseWriter struct {
	http.ResponseWriter
	Status int
}

func (h *ResponseWriter) WriteHeader(statusCode int) {
	h.Status = statusCode
	h.ResponseWriter.WriteHeader(statusCode)
}

func AddRequestProps(req *http.Request, ev *libhoney.Event) {
	// identify the type of event
	ev.AddField("meta.type", "http request")
	// Add a variety of details about the HTTP request, such as user agent
	// and method, to any created libhoney event.
	ev.AddField("request.method", req.Method)
	ev.AddField("request.path", req.URL.Path)
	ev.AddField("request.host", req.URL.Host)
	ev.AddField("request.proto", req.Proto)
	ev.AddField("request.content_length", req.ContentLength)
	ev.AddField("request.remote_addr", req.RemoteAddr)
	ev.AddField("request.header.user_agent", req.UserAgent())
	// add any AWS trace headers that might be present
	traceID := parseTraceHeader(req, ev)
	ev.AddField("trace.trace_id", traceID)

	// add a span ID
	id, _ := uuid.NewV4()
	ev.AddField("trace.span_id", id.String())
}

// parseTraceHeader parses tracing headers if they exist
//
// Request-Id: abcd-1234-uuid-v4
// X-Amzn-Trace-Id X-Amzn-Trace-Id: Self=1-67891234-12456789abcdef012345678;Root=1-67891233-abcdef012345678912345678;CalledFrom=app
//
// adds all trace IDs to the passed in event, and returns a trace ID if it finds
// one. Request-ID is preferred over the Amazon trace ID. Will generate a UUID
// if it doesn't find any trace IDs.
//
// NOTE that Amazon actually only means for the latter part of the header to be
// the ID - format is version-timestamp-id. For now though (TODO) we treat it as
// the entire string
func parseTraceHeader(req *http.Request, ev *libhoney.Event) string {
	var traceID string
	awsHeader := req.Header.Get("X-Amzn-Trace-Id")
	if awsHeader != "" {
		// break into key=val pairs on `;` and add each key=val header
		ids := strings.Split(awsHeader, ";")
		for _, id := range ids {
			keyval := strings.Split(id, "=")
			if len(keyval) != 2 {
				// malformed keyval
				continue
			}
			ev.AddField("request.header.aws_trace_id."+keyval[0], keyval[1])
			if keyval[0] == "Root" {
				traceID = keyval[0]
			}
		}
	}
	requestID := req.Header.Get("Request-Id")
	if requestID != "" {
		ev.AddField("request.header.request_id", requestID)
		traceID = requestID
	}
	if traceID == "" {
		id, _ := uuid.NewV4()
		traceID = id.String()
	}
	return traceID
}

// BuildDBEvent tries to bring together most of the things that need to happen
// for an event to wrap a DB call in bot the sql and sqlx packages. It returns a
// function which, when called, dispatches the event that it created. This lets
// it finish a timer around the call automatically.
func BuildDBEvent(ctx context.Context, bld *libhoney.Builder, query string, args ...interface{}) (*libhoney.Event, func(error)) {
	timer := timer.Start()
	ev := bld.NewEvent()
	fn := func(err error) {
		duration := timer.Finish()
		rollup(ctx, ev, duration)
		ev.AddField("duration_ms", duration)
		if err != nil {
			ev.AddField("error", err)
		}
		ev.Send()
	}
	addTraceID(ctx, ev)

	// get the name of the function that called this one. Strip the package and type
	pc, _, _, _ := runtime.Caller(1)
	callName := runtime.FuncForPC(pc).Name()
	callNameChunks := strings.Split(callName, ".")
	ev.AddField("db.call", callNameChunks[len(callNameChunks)-1])

	if query != "" {
		ev.AddField("db.query", query)
	}
	if args != nil {
		ev.AddField("db.query_args", args)
	}
	return ev, fn
}

// rollup takes a context that might contain a parent event, the current event,
// and a duration. It pulls some attributes from the current event in order to
// add the duration to a summed timer in the parent event.
func rollup(ctx context.Context, ev *libhoney.Event, dur float64) {
	parentEv := beeline.ContextEvent(ctx)
	if parentEv == nil {
		return
	}
	// ok now parentEv exists. lets add this to a total duration for the
	// meta.type and the specific db call
	evFields := ev.Fields()
	pvFields := parentEv.Fields()
	metaType, _ := evFields["meta.type"]
	dbCall, _ := evFields["db.call"]
	totalMetaCountKey := fmt.Sprintf("totals.%s_count", metaType)
	totalMetaDurKey := fmt.Sprintf("totals.%s_duration_ms", metaType)
	totalCallCountKey := fmt.Sprintf("totals.%s_%s_count", metaType, dbCall)
	totalCallDurKey := fmt.Sprintf("totals.%s_%s_duration_ms", metaType, dbCall)

	// cast everything appropriately and set to zero if it didn't already exist
	totalTypeCount, _ := pvFields[totalMetaCountKey]
	totalTypeCountVal, ok := totalTypeCount.(int)
	if !ok {
		totalTypeCountVal = 0
	}

	totalTypeDur, _ := pvFields[totalMetaDurKey]
	totalTypeDurVal, ok := totalTypeDur.(float64)
	if !ok {
		totalTypeDurVal = 0
	}
	totalCallCount, _ := pvFields[totalCallCountKey]
	totalCallCountVal, ok := totalCallCount.(int)
	if !ok {
		totalCallCountVal = 0
	}
	totalCallDur, _ := pvFields[totalCallDurKey]
	totalCallDurVal, ok := totalCallDur.(float64)
	if !ok {
		totalCallDurVal = 0
	}

	// ok, set new values with the current stuff added. Note that this is racy
	// and will stomp each other. Not sure what to do about it just yet
	parentEv.AddField(totalMetaCountKey, totalTypeCountVal+1)
	parentEv.AddField(totalMetaDurKey, totalTypeDurVal+dur)
	parentEv.AddField(totalCallCountKey, totalCallCountVal+1)
	parentEv.AddField(totalCallDurKey, totalCallDurVal+dur)
}

func addTraceID(ctx context.Context, ev *libhoney.Event) {
	// get a transaction ID from the request's event, if it's sitting in context
	if parentEv := beeline.ContextEvent(ctx); parentEv != nil {
		if id, ok := parentEv.Fields()["trace.trace_id"]; ok {
			ev.AddField("trace.trace_id", id)
		}
		if id, ok := parentEv.Fields()["trace.span_id"]; ok {
			ev.AddField("trace.parent_id", id)
		}
		id, _ := uuid.NewV4()
		ev.AddField("trace.span_id", id.String())
	}
}