package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// published is what the host's output stream last said about input.
type published struct {
	binding *Binding
	next    string
	// claimLabel names the harness holding input; empty when the page holds it.
	claimLabel string
	// lastInput is the latest runtime-input-result; admitted is the highest
	// sequence the runtime accepted under admittedBinding.
	lastInput       *Result
	admittedBinding Binding
	admitted        uint64
	connected       bool
	err             string
}

// watcher follows GET outputs/fresh. Connecting never mutates a running
// runtime; each connection starts with a fresh baseline that names the binding.
type watcher struct {
	host *Host

	mu      sync.Mutex
	state   published
	changed chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}

func watch(host *Host) *watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{host: host, changed: make(chan struct{}), cancel: cancel, done: make(chan struct{})}
	go w.loop(ctx)
	return w
}

func (w *watcher) stop() {
	w.cancel()
	<-w.done
}

func (w *watcher) snapshot() published {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *watcher) update(change func(*published)) {
	w.mu.Lock()
	change(&w.state)
	close(w.changed)
	w.changed = make(chan struct{})
	w.mu.Unlock()
}

// wait returns the first state satisfying ready, or the last state seen when
// ctx ends.
func (w *watcher) wait(ctx context.Context, ready func(published) bool) (published, bool) {
	for {
		w.mu.Lock()
		state, changed := w.state, w.changed
		w.mu.Unlock()
		if ready(state) {
			return state, true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return state, false
		}
	}
}

func (w *watcher) loop(ctx context.Context) {
	defer close(w.done)
	for ctx.Err() == nil {
		err := w.follow(ctx)
		w.update(func(p *published) {
			p.connected = false
			if err != nil {
				p.err = err.Error()
			}
		})
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (w *watcher) follow(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, w.host.Origin+runtimePrefix+"/outputs/fresh", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "text/event-stream")
	response, err := w.host.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("output stream: " + response.Status)
	}
	w.update(func(p *published) { p.connected, p.err = true, "" })
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var event string
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				w.dispatch(event, data.String())
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("output stream closed")
}

type output struct {
	Kind              string   `json:"kind"`
	Runtime           *Binding `json:"runtime"`
	NextInputSequence string   `json:"nextInputSequence"`
	InputClaim        string   `json:"inputClaim"`
	Result            *Result  `json:"result"`
}

func (w *watcher) dispatch(event, data string) {
	switch event {
	case "rusty-output-baseline":
		var baseline Result
		if json.Unmarshal([]byte(data), &baseline) == nil && baseline.Binding != nil {
			w.update(func(p *published) { p.binding, p.next = baseline.Binding, baseline.NextInputSequence })
		}
	case "":
		var outputs []output
		if json.Unmarshal([]byte(data), &outputs) != nil {
			return
		}
		for _, o := range outputs {
			switch o.Kind {
			case "binding":
				if o.Runtime != nil {
					w.update(func(p *published) { p.binding, p.next, p.claimLabel = o.Runtime, o.NextInputSequence, o.InputClaim })
				}
			case "runtime-input-result":
				if o.Result != nil {
					result := *o.Result
					w.update(func(p *published) {
						p.lastInput = &result
						if through, err := strconv.ParseUint(result.AcceptedThrough, 10, 64); err == nil && result.Binding != nil {
							if *result.Binding != p.admittedBinding {
								p.admittedBinding, p.admitted = *result.Binding, 0
							}
							p.admitted = max(p.admitted, through)
						}
						if result.NextInputSequence != "" {
							p.next = result.NextInputSequence
						}
					})
				}
			}
		}
	}
}
