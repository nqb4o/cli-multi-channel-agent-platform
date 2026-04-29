// Replay the full canonical 13-span tree across propagation hops.
//
// Mirrors test_span_tree.py: gateway → orchestrator → runtime, each handing
// over via a bytes-encoded W3C carrier (Redis Streams). Asserts all 13
// canonical spans are emitted, share a trace id, and form the documented
// parent/child topology.
package telemetry

import (
	"context"
	"sort"
	"testing"
)

func runGateway(t *testing.T, carrierToOrchestrator map[string]string) {
	t.Helper()
	ctx, end := StartSpan(context.Background(), SpanGatewayHandleWebhook)
	defer end(nil)
	enqCtx, endE := StartSpan(ctx, SpanGatewayEnqueue)
	defer endE(nil)
	InjectTraceparent(enqCtx, carrierToOrchestrator)
}

func runOrchestrator(t *testing.T, carrierFromGateway map[string]string, carrierToRuntime map[string]string) {
	t.Helper()
	parentCtx := ExtractTraceparent(context.Background(), carrierFromGateway)
	ctx, end := StartSpan(parentCtx, SpanOrchestratorRun)
	defer end(nil)
	_, endR := StartSpan(ctx, SpanOrchestratorResumeSandbox)
	endR(nil)
	execCtx, endX := StartSpan(ctx, SpanOrchestratorExecRuntime)
	defer endX(nil)
	InjectTraceparent(execCtx, carrierToRuntime)
}

func runRuntime(t *testing.T, carrierFromOrch map[string]string) {
	t.Helper()
	parentCtx := ExtractTraceparent(context.Background(), carrierFromOrch)
	ctx, end := StartSpan(parentCtx, SpanRuntimeRunLoop)
	defer end(nil)
	_, e1 := StartSpan(ctx, SpanRuntimeSkillResolve)
	e1(nil)
	_, e2 := StartSpan(ctx, SpanRuntimeMCPBridgeStart)
	e2(nil)
	cliCtx, eCli := StartSpan(ctx, SpanRuntimeCliTurn)
	_, eSpawn := StartSpan(cliCtx, SpanRuntimeCliSpawn)
	eSpawn(nil)
	_, eParse := StartSpan(cliCtx, SpanRuntimeCliParse)
	eParse(nil)
	eCli(nil)
	_, eTool := StartSpan(ctx, SpanRuntimeToolCall)
	eTool(nil)
	_, eResp := StartSpan(ctx, SpanRuntimeRespond)
	eResp(nil)
}

func TestFullCanonicalSpanTree(t *testing.T) {
	initIsolated(t, "span-tree-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	g2o := map[string]string{}
	o2r := map[string]string{}
	runGateway(t, g2o)
	// Hand off via "Redis Streams" — bytes-encoded carrier.
	bytesG2O := map[string][]byte{}
	for k, v := range g2o {
		bytesG2O[k] = []byte(v)
	}
	g2oBack := CoerceBytesCarrier(bytesG2O)
	runOrchestrator(t, g2oBack, o2r)
	bytesO2R := map[string][]byte{}
	for k, v := range o2r {
		bytesO2R[k] = []byte(v)
	}
	runRuntime(t, CoerceBytesCarrier(bytesO2R))

	spans := exp.GetSpans()
	if len(spans) != 13 {
		t.Fatalf("expected 13 spans, got %d", len(spans))
	}
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	sort.Strings(names)
	expected := append([]string{}, CanonicalSpanTree...)
	sort.Strings(expected)
	for i := range names {
		if names[i] != expected[i] {
			t.Fatalf("span name mismatch at %d: got %s want %s\nfull got=%v\nfull want=%v",
				i, names[i], expected[i], names, expected)
		}
	}
}

func TestFullTreeSharesSingleTraceID(t *testing.T) {
	initIsolated(t, "span-tree-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	g2o := map[string]string{}
	o2r := map[string]string{}
	runGateway(t, g2o)
	runOrchestrator(t, g2o, o2r)
	runRuntime(t, o2r)

	traceIDs := map[string]struct{}{}
	for _, s := range exp.GetSpans() {
		traceIDs[s.SpanContext.TraceID().String()] = struct{}{}
	}
	if len(traceIDs) != 1 {
		t.Fatalf("expected 1 trace id, got %v", traceIDs)
	}
}

func TestFullTreeParentChildRelationships(t *testing.T) {
	initIsolated(t, "span-tree-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	g2o := map[string]string{}
	o2r := map[string]string{}
	runGateway(t, g2o)
	runOrchestrator(t, g2o, o2r)
	runRuntime(t, o2r)

	byName := map[string]int{}
	spans := exp.GetSpans()
	for i, s := range spans {
		byName[s.Name] = i
	}

	expectedParent := map[string]string{
		SpanGatewayHandleWebhook:      "",
		SpanGatewayEnqueue:            SpanGatewayHandleWebhook,
		SpanOrchestratorRun:           SpanGatewayEnqueue,
		SpanOrchestratorResumeSandbox: SpanOrchestratorRun,
		SpanOrchestratorExecRuntime:   SpanOrchestratorRun,
		SpanRuntimeRunLoop:            SpanOrchestratorExecRuntime,
		SpanRuntimeSkillResolve:       SpanRuntimeRunLoop,
		SpanRuntimeMCPBridgeStart:     SpanRuntimeRunLoop,
		SpanRuntimeCliTurn:            SpanRuntimeRunLoop,
		SpanRuntimeCliSpawn:           SpanRuntimeCliTurn,
		SpanRuntimeCliParse:           SpanRuntimeCliTurn,
		SpanRuntimeToolCall:           SpanRuntimeRunLoop,
		SpanRuntimeRespond:            SpanRuntimeRunLoop,
	}
	for name, parentName := range expectedParent {
		idx, ok := byName[name]
		if !ok {
			t.Fatalf("span %q missing", name)
		}
		s := spans[idx]
		if parentName == "" {
			if s.Parent.IsValid() {
				t.Fatalf("%s should be root, got parent %s", name, s.Parent.SpanID())
			}
			continue
		}
		pidx := byName[parentName]
		want := spans[pidx].SpanContext.SpanID()
		if s.Parent.SpanID() != want {
			t.Fatalf("%s parent: got %s, want %s (%s)",
				name, s.Parent.SpanID(), want, parentName)
		}
	}
}

func TestPropagationHopsPreserveRemoteParent(t *testing.T) {
	initIsolated(t, "span-tree-tests")
	exp := GetInMemorySpanExporter()
	exp.Reset()

	carrier := map[string]string{}
	ctx, end := StartSpan(context.Background(), SpanGatewayEnqueue)
	gatewaySpanID := CurrentSpan(ctx).SpanContext().SpanID()
	InjectTraceparent(ctx, carrier)
	end(nil)

	parentCtx := ExtractTraceparent(context.Background(), carrier)
	orchCtx, endO := StartSpan(parentCtx, SpanOrchestratorRun)
	_ = orchCtx
	endO(nil)

	for _, s := range exp.GetSpans() {
		if s.Name != SpanOrchestratorRun {
			continue
		}
		if !s.Parent.IsValid() {
			t.Fatal("orch span should have remote parent")
		}
		if s.Parent.SpanID() != gatewaySpanID {
			t.Fatalf("orch span parent: got %s, want %s",
				s.Parent.SpanID(), gatewaySpanID)
		}
	}
}
