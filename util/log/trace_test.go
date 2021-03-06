// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Radu Berinde (radu@cockroachlabs.com)

package log

import (
	"fmt"
	"regexp"
	"testing"

	"golang.org/x/net/context"
	"golang.org/x/net/trace"

	basictracer "github.com/opentracing/basictracer-go"
	opentracing "github.com/opentracing/opentracing-go"
)

type events []string

// testingTracer creates a Tracer that appends the events to the given slice.
func testingTracer(ev *events) opentracing.Tracer {
	opts := basictracer.DefaultOptions()
	opts.ShouldSample = func(_ uint64) bool { return true }
	opts.NewSpanEventListener = func() func(basictracer.SpanEvent) {
		return func(e basictracer.SpanEvent) {
			switch t := e.(type) {
			case basictracer.EventCreate:
				*ev = append(*ev, "start")
			case basictracer.EventFinish:
				*ev = append(*ev, "finish")
			case basictracer.EventLog:
				*ev = append(*ev, t.Event)
			}
		}
	}
	opts.DebugAssertUseAfterFinish = true
	// We don't care about the recorder but we need to set it to something.
	opts.Recorder = &basictracer.InMemorySpanRecorder{}
	return basictracer.NewWithOptions(opts)
}

func compareTraces(expected, actual events) bool {
	if len(expected) != len(actual) {
		return false
	}
	for i, ev := range expected {
		if ev == actual[i] {
			continue
		}
		// Try to strip file:line from the actual event.
		groups := regexp.MustCompile(`.*:[0-9]* (.*)`).FindStringSubmatch(actual[i])
		if len(groups) != 2 || groups[0] != actual[i] || groups[1] != ev {
			return false
		}
	}
	return true
}

func TestTrace(t *testing.T) {
	ctx := context.Background()

	// Events to context without a trace should be no-ops.
	Event(ctx, "should-not-show-up")

	var ev events

	tracer := testingTracer(&ev)
	sp := tracer.StartSpan("")
	ctxWithSpan := opentracing.ContextWithSpan(ctx, sp)
	Event(ctxWithSpan, "test1")
	ErrEvent(ctxWithSpan, "testerr")
	VEvent(logging.verbosity.get()+1, ctxWithSpan, "test2")
	Info(ctxWithSpan, "log")

	// Events to parent context should still be no-ops.
	Event(ctx, "should-not-show-up")

	sp.Finish()

	expected := events{"start", "test1", "testerr", "test2", "log", "finish"}
	if !compareTraces(expected, ev) {
		t.Errorf("expected events '%s', got '%s'", expected, fmt.Sprint(ev))
	}
}

func TestTraceWithTags(t *testing.T) {
	ctx := context.Background()
	ctx = WithLogTagInt(ctx, "tag", 1)

	var ev events

	tracer := testingTracer(&ev)
	sp := tracer.StartSpan("")
	ctxWithSpan := opentracing.ContextWithSpan(ctx, sp)
	Event(ctxWithSpan, "test1")
	ErrEvent(ctxWithSpan, "testerr")
	VEvent(logging.verbosity.get()+1, ctxWithSpan, "test2")
	Info(ctxWithSpan, "log")

	sp.Finish()

	expected := events{"start", "[tag=1] test1", "[tag=1] testerr", "[tag=1] test2", "[tag=1] log", "finish"}
	if !compareTraces(expected, ev) {
		t.Errorf("expected events '%s', got '%s'", expected, fmt.Sprint(ev))
	}
}

// testingEventLog is a simple implementation of trace.EventLog.
type testingEventLog struct {
	ev events
}

var _ trace.EventLog = &testingEventLog{}

func (el *testingEventLog) Printf(format string, a ...interface{}) {
	el.ev = append(el.ev, fmt.Sprintf(format, a...))
}

func (el *testingEventLog) Errorf(format string, a ...interface{}) {
	el.ev = append(el.ev, fmt.Sprintf(format+"(err)", a...))
}

func (el *testingEventLog) Finish() {
	el.ev = append(el.ev, "finish")
}

func TestEventLog(t *testing.T) {
	ctx := context.Background()

	// Events to context without a trace should be no-ops.
	Event(ctx, "should-not-show-up")

	el := &testingEventLog{}
	ctxWithEventLog := withEventLogInternal(ctx, el)

	Eventf(ctxWithEventLog, "test%d", 1)
	ErrEvent(ctxWithEventLog, "testerr")
	VEventf(logging.verbosity.get()+1, ctxWithEventLog, "test%d", 2)
	Info(ctxWithEventLog, "log")
	Errorf(ctxWithEventLog, "errlog%d", 1)

	// Events to child contexts without the event log should be no-ops.
	Event(WithNoEventLog(ctxWithEventLog), "should-not-show-up")

	// Events to parent context should still be no-ops.
	Event(ctx, "should-not-show-up")

	FinishEventLog(ctxWithEventLog)

	// Events after Finish should be ignored.
	Errorf(ctxWithEventLog, "should-not-show-up")

	expected := events{"test1", "testerr(err)", "test2", "log", "errlog1(err)", "finish"}
	if !compareTraces(expected, el.ev) {
		t.Errorf("expected events '%s', got '%s'", expected, fmt.Sprint(el.ev))
	}
}

func TestEventLogAndTrace(t *testing.T) {
	ctx := context.Background()

	// Events to context without a trace should be no-ops.
	Event(ctx, "should-not-show-up")

	el := &testingEventLog{}
	ctxWithEventLog := withEventLogInternal(ctx, el)

	Event(ctxWithEventLog, "test1")
	ErrEvent(ctxWithEventLog, "testerr")

	var traceEv events
	tracer := testingTracer(&traceEv)
	sp := tracer.StartSpan("")
	ctxWithBoth := opentracing.ContextWithSpan(ctxWithEventLog, sp)
	// Events should only go to the trace.
	Event(ctxWithBoth, "test3")
	ErrEventf(ctxWithBoth, "%s", "test3err")

	// Events to parent context should still go to the event log.
	Event(ctxWithEventLog, "test5")

	sp.Finish()
	el.Finish()

	trExpected := "[start test3 test3err finish]"
	if evStr := fmt.Sprint(traceEv); evStr != trExpected {
		t.Errorf("expected events '%s', got '%s'", trExpected, evStr)
	}

	elExpected := "[test1 testerr(err) test5 finish]"
	if evStr := fmt.Sprint(el.ev); evStr != elExpected {
		t.Errorf("expected events '%s', got '%s'", elExpected, evStr)
	}
}
