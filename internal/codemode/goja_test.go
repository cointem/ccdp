package codemode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGojaResultsSurviveGlobalMutation(t *testing.T) {
	s, err := runTest(t, `
JSON.parse = () => { throw new Error("replaced parser"); };
JSON.stringify = () => "corrupt";
globalThis.Promise = null;
const result = await tools.Read({});
text([Object.getPrototypeOf(result.data) === Object.prototype, result.data.value]);
`, map[string]Binding{"Read": {Concurrent: func(map[string]any) bool { return true }, Run: func(context.Context, int, map[string]any) Result {
		return Result{Status: "success", Data: json.RawMessage(`{"value":"JS-owned"}`)}
	}}})
	if err != nil || len(s.Outputs) != 1 || string(s.Outputs[0]) != `[true,"JS-owned"]` {
		t.Fatalf("JSON bridge depends on modified globals: %+v %v", s, err)
	}
}

func TestGojaMemoryBudget(t *testing.T) {
	ctx := testWorkerContext(t, context.Background())
	start := time.Now()
	_, err := Run(ctx, Options{Code: `const held=[]; while(true) held.push(new Uint8Array(4*1024*1024));`, Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "memory budget") || time.Since(start) > 8*time.Second {
		t.Fatalf("worker did not report memory exhaustion: %v (%s)", err, time.Since(start))
	}
}

func TestGojaMicrotasksCannotResetExecutionBudget(t *testing.T) {
	ctx := testWorkerContext(t, context.Background())
	start := time.Now()
	_, err := Run(ctx, Options{Code: `while(true) await Promise.resolve();`, Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "execution limit") || time.Since(start) > 8*time.Second {
		t.Fatalf("microtask loop escaped budget: %v (%s)", err, time.Since(start))
	}
}

func TestGojaRejectionFormattingIsBounded(t *testing.T) {
	_, err := runTest(t, `throw {toString(){ while(true) {} }};`, nil)
	if err == nil || !strings.Contains(err.Error(), "execution limit") {
		t.Fatalf("unbounded rejection conversion: %v", err)
	}
}

func TestGojaLateRejectionHandler(t *testing.T) {
	s, err := runTest(t, `const p=Promise.reject(new Error("handled")); await Promise.resolve(); p.catch(()=>{}); text("ok");`, nil)
	if err != nil || len(s.Outputs) != 1 || string(s.Outputs[0]) != `"ok"` {
		t.Fatalf("handled rejection reported as failure: %+v %v", s, err)
	}
}

func TestGojaWaitDoesNotSpendExecutionBudget(t *testing.T) {
	s, err := runTest(t, `await tools.Wait({}); text("ok");`, map[string]Binding{"Wait": {Run: func(context.Context, int, map[string]any) Result {
		time.Sleep(workerExecutionBudget + 100*time.Millisecond)
		return Result{Status: "success"}
	}}})
	if err != nil || len(s.Outputs) != 1 || string(s.Outputs[0]) != `"ok"` {
		t.Fatalf("tool wait charged to JavaScript budget: %+v %v", s, err)
	}
}
