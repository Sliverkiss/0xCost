package waf

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
)

func decodeBase64(s string) (string, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("invalid base64")
}

func encodeBase64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func randomBytes(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return string(b)
}

// timerQueue implements the setTimeout/setInterval surface the challenge
// scripts touch. Callbacks run in FIFO order with their delays ignored: the
// token derivation is synchronous arithmetic, and honouring wall-clock delays
// would only slow the solver down.
type timerQueue struct {
	mu    sync.Mutex
	jobs  []func()
	next  int
	limit int
}

func installTimers(vm *goja.Runtime) *timerQueue {
	q := &timerQueue{limit: 1000}
	push := func(fn goja.Value) int {
		call, ok := goja.AssertFunction(fn)
		if !ok {
			return 0
		}
		q.mu.Lock()
		q.next++
		id := q.next
		q.jobs = append(q.jobs, func() {
			_, _ = call(goja.Undefined())
		})
		q.mu.Unlock()
		return id
	}
	_ = vm.Set("setTimeout", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return vm.ToValue(0)
		}
		return vm.ToValue(push(call.Arguments[0]))
	})
	_ = vm.Set("setInterval", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return vm.ToValue(0)
		}
		return vm.ToValue(push(call.Arguments[0]))
	})
	_ = vm.Set("clearTimeout", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
	_ = vm.Set("clearInterval", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
	_ = vm.Set("queueMicrotask", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return goja.Undefined()
		}
		return vm.ToValue(push(call.Arguments[0]))
	})
	return q
}

// drain executes queued callbacks until the queue is empty, the iteration cap
// is hit or the deadline passes (a runaway interval must not hang the solver).
func (q *timerQueue) drain(vm *goja.Runtime, maxIterations int, deadline time.Duration) {
	start := time.Now()
	for i := 0; i < maxIterations; i++ {
		if time.Since(start) > deadline {
			return
		}
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.mu.Unlock()
			return
		}
		job := q.jobs[0]
		q.jobs = q.jobs[1:]
		q.mu.Unlock()
		func() {
			defer func() { _ = recover() }()
			job()
		}()
	}
}
