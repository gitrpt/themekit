package file

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Shopify/themekit/src/env"
	"github.com/radovskyb/watcher"
)

// Op describes the different types of file operations
type Op int

const (
	// Update is a file op where the file is updated
	Update Op = iota
	// Remove is a file op where the file is removed
	Remove
	filepathSplit = " -> "
)

var (
	// how long until we stop trying to drain events before emitting events
	drainTimeout = time.Second
	// the interval that the watcher polls the filesystem this needs to be less than
	// the drain timeout, otherwise debouncing will not work
	pollInterval = 500 * time.Millisecond
	// amount of time with no events before touching the notify file
	idleTimeout = time.Second
)

// Event decsribes a file change event
type Event struct {
	Op   Op
	Path string
}

// Watcher is the object used to watch files for change and notify on any events,
// these events can then be passed along to kit to be sent to shopify.
type Watcher struct {
	Events chan Event

	fsWatcher  *watcher.Watcher
	filter     Filter
	notify     string
	directory  string
	configPath string
}

// NewWatcher will create a new file change watching for a a given directory defined
// in an environment
func NewWatcher(e *env.Env, configPath string) (*Watcher, error) {
	filter, err := NewFilter(e.Directory, e.IgnoredFiles, e.Ignores)
	if err != nil {
		return nil, err
	}

	fsWatcher := watcher.New()
	fsWatcher.IgnoreHiddenFiles(true)
	fsWatcher.FilterOps(
		watcher.Create,
		watcher.Write,
		watcher.Remove,
		watcher.Rename,
		watcher.Move,
	)

	if configPath != "" {
		if err := fsWatcher.Add(configPath); err != nil {
			return nil, fmt.Errorf("Could not watch config path %s: %s", configPath, err)
		}
	}

	for _, folder := range assetLocations {
		path := filepath.Join(e.Directory, folder)
		if err := fsWatcher.Add(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("Could not watch directory %s: %s", path, err)
		}
	}

	return &Watcher{
		Events:     make(chan Event),
		configPath: configPath,
		directory:  e.Directory,
		filter:     filter,
		fsWatcher:  fsWatcher,
		notify:     e.Notify,
	}, nil
}

// Watch will start the watcher actually receiving file change events and sending
// events to the Events channel
func (w *Watcher) Watch() {
	go w.watchFsEvents()
	go w.fsWatcher.Start(pollInterval)
}

func (w *Watcher) watchFsEvents() {
	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()
	for {
		select {
		case event, ok := <-w.fsWatcher.Event:
			if ok && w.onEvent(event) {
				idleTimer.Reset(idleTimeout)
			}
		case <-idleTimer.C:
			w.onIdle()
		case <-w.fsWatcher.Closed:
			w.Stop()
			return
		}
	}
}

func (w *Watcher) onEvent(event watcher.Event) bool {
	events := map[string]Event{}
	for _, event := range w.translateEvent(event) {
		events[event.Path] = event
	}
	drainTimer := time.NewTimer(drainTimeout)
	defer drainTimer.Stop()
	for {
		select {
		case event, ok := <-w.fsWatcher.Event:
			if !ok {
				continue
			}
			for _, e := range w.translateEvent(event) {
				events[e.Path] = e
			}
			drainTimer.Reset(drainTimeout)
		case <-drainTimer.C:
			for _, e := range events {
				w.Events <- e
			}
			return len(events) > 0
		}
	}
}

func (w *Watcher) translateEvent(event watcher.Event) []Event {
	var events []Event

	if event.IsDir() {
		return events
	}

	oldPath, currentPath := w.parsePath(event.Path)
	if w.configPath != event.Path && w.filter.Match(currentPath) {
		return events
	}

	if isEventType(event.Op, watcher.Rename, watcher.Move) {
		events = append(events, Event{Op: Remove, Path: oldPath}, Event{Op: Update, Path: currentPath})
	} else if isEventType(event.Op, watcher.Remove) {
		events = append(events, Event{Op: Remove, Path: currentPath})
	} else if isEventType(event.Op, watcher.Create, watcher.Write) {
		events = append(events, Event{Op: Update, Path: currentPath})
	}
	return events
}

func (w *Watcher) parsePath(path string) (old, current string) {
	parts := strings.Split(path, filepathSplit)
	for i, path := range parts {
		projectPath := pathToProject(w.directory, path)
		if projectPath == "" {
			projectPath = path
		}
		parts[i] = projectPath
	}
	if len(parts) > 1 {
		return parts[0], parts[1]
	}
	return "", parts[0]
}

func isEventType(currentOp watcher.Op, allowedOps ...watcher.Op) bool {
	for _, op := range allowedOps {
		if currentOp == op {
			return true
		}
	}
	return false
}

// Stop will stop the Watcher from watching it's directories and clean
// up any go routines doing work.
func (w *Watcher) Stop() {
	w.fsWatcher.Close()
	for len(w.Events) > 0 { // drain events
		<-w.Events
	}
}

func (w *Watcher) onIdle() {
	if w.notify == "" {
		return
	}
	os.Create(w.notify)
	os.Chtimes(w.notify, time.Now(), time.Now())
}
