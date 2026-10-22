package codemode

import (
	"bytes"
	"ccdp/internal/execution"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "--code-probe" {
		_, readErr := os.ReadFile(os.Args[2])
		writeErr := os.WriteFile(os.Args[2]+".write", []byte("probe"), 0600)
		conn, networkErr := net.DialTimeout("tcp", os.Args[3], 100*time.Millisecond)
		if conn != nil {
			conn.Close()
		}
		forkErr := exec.Command("/bin/sh", "-c", "true").Run()
		if readErr == nil || writeErr == nil || networkErr == nil || forkErr == nil || os.Getenv("CCDP_CODE_TEST_SECRET") != "" {
			fmt.Fprintf(os.Stdout, "isolation failed: read=%v write=%v network=%v fork=%v", readErr, writeErr, networkErr, forkErr)
			os.Exit(2)
		}
		fmt.Fprint(os.Stdout, "isolated")
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == WorkerArgument {
		os.Exit(WorkerMain(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}
func runTest(t *testing.T, code string, bindings map[string]Binding) (Summary, error) {
	t.Helper()
	if !Available {
		t.Skip("requires Darwin Seatbelt")
	}
	return Run(context.Background(), Options{Code: code, Timeout: 10 * time.Second, MaxParallel: 2, Bindings: bindings})
}
func TestWorkerAsyncToolComposition(t *testing.T) {
	s, err := runTest(t, `const a = await tools.Read({x:3}); const b = await tools.Read({x:a.data.value+4}); text(b.data);`, map[string]Binding{
		"Read": {ReadOnly: true, Run: func(ctx context.Context, i int, a map[string]any) Result {
			data, _ := json.Marshal(map[string]any{"value": a["x"]})
			return Result{Status: "success", Data: data}
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 2 || len(s.Outputs) != 1 || string(s.Outputs[0]) != `{"value":7}` {
		t.Fatalf("%+v", s)
	}
}

func TestWorkerDataErrorRetainsCompletedExecutionStatus(t *testing.T) {
	calls := 0
	summary, err := runTest(t, `try { await tools.Write({}); } catch(e) { text([e.name,e.status,e.kind]); }`, map[string]Binding{
		"Write": {Run: func(context.Context, int, map[string]any) Result {
			calls++
			return Result{Status: "success", Text: "write completed", DataError: "invalid structured result"}
		}},
	})
	if err != nil || calls != 1 || summary.Failed != 0 || len(summary.Outputs) != 1 || string(summary.Outputs[0]) != `["ToolError","success","invalid_data"]` {
		t.Fatalf("execution/data outcomes confused: calls=%d summary=%+v error=%v", calls, summary, err)
	}
}

func TestWorkerRejectsUnsafeIntegerBeforeCallingTool(t *testing.T) {
	calls := 0
	summary, err := runTest(t, `try { await tools.Read({id:9007199254740993}); } catch(e) { text(e.message); }`, map[string]Binding{
		"Read": {ReadOnly: true, Run: func(context.Context, int, map[string]any) Result {
			calls++
			return Result{Status: "success"}
		}},
	})
	if err != nil || calls != 0 || len(summary.Outputs) != 1 || !strings.Contains(string(summary.Outputs[0]), "exact range") {
		t.Fatalf("unsafe number escaped: calls=%d summary=%+v error=%v", calls, summary, err)
	}
}
func TestWorkerFreshNoHostGlobals(t *testing.T) {
	for range 2 {
		s, err := runTest(t, `text([typeof process,typeof require,typeof fetch,typeof std,typeof os,typeof __send,typeof previous]); globalThis.previous=1;`, nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(s.Outputs[0]) != `["undefined","undefined","undefined","undefined","undefined","undefined","undefined"]` {
			t.Fatal(string(s.Outputs[0]))
		}
	}
}
func TestWorkerFailureCatchAndIncomplete(t *testing.T) {
	b := map[string]Binding{"Fail": {Run: func(ctx context.Context, i int, a map[string]any) Result {
		return Result{CallID: "child", Status: "denied", Text: "blocked"}
	}}}
	s, err := runTest(t, `try {await tools.Fail({});} catch(e) {text([e.name,e.status,e.call_id]);}`, b)
	if err != nil || s.Denied != 1 || string(s.Outputs[0]) != `["ToolError","denied","child"]` {
		t.Fatalf("%+v %v", s, err)
	}
	b["Fail"] = Binding{Run: func(ctx context.Context, i int, a map[string]any) Result {
		<-ctx.Done()
		return Result{Status: "cancelled"}
	}}
	s, err = runTest(t, `tools.Fail({});`, b)
	if err == nil || !strings.Contains(err.Error(), "incomplete") || s.Cancelled != 1 {
		t.Fatalf("%+v %v", s, err)
	}
}
func TestWorkerSchedulerBarriers(t *testing.T) {
	var mu sync.Mutex
	active := 0
	peak := 0
	var order []string
	b := map[string]Binding{}
	for _, name := range []string{"Read", "Write"} {
		b[name] = Binding{ReadOnly: name == "Read", Run: func(ctx context.Context, i int, args map[string]any) Result {
			mu.Lock()
			active++
			peak = max(peak, active)
			if name == "Write" && active != 1 {
				t.Error("write overlaps reads")
			}
			order = append(order, name+" start")
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			mu.Lock()
			active--
			order = append(order, name+" end")
			mu.Unlock()
			return Result{Status: "success"}
		}}
	}
	s, err := runTest(t, `await Promise.all([tools.Read({}),tools.Read({}),tools.Write({}),tools.Read({})]);`, b)
	if err != nil {
		t.Fatal(err)
	}
	if peak != 2 || s.Calls != 4 || !reflect.DeepEqual(order, []string{"Read start", "Read start", "Read end", "Read end", "Write start", "Write end", "Read start", "Read end"}) {
		t.Fatalf("peak %d order %v", peak, order)
	}
}
func TestWorkerLimitsAndInvalidJSON(t *testing.T) {
	for _, code := range []string{`text(1n)`, `text(NaN)`, `const x={};x.x=x;text(x)`, `text({bad:undefined})`, `await new Promise(()=>{})`, `text('x'.repeat(1024*1024))`, `let a=[];while(true){a.push('x'.repeat(1024*1024))}`} {
		t.Run(code[:min(len(code), 25)], func(t *testing.T) {
			_, err := runTest(t, code, nil)
			if err == nil || strings.Contains(err.Error(), "worker exited") {
				t.Fatal("expected failure")
			}
		})
	}
}

func TestWorkerCaptureDoesNotUseModelDisplayBudget(t *testing.T) {
	s, err := runTest(t, `text('x'.repeat(33000));`, nil)
	if err != nil || len(s.Outputs) != 1 || len(s.Outputs[0]) != 33002 {
		t.Fatalf("outputs=%d error=%v", len(s.Outputs), err)
	}
}

func TestWorkerCaptureSharesProgramResultBudget(t *testing.T) {
	b := map[string]Binding{"Large": {Run: func(context.Context, int, map[string]any) Result {
		return Result{Status: "success", Text: strings.Repeat("x", 500000)}
	}}}
	s, err := runTest(t, `for(let i=0;i<16;i++) await tools.Large({}); text('x'.repeat(400000));`, b)
	if err == nil || !strings.Contains(err.Error(), "result budget") || s.Calls != 16 || len(s.Outputs) != 0 {
		t.Fatalf("calls=%d outputs=%d error=%v", s.Calls, len(s.Outputs), err)
	}
}

func TestWorkerStartupFailurePopulatesSummary(t *testing.T) {
	s, err := Run(context.Background(), Options{Code: "text(1)", Executable: filepath.Join(t.TempDir(), "missing-worker")})
	if err == nil || s.Error != err.Error() {
		t.Fatalf("summary=%+v error=%v", s, err)
	}
}

func TestWorkerOSIsolation(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCDP_CODE_TEST_SECRET", "must-not-inherit")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	executable, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := execution.CodeWorker(ctx, executable, "--code-probe", secret, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p.SetIO(nil, &out, &out)
	if err = p.Run(); err != nil || out.String() != "isolated" {
		t.Fatalf("%v %s", err, out.String())
	}
}
func TestWorkerCPULimit(t *testing.T) {
	start := time.Now()
	_, err := runTest(t, `while(true){}`, nil)
	if err == nil || !strings.Contains(err.Error(), "execution limit") || time.Since(start) > 8*time.Second {
		t.Fatalf("CPU timeout: %v %s", err, time.Since(start))
	}
}
func TestApprovalPausesOnlyCellClock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := map[string]Binding{"Approve": {Run: func(ctx context.Context, i int, a map[string]any) Result {
		resume := Pause(ctx)
		time.Sleep(300 * time.Millisecond)
		resume()
		return Result{Status: "success"}
	}}}
	_, err := Run(ctx, Options{Code: `await tools.Approve({});`, Timeout: 150 * time.Millisecond, Bindings: b})
	if err != nil {
		t.Fatal(err)
	}
	b["Approve"] = Binding{Run: func(ctx context.Context, i int, a map[string]any) Result {
		defer Pause(ctx)()
		cancel()
		<-ctx.Done()
		return Result{Status: "cancelled"}
	}}
	_, err = Run(ctx, Options{Code: `await tools.Approve({});`, Timeout: time.Second, Bindings: b})
	if err == nil {
		t.Fatal("parent cancellation ignored while approval paused")
	}
}
func TestFrameBounds(t *testing.T) {
	for _, n := range []uint32{0, MaxFrame + 1, ^uint32(0)} {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, n)
		if _, err := readFrame(&b); err == nil {
			t.Fatal("accepted oversized frame")
		}
	}
}

func TestWorkerLargeDuplexAndCallLimit(t *testing.T) {
	b := map[string]Binding{"Large": {ReadOnly: true, Run: func(ctx context.Context, i int, a map[string]any) Result {
		return Result{Status: "success", Text: strings.Repeat("x", 200000)}
	}}}
	s, err := runTest(t, `const pad='a'.repeat(100000);const r=await Promise.all(Array.from({length:6},()=>tools.Large({pad})));text(r.map(x=>x.text.length));`, b)
	if err != nil || s.Calls != 6 || len(s.Outputs) != 1 {
		t.Fatalf("duplex failed %+v %v", s, err)
	}
	b["Large"] = Binding{Run: func(ctx context.Context, i int, a map[string]any) Result { return Result{Status: "success"} }}
	s, err = runTest(t, `for(let i=0;i<129;i++){try{await tools.Large({})}catch(e){}}`, b)
	if err == nil || !strings.Contains(err.Error(), "limit") || s.Calls != 128 {
		t.Fatalf("caught resource limit %+v %v", s, err)
	}
}
func TestWorkerQueueOverflowCancelsQueuedWithoutStarting(t *testing.T) {
	var started, settled atomic.Int32
	b := map[string]Binding{"Wait": {Run: func(ctx context.Context, i int, a map[string]any) Result {
		started.Add(1)
		<-ctx.Done()
		settled.Add(1)
		return Result{Status: "cancelled"}
	}}}
	s, err := runTest(t, `await Promise.all(Array.from({length:50},()=>tools.Wait({})))`, b)
	if err == nil || !strings.Contains(err.Error(), "queue limit") || s.Cancelled != s.Calls || started.Load() != 1 || settled.Load() != 1 {
		t.Fatalf("%+v %v started=%d settled=%d", s, err, started.Load(), settled.Load())
	}
}
func TestWorkerUnhandledRejectionAndStack(t *testing.T) {
	for _, code := range []string{`Promise.reject(new Error("unhandled fixture"));`, `function f(){return f()}f()`} {
		_, err := runTest(t, code, nil)
		if err == nil || strings.Contains(err.Error(), "worker exited") {
			t.Fatalf("%s: %v", code, err)
		}
	}
}
func TestWorkerCancellationJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	settled := make(chan struct{})
	go func() { <-started; cancel() }()
	s, err := Run(ctx, Options{Code: `await tools.Wait({});`, Bindings: map[string]Binding{"Wait": {Run: func(ctx context.Context, i int, a map[string]any) Result {
		close(started)
		<-ctx.Done()
		close(settled)
		return Result{Status: "cancelled"}
	}}}})
	if err == nil || s.Cancelled != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	select {
	case <-settled:
	default:
		t.Fatal("child not settled")
	}
}
